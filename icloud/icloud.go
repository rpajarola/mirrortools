// Package icloud downloads originals from iCloud Photos by talking directly
// to the same private CloudKit web API (setup.icloud.com,
// idmsa.apple.com/appleauth) that icloud.com itself uses.
//
// This is NOT an official API — Apple doesn't publish one for third-party
// bulk library access. The auth flow, endpoints, and CloudKit query/record
// shapes here were derived by reading (not vendoring) two existing Go
// clients that already implement this against real Apple accounts:
// github.com/chyroc/icloudgo (a Go port of the well-known Python
// pyicloud) and github.com/alvaroaleman/icloud-go. Both pull in dependencies
// well beyond what this repo otherwise needs (an embedded database, a CLI
// framework, protobuf, Kubernetes API machinery), so rather than depend on
// either, this reimplements just the auth + list + download subset in
// stdlib Go, matching the gphotos package's zero-dependency design. Endpoint
// URLs, header names, and the CloudKit query/record JSON shapes below are
// confirmed against icloudgo's source; this package's own code (session
// persistence format, filename/date-folder layout, the download index) is
// original.
//
// Unlike gphotos, there's no pre-exported cookies file: iCloud's sign-in
// needs your Apple ID password and, the first time, a 2FA code typed
// interactively. See README.md for the full flow. Session state (cookies +
// Apple's session/trust tokens) is persisted to -session-dir afterward, so
// normal re-runs don't need either again — much like how a browser
// remembers a trusted device.
package icloud

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rpajarola/mirrortools/mirror"
)

func init() {
	mirror.Register(&mirror.Method{
		Name: "icloud",
		Source: "Apple ID email — also the login identifier itself, unlike gphotos' -cookies-owns-the-account " +
			"design, since iCloud has no equivalent of a pre-exported cookies file",
		Describe: "download originals from iCloud Photos via the private CloudKit API icloud.com itself uses",
		SetupFlags: func(fs *flag.FlagSet) mirror.Func {
			passwordFile := fs.String("password-file", "", "path to a file containing the Apple ID password (its "+
				"content is read and trailing whitespace trimmed) — keeps the password out of the command line, "+
				"unlike passing it as a plain flag. $ICLOUD_PASSWORD is used if this isn't set. Only needed if no "+
				"valid saved session exists yet; see -session-dir")
			sessionDir := fs.String("session-dir", "", "directory to persist the iCloud session (cookies, trust "+
				"token) across runs, so sign-in/2FA isn't needed every time (default: a per-account directory under "+
				"the OS user cache dir)")
			album := fs.String("album", AlbumAll, "iCloud Photos album to mirror")
			livePhotoVideo := fs.Bool("live-photo-video", true, "also download a Live Photo's paired video")
			after := fs.String("after", "", "only download items taken on/after this date, YYYY-MM-DD (default: no lower bound)")
			before := fs.String("before", "", "only download items taken on/before this date, YYYY-MM-DD (default: now)")
			last := fs.String("last", "", "only download items taken in the last duration, e.g. 30d, 2w, 6m, 1y "+
				"(an alternative to -after, relative to -before or now; mutually exclusive with -after)")
			return func(ctx context.Context, source, destDir string) error {
				pw := os.Getenv("ICLOUD_PASSWORD")
				if *passwordFile != "" {
					data, err := os.ReadFile(*passwordFile)
					if err != nil {
						return fmt.Errorf("reading -password-file: %w", err)
					}
					pw = strings.TrimSpace(string(data))
				}
				dir := *sessionDir
				if dir == "" {
					d, err := defaultSessionDir(source)
					if err != nil {
						return fmt.Errorf("finding a default -session-dir: %w", err)
					}
					dir = d
				}

				effectiveAfter := *after
				if *last != "" {
					if *after != "" {
						return fmt.Errorf("-last and -after are mutually exclusive")
					}
					ref := time.Now()
					if *before != "" {
						t, err := time.Parse("2006-01-02", *before)
						if err != nil {
							return fmt.Errorf("bad -before date: %w", err)
						}
						ref = t
					}
					afterT, err := parseLast(*last, ref)
					if err != nil {
						return err
					}
					effectiveAfter = afterT.Format("2006-01-02")
				}

				return Mirror(ctx, source, destDir, pw, dir, *album, *livePhotoVideo, effectiveAfter, *before)
			}
		},
	})
}

// defaultSessionDir returns a stable, per-account directory to persist the
// session in when -session-dir isn't given, so a plain `mirror icloud
// someone@example.com ./photos` re-run picks the same one back up.
func defaultSessionDir(appleID string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "mirrortools", "icloud", sanitizeForPath(appleID)), nil
}

var unsafePathChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitizeForPath(s string) string {
	s = unsafePathChars.ReplaceAllString(s, "_")
	if s == "" {
		return "_"
	}
	return s
}

// ---- -last parsing (identical algorithm to gphotos' parseLast) ----

var lastRe = regexp.MustCompile(`^(\d+)([dwmy])$`)

func parseLast(last string, ref time.Time) (time.Time, error) {
	m := lastRe.FindStringSubmatch(last)
	if m == nil {
		return time.Time{}, fmt.Errorf("invalid -last %q: want a number followed by d, w, m, or y (e.g. 30d, 2w, 6m, 1y)", last)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid -last %q: %w", last, err)
	}
	switch m[2] {
	case "d":
		return ref.AddDate(0, 0, -n), nil
	case "w":
		return ref.AddDate(0, 0, -7*n), nil
	case "m":
		return ref.AddDate(0, -n, 0), nil
	case "y":
		return ref.AddDate(-n, 0, 0), nil
	default:
		return time.Time{}, fmt.Errorf("invalid -last %q: unknown unit %q", last, m[2])
	}
}

// ---- albums ----

// Well-known iCloud Photos smart-album names, confirmed against icloudgo's
// icloudPhotoFolderMeta. Custom user albums are looked up by name too (see
// getAlbumQuery), but these are the ones every library has.
const (
	AlbumAll             = "All Photos"
	AlbumFavorites       = "Favorites"
	AlbumVideos          = "Videos"
	AlbumSelfies         = "Selfies"
	AlbumPanoramas       = "Panoramas"
	AlbumScreenshots     = "Screenshots"
	AlbumHidden          = "Hidden"
	AlbumRecentlyDeleted = "Recently Deleted"
)

// albumQuery is the CloudKit recordType/filter combination for a given
// smart album. Confirmed against icloudgo's icloudPhotoFolderMeta.
type albumQuery struct {
	recordType string
	filterBy   []queryFilter
}

var smartAlbumQueries = map[string]albumQuery{
	AlbumAll: {recordType: "CPLAssetAndMasterByAddedDate"},
	AlbumFavorites: {
		recordType: "CPLAssetAndMasterInSmartAlbumByAssetDate",
		filterBy:   []queryFilter{{FieldName: "smartAlbum", Comparator: "EQUALS", FieldValue: queryValue{Type: "STRING", Value: "FAVORITE"}}},
	},
	AlbumVideos: {
		recordType: "CPLAssetAndMasterInSmartAlbumByAssetDate",
		filterBy:   []queryFilter{{FieldName: "smartAlbum", Comparator: "EQUALS", FieldValue: queryValue{Type: "STRING", Value: "VIDEO"}}},
	},
	AlbumSelfies: {
		recordType: "CPLAssetAndMasterInSmartAlbumByAssetDate",
		filterBy:   []queryFilter{{FieldName: "smartAlbum", Comparator: "EQUALS", FieldValue: queryValue{Type: "STRING", Value: "SELFIES"}}},
	},
	AlbumPanoramas: {
		recordType: "CPLAssetAndMasterInSmartAlbumByAssetDate",
		filterBy:   []queryFilter{{FieldName: "smartAlbum", Comparator: "EQUALS", FieldValue: queryValue{Type: "STRING", Value: "PANORAMA"}}},
	},
	AlbumScreenshots: {
		recordType: "CPLAssetAndMasterInSmartAlbumByAssetDate",
		filterBy:   []queryFilter{{FieldName: "smartAlbum", Comparator: "EQUALS", FieldValue: queryValue{Type: "STRING", Value: "SCREENSHOT"}}},
	},
	AlbumHidden:          {recordType: "CPLAssetAndMasterHiddenByAssetDate"},
	AlbumRecentlyDeleted: {recordType: "CPLAssetAndMasterDeletedByExpungedDate"},
}

type queryFilter struct {
	FieldName  string     `json:"fieldName"`
	Comparator string     `json:"comparator"`
	FieldValue queryValue `json:"fieldValue"`
}

type queryValue struct {
	Type  string `json:"type"`
	Value any    `json:"value"`
}

// ---- session state: Apple's custom auth headers plus an exportable jar ----
//
// Sign-in relies on two things across requests: a handful of custom
// X-Apple-* response headers that have to be echoed back as request headers
// on later calls (scnt, X-Apple-ID-Session-Id, ...; confirmed against
// icloudgo's contextHeader/getAuthHeaders), and ordinary Set-Cookie cookies
// on idmsa.apple.com, setup.icloud.com, and the account's own CloudKit shard
// host. net/http/cookiejar.Jar has no export/import support, so
// exportableJar wraps one and separately records every cookie it's ever
// been given, keyed by host and name (overwriting on repeat sets, so this
// doesn't grow unbounded across a long session) — that record is what
// actually gets persisted to -session-dir, which is the whole reason a
// second run doesn't need to sign in or 2FA again.
type sessionState struct {
	ClientID       string                            `json:"client_id"`
	SessionToken   string                            `json:"session_token"`
	Scnt           string                            `json:"scnt"`
	SessionID      string                            `json:"session_id"`
	AccountCountry string                            `json:"account_country"`
	TrustToken     string                            `json:"trust_token"`
	Cookies        map[string]map[string]savedCookie `json:"cookies"`
}

type savedCookie struct {
	Name, Value, Path, Domain string
	Expires                   time.Time
}

type exportableJar struct {
	*cookiejar.Jar
	mu     sync.Mutex
	byHost map[string]map[string]savedCookie
}

func newExportableJar() *exportableJar {
	j, _ := cookiejar.New(nil)
	return &exportableJar{Jar: j, byHost: map[string]map[string]savedCookie{}}
}

func (j *exportableJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.Jar.SetCookies(u, cookies)
	if len(cookies) == 0 {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.byHost[u.Host] == nil {
		j.byHost[u.Host] = map[string]savedCookie{}
	}
	for _, c := range cookies {
		j.byHost[u.Host][c.Name] = savedCookie{Name: c.Name, Value: c.Value, Path: c.Path, Domain: c.Domain, Expires: c.Expires}
	}
}

func (j *exportableJar) export() map[string]map[string]savedCookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make(map[string]map[string]savedCookie, len(j.byHost))
	for host, cookies := range j.byHost {
		m := make(map[string]savedCookie, len(cookies))
		for name, c := range cookies {
			m[name] = c
		}
		out[host] = m
	}
	return out
}

func (j *exportableJar) restore(byHost map[string]map[string]savedCookie) {
	for host, cookies := range byHost {
		var httpCookies []*http.Cookie
		for _, c := range cookies {
			httpCookies = append(httpCookies, &http.Cookie{Name: c.Name, Value: c.Value, Path: c.Path, Domain: c.Domain, Expires: c.Expires})
		}
		j.Jar.SetCookies(&url.URL{Scheme: "https", Host: host}, httpCookies)
	}
}

func loadSession(path string) (*sessionState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &sessionState{}, nil
	}
	if err != nil {
		return nil, err
	}
	s := &sessionState{}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return s, nil
}

func (c *Client) saveSession() error {
	c.session.Cookies = c.jar.export()
	data, err := json.MarshalIndent(c.session, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.sessionPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.sessionPath)
}

func randomClientID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "auth-" + hex.EncodeToString(b), nil
}

// ---- HTTP client ----

const (
	// oauthClientID is icloud.com's own web client id, embedded in its
	// public JS — not a secret, and the same for every user.
	oauthClientID = "d39ba9916b7251055b22c7f910e2ea796ee65e98b2ddecea8f5dde8d9d1a815d"
	userAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/109.0.0.0 Safari/537.36"
)

// Client holds one iCloud session. Use newClient, then authenticate.
type Client struct {
	appleID       string
	setupEndpoint string
	homeEndpoint  string
	authEndpoint  string

	session     *sessionState
	validated   *validateData
	jar         *exportableJar
	httpClient  *http.Client
	sessionPath string
}

func newClient(appleID, sessionDir string) (*Client, error) {
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating session dir: %w", err)
	}
	sessionPath := filepath.Join(sessionDir, "session.json")
	session, err := loadSession(sessionPath)
	if err != nil {
		return nil, err
	}
	if session.ClientID == "" {
		id, err := randomClientID()
		if err != nil {
			return nil, err
		}
		session.ClientID = id
	}

	jar := newExportableJar()
	jar.restore(session.Cookies)

	// http.Client.Timeout caps the entire request, including reading the
	// response body — fine for the small JSON API calls this client mostly
	// handles, but it also downloads originals and Live Photo videos
	// (downloadURL), and a large file on a slow connection can easily take
	// longer than a flat 5 minutes to finish streaming even though it's
	// making perfectly good progress. Transport.ResponseHeaderTimeout keeps
	// the fail-fast behavior that actually matters — the server never even
	// starting to respond — without capping how long a large, in-progress
	// download is allowed to take.
	transport := http.DefaultTransport
	if t, ok := transport.(*http.Transport); ok {
		t = t.Clone()
		t.ResponseHeaderTimeout = 5 * time.Minute
		transport = t
	}

	return &Client{
		appleID:       appleID,
		setupEndpoint: "https://setup.icloud.com/setup/ws/1",
		homeEndpoint:  "https://www.icloud.com",
		authEndpoint:  "https://idmsa.apple.com/appleauth/auth",
		session:       session,
		jar:           jar,
		httpClient:    &http.Client{Jar: jar, Transport: transport},
		sessionPath:   sessionPath,
	}, nil
}

func (c *Client) captureSessionHeaders(h http.Header) {
	if v := h.Get("X-Apple-ID-Account-Country"); v != "" {
		c.session.AccountCountry = v
	}
	if v := h.Get("X-Apple-ID-Session-Id"); v != "" {
		c.session.SessionID = v
	}
	if v := h.Get("X-Apple-Session-Token"); v != "" {
		c.session.SessionToken = v
	}
	if v := h.Get("X-Apple-TwoSV-Trust-Token"); v != "" {
		c.session.TrustToken = v
	}
	if v := h.Get("scnt"); v != "" {
		c.session.Scnt = v
	}
}

func (c *Client) authHeaders(extra map[string]string) map[string]string {
	h := map[string]string{
		"Accept":                           "*/*",
		"Content-Type":                     "application/json",
		"X-Apple-OAuth-Client-Id":          oauthClientID,
		"X-Apple-OAuth-Client-Type":        "firstPartyAuth",
		"X-Apple-OAuth-Redirect-URI":       "https://www.icloud.com",
		"X-Apple-OAuth-Require-Grant-Code": "true",
		"X-Apple-OAuth-Response-Mode":      "web_message",
		"X-Apple-OAuth-Response-Type":      "code",
		"X-Apple-OAuth-State":              c.session.ClientID,
		"X-Apple-Widget-Key":               oauthClientID,
		"Origin":                           c.homeEndpoint,
		"Referer":                          c.homeEndpoint + "/",
		"User-Agent":                       userAgent,
	}
	if c.session.Scnt != "" {
		h["scnt"] = c.session.Scnt
	}
	if c.session.SessionID != "" {
		h["X-Apple-ID-Session-Id"] = c.session.SessionID
	}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func (c *Client) commonHeaders(extra map[string]string) map[string]string {
	h := map[string]string{
		"Origin":     c.homeEndpoint,
		"Referer":    c.homeEndpoint + "/",
		"User-Agent": userAgent,
	}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// ---- request/response plumbing ----

type apiRequest struct {
	method       string
	url          string
	query        url.Values
	headers      map[string]string
	body         any
	expectStatus int // 0 = don't check
}

// do sends req, capturing any of Apple's session-carrying response headers
// (see captureSessionHeaders) regardless of outcome, and returns the raw
// response body.
func (c *Client) do(ctx context.Context, req *apiRequest) ([]byte, *http.Response, error) {
	var bodyReader io.Reader
	if req.body != nil {
		b, err := json.Marshal(req.body)
		if err != nil {
			return nil, nil, err
		}
		bodyReader = strings.NewReader(string(b))
	}

	u := req.url
	if len(req.query) > 0 {
		u += "?" + req.query.Encode()
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.method, u, bodyReader)
	if err != nil {
		return nil, nil, err
	}
	if req.body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	for k, v := range req.headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", req.method, req.url, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp, fmt.Errorf("%s %s: reading response: %w", req.method, req.url, err)
	}
	c.captureSessionHeaders(resp.Header)

	if req.expectStatus != 0 && resp.StatusCode != req.expectStatus {
		if apiErr := parseAPIError(respBody); apiErr != nil {
			return respBody, resp, fmt.Errorf("%s %s: expected status %d, got %d: %w", req.method, req.url, req.expectStatus, resp.StatusCode, apiErr)
		}
		return respBody, resp, fmt.Errorf("%s %s: expected status %d, got %d: %s", req.method, req.url, req.expectStatus, resp.StatusCode, snippet(respBody, 300))
	}
	if req.expectStatus == 0 && resp.StatusCode >= 400 {
		if apiErr := parseAPIError(respBody); apiErr != nil {
			return respBody, resp, fmt.Errorf("%s %s: http %d: %w", req.method, req.url, resp.StatusCode, apiErr)
		}
		return respBody, resp, fmt.Errorf("%s %s: http %d: %s", req.method, req.url, resp.StatusCode, snippet(respBody, 300))
	}
	return respBody, resp, nil
}

func snippet(body []byte, n int) string {
	s := strings.ReplaceAll(string(body), "\n", "\\n")
	if len(s) > n {
		s = s[:n] + "...(truncated)"
	}
	return s
}

// apiError is an error Apple's auth or CloudKit APIs reported in their own
// JSON body, as opposed to a bare non-2xx with no parseable detail.
type apiError struct {
	Code    string
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// errCodeWrongVerificationCode is the code Apple's API uses for "that 2FA
// code was wrong" — confirmed against icloudgo's ErrValidateCodeWrong.
const errCodeWrongVerificationCode = "-21669"

// parseAPIError recognizes the two error body shapes actually seen from
// these endpoints (confirmed against icloudgo's mayErr, trimmed to the ones
// this package's own request set can trigger):
//
//	{"service_errors":[{"code":"...","title":"...","message":"..."}],"hasError":true}
//	{"reason":"...","error":"..."}
//
// Returns nil if body doesn't parse as either shape or carries no error —
// callers fall back to a generic HTTP-status error in that case.
func parseAPIError(body []byte) error {
	var e1 struct {
		ServiceErrors []struct {
			Code, Title, Message string
		} `json:"service_errors"`
		HasError bool `json:"hasError"`
	}
	if err := json.Unmarshal(body, &e1); err == nil {
		for _, se := range e1.ServiceErrors {
			if se.Code != "" && se.Code != "0" {
				msg := strings.Trim(se.Title, ".")
				if m2 := strings.Trim(se.Message, "."); !strings.EqualFold(m2, msg) && m2 != "" {
					msg += ", " + m2
				}
				return &apiError{Code: se.Code, Message: msg}
			}
		}
	}

	var e2 struct {
		Reason string `json:"reason"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(body, &e2); err == nil && e2.Error != "" {
		msg := e2.Error
		if e2.Reason != "" {
			msg += " " + e2.Reason
		}
		return &apiError{Code: "error", Message: msg}
	}

	return nil
}

// ---- auth flow ----
//
// Confirmed step-by-step against icloudgo's internal/auth*.go. No SRP or
// other client-side crypto is involved — sign-in is a plain HTTPS POST of
// the password, protected only by TLS, same as any ordinary login form.
//
//  1. signIn: POST .../signin with the password (plus any previously
//     granted trust token, so a still-trusted device can skip 2FA). Ends
//     with a call to authWithToken to fetch account/session details.
//  2. If the account needs 2FA (HsaVersion==2 && (HsaChallengeRequired ||
//     !HsaTrustedBrowser)), prompt for the 6-digit code sent to a trusted
//     device, POST it to .../verify/trusteddevice/securitycode, then GET
//     .../2sv/trust to mark this "device" (this session) as trusted going
//     forward — the response to that carries the long-lived trust token
//     saved in sessionState, which is what lets a future run skip 2FA
//     entirely.
//  3. On a future run, validateToken is tried first: if the saved session
//     token is still accepted, authenticate returns immediately without
//     touching the password or prompting for anything.

// validateData is the subset of setup.icloud.com's accountLogin/validate
// response this package actually uses.
type validateData struct {
	DsInfo *struct {
		HsaVersion int `json:"hsaVersion"`
	} `json:"dsInfo"`
	HsaTrustedBrowser    bool                  `json:"hsaTrustedBrowser"`
	HsaChallengeRequired bool                  `json:"hsaChallengeRequired"`
	Webservices          map[string]webService `json:"webservices"`
}

type webService struct {
	URL string `json:"url"`
}

func (c *Client) signIn(ctx context.Context, password string) error {
	trustTokens := []string{}
	if c.session.TrustToken != "" {
		trustTokens = []string{c.session.TrustToken}
	}
	_, _, err := c.do(ctx, &apiRequest{
		method:  http.MethodPost,
		url:     c.authEndpoint + "/signin",
		query:   url.Values{"isRememberMeEnabled": {"true"}},
		headers: c.authHeaders(nil),
		body: map[string]any{
			"accountName": c.appleID,
			"password":    password,
			"rememberMe":  true,
			"trustTokens": trustTokens,
		},
		expectStatus: http.StatusOK,
	})
	if err != nil {
		return fmt.Errorf("sign in: %w", err)
	}
	return c.authWithToken(ctx)
}

func (c *Client) authWithToken(ctx context.Context) error {
	body, _, err := c.do(ctx, &apiRequest{
		method:  http.MethodPost,
		url:     c.setupEndpoint + "/accountLogin",
		headers: c.commonHeaders(nil),
		body: map[string]any{
			"accountCountryCode": c.session.AccountCountry,
			"dsWebAuthToken":     c.session.SessionToken,
			"extended_login":     true,
			"trustToken":         c.session.TrustToken,
		},
	})
	if err != nil {
		return fmt.Errorf("account login: %w", err)
	}
	data := &validateData{}
	if err := json.Unmarshal(body, data); err != nil {
		return fmt.Errorf("account login: parsing response: %w", err)
	}
	c.validated = data
	return nil
}

func (c *Client) validateToken(ctx context.Context) error {
	body, _, err := c.do(ctx, &apiRequest{
		method:  http.MethodPost,
		url:     c.setupEndpoint + "/validate",
		headers: c.commonHeaders(nil),
	})
	if err != nil {
		return err
	}
	data := &validateData{}
	if err := json.Unmarshal(body, data); err != nil {
		return fmt.Errorf("validate: parsing response: %w", err)
	}
	c.validated = data
	return nil
}

func (c *Client) requires2FA() bool {
	return c.validated != nil && c.validated.DsInfo != nil && c.validated.DsInfo.HsaVersion == 2 &&
		(c.validated.HsaChallengeRequired || !c.validated.HsaTrustedBrowser)
}

func (c *Client) validate2FACode(ctx context.Context, code string) error {
	_, _, err := c.do(ctx, &apiRequest{
		method:       http.MethodPost,
		url:          c.authEndpoint + "/verify/trusteddevice/securitycode",
		headers:      c.authHeaders(map[string]string{"Accept": "application/json"}),
		body:         map[string]any{"securityCode": map[string]string{"code": code}},
		expectStatus: http.StatusNoContent,
	})
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.Code == errCodeWrongVerificationCode {
			return fmt.Errorf("incorrect verification code")
		}
		return fmt.Errorf("submitting 2FA code: %w", err)
	}
	return nil
}

func (c *Client) trustSession(ctx context.Context) error {
	_, _, err := c.do(ctx, &apiRequest{
		method:       http.MethodGet,
		url:          c.authEndpoint + "/2sv/trust",
		headers:      c.authHeaders(nil),
		expectStatus: http.StatusNoContent,
	})
	if err != nil {
		return fmt.Errorf("trusting session: %w", err)
	}
	return c.authWithToken(ctx) // refreshes c.validated with hsaTrustedBrowser now true
}

// verify2FA prompts for and submits a 2FA code if the account needs one, a
// no-op otherwise. codePrompt is called at most once.
func (c *Client) verify2FA(ctx context.Context, codePrompt func() (string, error)) error {
	if !c.requires2FA() {
		return nil
	}
	code, err := codePrompt()
	if err != nil {
		return fmt.Errorf("get 2FA code: %w", err)
	}
	if err := c.validate2FACode(ctx, code); err != nil {
		return err
	}
	if !c.validated.HsaTrustedBrowser {
		if err := c.trustSession(ctx); err != nil {
			return err
		}
	}
	return nil
}

// authenticate establishes a working session: reuses the saved one if
// still valid (no password or prompting needed at all), otherwise signs in
// fresh and, if required, runs the 2FA prompt.
func (c *Client) authenticate(ctx context.Context, password string, codePrompt func() (string, error)) error {
	if c.session.SessionToken != "" {
		if err := c.validateToken(ctx); err == nil {
			log.Printf("icloud: reusing saved session for %s", c.appleID)
			return nil
		}
		log.Printf("icloud: saved session no longer valid, signing in fresh")
	}
	if password == "" {
		return fmt.Errorf("no valid saved session for %s, and no password available "+
			"(set -password-file or $ICLOUD_PASSWORD)", c.appleID)
	}
	if err := c.signIn(ctx, password); err != nil {
		return err
	}
	return c.verify2FA(ctx, codePrompt)
}

// promptStdin writes prompt to stderr and reads one line from stdin,
// trimmed. Used for the 2FA code — plain, unhidden input is fine for a
// single-use, short-lived code, unlike a password.
func promptStdin(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// ---- photo listing (CloudKit records/query) ----
//
// This is Apple's CloudKit Web Services API, the same one icloud.com's own
// JS uses to render the photo grid — confirmed against icloudgo's
// photo_album_photos.go. Listing is offset ("startRank") based, ascending
// by added-date for "All Photos"; each page mixes two record types that
// have to be paired up: CPLMaster (the file itself — filename, dimensions,
// a presigned download URL per resolution/type) and CPLAsset (per-library
// metadata — taken date, favorite/hidden flags — linked back to its master
// via masterRef). The download URLs come back directly in the listing
// response; unlike gphotos there's no separate "prepare download" step.
const photoPageSize = 200

// desiredPhotoKeys is sent verbatim on every query — trimming it is
// unverified against the real API, so this keeps the exact working set
// confirmed in icloudgo's listQueryGenerate rather than guessing which
// fields are safe to drop.
var desiredPhotoKeys = []string{
	"resJPEGFullWidth", "resJPEGFullHeight", "resJPEGFullFileType", "resJPEGFullFingerprint", "resJPEGFullRes",
	"resJPEGLargeWidth", "resJPEGLargeHeight", "resJPEGLargeFileType", "resJPEGLargeFingerprint", "resJPEGLargeRes",
	"resJPEGMedWidth", "resJPEGMedHeight", "resJPEGMedFileType", "resJPEGMedFingerprint", "resJPEGMedRes",
	"resJPEGThumbWidth", "resJPEGThumbHeight", "resJPEGThumbFileType", "resJPEGThumbFingerprint", "resJPEGThumbRes",
	"resVidFullWidth", "resVidFullHeight", "resVidFullFileType", "resVidFullFingerprint", "resVidFullRes",
	"resVidMedWidth", "resVidMedHeight", "resVidMedFileType", "resVidMedFingerprint", "resVidMedRes",
	"resVidSmallWidth", "resVidSmallHeight", "resVidSmallFileType", "resVidSmallFingerprint", "resVidSmallRes",
	"resSidecarWidth", "resSidecarHeight", "resSidecarFileType", "resSidecarFingerprint", "resSidecarRes",
	"itemType", "dataClassType", "filenameEnc", "originalOrientation",
	"resOriginalWidth", "resOriginalHeight", "resOriginalFileType", "resOriginalFingerprint", "resOriginalRes",
	"resOriginalAltWidth", "resOriginalAltHeight", "resOriginalAltFileType", "resOriginalAltFingerprint", "resOriginalAltRes",
	"resOriginalVidComplWidth", "resOriginalVidComplHeight", "resOriginalVidComplFileType", "resOriginalVidComplFingerprint", "resOriginalVidComplRes",
	"isDeleted", "isExpunged", "dateExpunged", "remappedRef", "recordName", "recordType", "recordChangeTag",
	"masterRef", "adjustmentRenderType", "assetDate", "addedDate", "isFavorite", "isHidden", "orientation",
	"duration", "assetSubtype", "assetSubtypeV2", "assetHDRType", "burstFlags", "burstFlagsExt", "burstId",
	"captionEnc", "locationEnc", "locationV2Enc", "locationLatitude", "locationLongitude", "adjustmentType",
	"timeZoneOffset", "vidComplDurValue", "vidComplDurScale", "vidComplDispValue", "vidComplDispScale",
	"vidComplVisibilityState", "customRenderedValue", "containerId", "itemId", "position", "isKeyAsset",
}

// photoItem is one paired-up CPLMaster+CPLAsset: everything Mirror needs to
// place and fetch one library item.
type photoItem struct {
	ID       string // CPLMaster's recordName — stable per-item identifier
	Filename string // decoded filenameEnc, or ID if that's missing/empty

	AssetDateMS int64 // epoch ms
	Hidden      bool

	OriginalURL  string
	OriginalSize int64

	// LivePhotoVideoURL is the paired video's download URL for a Live
	// Photo, empty otherwise. Detection matches icloudgo's IsLivePhoto:
	// both the still and the video must have a real download URL.
	LivePhotoVideoURL  string
	LivePhotoVideoSize int64
}

func (it photoItem) IsLivePhoto() bool {
	return it.OriginalURL != "" && it.LivePhotoVideoURL != ""
}

func (it photoItem) AssetDate() time.Time {
	return time.UnixMilli(it.AssetDateMS)
}

// listPhotosPage fetches one page of album, starting at offset.
func (c *Client) listPhotosPage(ctx context.Context, serviceEndpoint string, album albumQuery, offset int64) ([]photoItem, error) {
	filterBy := append([]queryFilter{
		{FieldName: "startRank", Comparator: "EQUALS", FieldValue: queryValue{Type: "INT64", Value: offset}},
		{FieldName: "direction", Comparator: "EQUALS", FieldValue: queryValue{Type: "STRING", Value: "ASCENDING"}},
	}, album.filterBy...)

	body := map[string]any{
		"query": map[string]any{
			"filterBy":   filterBy,
			"recordType": album.recordType,
		},
		"resultsLimit": photoPageSize,
		"desiredKeys":  desiredPhotoKeys,
		"zoneID":       map[string]string{"zoneName": "PrimarySync"},
	}

	respBody, _, err := c.do(ctx, &apiRequest{
		method:  http.MethodPost,
		url:     serviceEndpoint + "/records/query",
		query:   url.Values{"remapEnums": {"true"}, "getCurrentSyncToken": {"true"}},
		headers: c.commonHeaders(nil),
		body:    body,
	})
	if err != nil {
		return nil, fmt.Errorf("listing photos at offset %d: %w", offset, err)
	}
	return parsePhotosResponse(respBody)
}

// cloudKitRecord is the raw shape of one CPLMaster or CPLAsset record.
// Trimmed to only the fields this package reads (confirmed against
// icloudgo's photoRecord).
type cloudKitRecord struct {
	RecordName string `json:"recordName"`
	RecordType string `json:"recordType"`
	Fields     struct {
		FilenameEnc            strField `json:"filenameEnc"`
		ResOriginalRes         urlField `json:"resOriginalRes"`
		ResOriginalVidComplRes urlField `json:"resOriginalVidComplRes"`
		MasterRef              struct {
			Value struct {
				RecordName string `json:"recordName"`
			} `json:"value"`
		} `json:"masterRef"`
		AssetDate intField `json:"assetDate"`
		IsHidden  intField `json:"isHidden"`
	} `json:"fields"`
}

type strField struct {
	Value string `json:"value"`
}

type intField struct {
	Value int64 `json:"value"`
}

type urlField struct {
	Value struct {
		DownloadURL string `json:"downloadURL"`
		Size        int64  `json:"size"`
	} `json:"value"`
}

// parsePhotosResponse pairs each page's CPLAsset/CPLMaster records into
// photoItems, preserving CPLMaster encounter order (matching icloudgo's
// GetPhotosByOffset, whose ordering this mirrors so offset-based paging
// stays consistent with what a real run against this same account saw).
func parsePhotosResponse(body []byte) ([]photoItem, error) {
	var resp struct {
		Records []cloudKitRecord `json:"records"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing photos response: %w", err)
	}

	assetByMasterID := map[string]cloudKitRecord{}
	var masterOrder []cloudKitRecord
	for _, rec := range resp.Records {
		switch rec.RecordType {
		case "CPLAsset":
			assetByMasterID[rec.Fields.MasterRef.Value.RecordName] = rec
		case "CPLMaster":
			masterOrder = append(masterOrder, rec)
		}
	}

	items := make([]photoItem, 0, len(masterOrder))
	for _, master := range masterOrder {
		asset, ok := assetByMasterID[master.RecordName]
		if !ok {
			continue // no per-library metadata (taken date, ...) without a matching CPLAsset — skip rather than guess
		}
		filename := decodeFilename(master.Fields.FilenameEnc.Value, master.RecordName)
		item := photoItem{
			ID:           master.RecordName,
			Filename:     filename,
			AssetDateMS:  asset.Fields.AssetDate.Value,
			Hidden:       asset.Fields.IsHidden.Value != 0,
			OriginalURL:  master.Fields.ResOriginalRes.Value.DownloadURL,
			OriginalSize: master.Fields.ResOriginalRes.Value.Size,
		}
		if master.Fields.ResOriginalVidComplRes.Value.DownloadURL != "" {
			item.LivePhotoVideoURL = master.Fields.ResOriginalVidComplRes.Value.DownloadURL
			item.LivePhotoVideoSize = master.Fields.ResOriginalVidComplRes.Value.Size
		}
		items = append(items, item)
	}
	return items, nil
}

// decodeFilename base64-decodes a CPLMaster's filenameEnc field (how iCloud
// stores the original filename) and sanitizes it for local use, falling
// back to id if the field's missing/empty/undecodable.
func decodeFilename(filenameEnc, id string) string {
	if filenameEnc != "" {
		if bs, err := base64.StdEncoding.DecodeString(filenameEnc); err == nil && len(bs) > 0 {
			return cleanFilename(string(bs))
		}
	}
	return id
}

// invalidPathChars covers what's actually unsafe on the filesystems this
// tool targets (macOS/Linux): the path separator and control characters.
// Unlike icloudgo's much more aggressive cleanName (which also strips
// spaces and most punctuation), this keeps filenames looking like their
// real iCloud name wherever the underlying filesystem already tolerates it.
var invalidPathChars = regexp.MustCompile(`[/\x00-\x1f]`)

func cleanFilename(s string) string {
	return invalidPathChars.ReplaceAllString(s, "_")
}

// livePhotoVideoName derives a Live Photo video's filename from its still
// image's, matching icloudgo's Filename(livePhoto=true): same base name,
// .MOV extension.
func livePhotoVideoName(stillFilename string) string {
	base, _, found := strings.Cut(stillFilename, ".")
	if !found {
		return stillFilename + ".MOV"
	}
	return base + ".MOV"
}

// ---- writing files: atomic, collision-safe, date-organized ----
//
// This section mirrors gphotos' resolveFilename/writeAtomically/dateSubdir
// design (see that package for the reasoning) — not shared code, since
// backends are independent packages, but the same proven approach.

// tmpSuffix marks a file as a download in progress; writeAtomically writes
// here first and only renames to the real name after the copy finishes
// successfully, so a cut-short download — network failure, the process
// getting killed outright — never leaves a truncated file at the real name.
const tmpSuffix = ".icloud-tmp"

func writeAtomically(destDir, filename string, r io.Reader) error {
	finalPath := filepath.Join(destDir, filename)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return err
	}
	tmpPath := finalPath + tmpSuffix
	if err := writeBody(tmpPath, r); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, finalPath)
}

func writeBody(path string, r io.Reader) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, r)
	return err
}

func removeStaleTempFiles(destDir string) error {
	return filepath.WalkDir(destDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, tmpSuffix) {
			return os.Remove(path)
		}
		return nil
	})
}

// dateSubdir returns the "YYYY/MM" subdirectory an item's files are
// organized under, based on its AssetDate in UTC. Unlike gphotos, this
// doesn't adjust for the item's own capture-location timezone: iCloud's
// timeZoneOffset field's unit isn't confirmed against real traffic the way
// gphotos' was, and getting a folder boundary off by a few hours near
// midnight is a low-stakes enough cosmetic issue that it's not worth
// guessing at an unverified field to fix it.
func dateSubdir(assetDate time.Time) string {
	utc := assetDate.UTC()
	return filepath.Join(fmt.Sprintf("%04d", utc.Year()), fmt.Sprintf("%02d", int(utc.Month())))
}

// resolveFilename decides the destDir-relative path (subdir + filename) an
// item should be saved under, and reports whether that file is already
// sitting in destDir from before.
//
//   - If the candidate isn't in used and nothing exists at destDir/candidate,
//     it's free: claim it, alreadyOnDisk is false.
//   - If a file already exists there and candidate is NOT in used, nothing
//     currently known claims it, so it's assumed to be this same item's own
//     file from a previous run (e.g. before the index existed). Adopt it:
//     alreadyOnDisk is true, caller should record it without re-downloading.
//   - If candidate IS in used (claimed by a different, already-known item —
//     from the index or earlier this run), that's a genuine collision
//     between two distinct items; append a numeric suffix and try again
//     rather than ever overwriting.
func resolveFilename(destDir, subdir, name string, used map[string]bool) (final string, alreadyOnDisk bool) {
	candidate := filepath.Join(subdir, name)
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		if !used[candidate] {
			if _, err := os.Stat(filepath.Join(destDir, candidate)); os.IsNotExist(err) {
				alreadyOnDisk = false
				break
			}
			alreadyOnDisk = true
			break
		}
		candidate = filepath.Join(subdir, fmt.Sprintf("%s-%d%s", base, i, ext))
	}
	used[candidate] = true
	return candidate, alreadyOnDisk
}

// ---- local download index: skip items already mirrored on a prior run ----

const indexFileName = ".icloud-index.json"

// An item normally downloads as a single file, recorded in Filename alone.
// A Live Photo additionally populates VideoFilename with its paired video,
// so both files are tracked under the item's one ID and both stay
// protected from future name collisions (see Mirror's usedNames seeding).
type indexEntry struct {
	Filename      string `json:"filename"`
	VideoFilename string `json:"video_filename,omitempty"`
	AssetDateMS   int64  `json:"asset_date_ms"`
}

func (e indexEntry) filenames() []string {
	if e.Filename == "" {
		return nil
	}
	if e.VideoFilename == "" {
		return []string{e.Filename}
	}
	return []string{e.Filename, e.VideoFilename}
}

// downloadIndex tracks which items are already mirrored, plus a resume
// offset (see Mirror): "All Photos" is walked oldest-added-first, so newly
// added items only ever appear at increasing offsets, letting a later run
// resume near where the last one left off instead of re-listing the whole
// library's metadata every time. It's the source of truth for "already
// downloaded" — a run never re-checks whether a recorded file is still on
// disk (delete the index, or an entry from it, to force a re-download).
type downloadIndex struct {
	path       string
	NextOffset int64                 `json:"next_offset"`
	Entries    map[string]indexEntry `json:"entries"`
}

func loadDownloadIndex(destDir string) (*downloadIndex, error) {
	idx := &downloadIndex{path: filepath.Join(destDir, indexFileName), Entries: map[string]indexEntry{}}
	data, err := os.ReadFile(idx.path)
	if errors.Is(err, os.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, idx); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", idx.path, err)
	}
	if idx.Entries == nil {
		idx.Entries = map[string]indexEntry{}
	}
	return idx, nil
}

func (idx *downloadIndex) has(id string) bool {
	_, ok := idx.Entries[id]
	return ok
}

func (idx *downloadIndex) record(id, filename, videoFilename string, assetDateMS int64) {
	idx.Entries[id] = indexEntry{Filename: filename, VideoFilename: videoFilename, AssetDateMS: assetDateMS}
}

func (idx *downloadIndex) save() error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	tmp := idx.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, idx.path)
}

// ---- downloading ----

// downloadURL fetches url (one of a photoItem's presigned download URLs)
// and writes it to destDir/filename atomically.
func downloadURL(ctx context.Context, c *http.Client, downloadURL, filename, destDir string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}
	return writeAtomically(destDir, filename, resp.Body)
}

// ---- Mirror ----

// Mirror downloads iCloud Photos items from album into destDir, using the
// Apple ID appleID. password is used only if no still-valid session is
// already saved under sessionDir; if empty in that situation, a 2FA code
// (if the account needs one) is still prompted for interactively, but the
// password itself is not — Mirror returns an error asking for
// -password-file/$ICLOUD_PASSWORD instead of trying to prompt for a
// password on stdin. (2FA codes are single-use and short-lived, so
// prompting for them is harmless; a password prompt would mean silently
// reading a secret from a channel the caller may not have intended — and
// unlike a 2FA code, there's no ready terminal-safe way to hide the input
// without a new dependency, so this package doesn't try.)
//
// after and before are YYYY-MM-DD date strings restricting which items (by
// taken date) get downloaded; either may be empty, meaning no lower bound
// (after) or up to now (before). withLivePhotoVideo controls whether a Live
// Photo's paired video is downloaded alongside its still image.
func Mirror(ctx context.Context, appleID, destDir, password, sessionDir, album string, withLivePhotoVideo bool, after, before string) error {
	var afterT time.Time
	if after != "" {
		t, err := time.Parse("2006-01-02", after)
		if err != nil {
			return fmt.Errorf("bad after date: %w", err)
		}
		afterT = t
	}
	beforeT := time.Now()
	if before != "" {
		t, err := time.Parse("2006-01-02", before)
		if err != nil {
			return fmt.Errorf("bad before date: %w", err)
		}
		beforeT = t.Add(24 * time.Hour) // inclusive end of day
	}

	aq, ok := smartAlbumQueries[album]
	if !ok {
		return fmt.Errorf("unknown album %q (custom, non-smart albums aren't supported yet; known albums: %s)", album, knownAlbumNames())
	}

	client, err := newClient(appleID, sessionDir)
	if err != nil {
		return err
	}
	if err := client.authenticate(ctx, password, func() (string, error) {
		return promptStdin(fmt.Sprintf("Enter the 2FA code sent to your trusted device for %s: ", appleID))
	}); err != nil {
		return err
	}
	if err := client.saveSession(); err != nil {
		log.Printf("icloud: saving session: %v", err)
	}

	serviceEndpoint, err := client.photoServiceEndpoint()
	if err != nil {
		return err
	}

	idx, err := loadDownloadIndex(destDir)
	if err != nil {
		return fmt.Errorf("loading download index: %w", err)
	}
	if err := removeStaleTempFiles(destDir); err != nil {
		log.Printf("icloud: cleaning up stale temp files: %v", err)
	}

	log.Printf("icloud: mirroring %s (%s) into %s", appleID, album, destDir)

	usedNames := map[string]bool{}
	for _, e := range idx.Entries {
		for _, fn := range e.filenames() {
			usedNames[fn] = true
		}
	}

	total, adopted, alreadyHave, skipped, outOfRange := 0, 0, 0, 0, 0
	offset := idx.NextOffset
	if offset < 0 {
		offset = 0
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		items, err := client.listPhotosPage(ctx, serviceEndpoint, aq, offset)
		if err != nil {
			return fmt.Errorf("listing photos: %w", err)
		}
		if len(items) == 0 {
			break
		}

		for _, it := range items {
			assetDate := it.AssetDate()
			if assetDate.Before(afterT) || assetDate.After(beforeT) {
				outOfRange++
				continue
			}
			if idx.has(it.ID) {
				alreadyHave++
				continue
			}

			subdir := dateSubdir(assetDate)
			fn, alreadyOnDisk := resolveFilename(destDir, subdir, it.Filename, usedNames)
			if alreadyOnDisk {
				log.Printf("icloud: adopting existing %s into index (%s)", fn, assetDate.Format(time.RFC3339))
				idx.record(it.ID, fn, "", it.AssetDateMS)
				adopted++
			} else {
				log.Printf("icloud: downloading %s (%s)", fn, assetDate.Format(time.RFC3339))
				if err := downloadURL(ctx, client.httpClient, it.OriginalURL, fn, destDir); err != nil {
					log.Printf("icloud:   skip %s: %v", it.ID, err)
					skipped++
					continue
				}

				videoFn := ""
				if withLivePhotoVideo && it.IsLivePhoto() {
					vfn, videoAlreadyOnDisk := resolveFilename(destDir, subdir, livePhotoVideoName(it.Filename), usedNames)
					if !videoAlreadyOnDisk {
						if err := downloadURL(ctx, client.httpClient, it.LivePhotoVideoURL, vfn, destDir); err != nil {
							log.Printf("icloud:   live photo video for %s failed: %v", it.ID, err)
							vfn = ""
						}
					}
					if vfn != "" {
						videoFn = vfn
						log.Printf("icloud:   %s came with a Live Photo video: %s", it.ID, vfn)
					}
				}

				idx.record(it.ID, fn, videoFn, it.AssetDateMS)
				total++
			}

			// Save after every item, not just at the end of a page, so a
			// run interrupted mid-page doesn't lose credit for what it
			// already downloaded and re-fetch it next time.
			if err := idx.save(); err != nil {
				log.Printf("icloud: saving download index: %v", err)
			}
		}

		offset += int64(len(items))
		idx.NextOffset = offset
		if err := idx.save(); err != nil {
			log.Printf("icloud: saving download index: %v", err)
		}
	}

	log.Printf("icloud: done: %d downloaded, %d adopted from disk, %d already had, %d out of date range, %d skipped",
		total, adopted, alreadyHave, outOfRange, skipped)
	return nil
}

func (c *Client) photoServiceEndpoint() (string, error) {
	if c.validated == nil {
		return "", fmt.Errorf("not authenticated")
	}
	ws, ok := c.validated.Webservices["ckdatabasews"]
	if !ok || ws.URL == "" {
		return "", fmt.Errorf("ckdatabasews (photos) service not available on this account")
	}
	return ws.URL + "/database/1/com.apple.photos.cloud/production/private", nil
}

func knownAlbumNames() string {
	names := make([]string, 0, len(smartAlbumQueries))
	for name := range smartAlbumQueries {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
