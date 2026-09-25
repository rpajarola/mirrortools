// Package gphotos downloads original-quality Google Photos in a date range
// by talking directly to the same undocumented batchexecute API the
// photos.google.com web app uses. Ported from
// github.com/xob0t/google_photos_web_client (RPC IDs: lcxiM = list by taken
// date, EWgK9e = batch item info/filenames).
//
// This is NOT the official API. It uses your logged-in browser session
// (cookies), not OAuth — as of March 2025 there's no OAuth-based official
// API left that can bulk-read an existing library at all, so this is the
// only way to mirror one programmatically. Cookies come from either a
// one-time manual cookies.txt export or, on macOS, directly from Safari's
// live cookie store (see loadCookies). There's no browser/Playwright
// dependency at runtime, only for the initial cookies.txt export if you use
// that route.
//
// It registers itself as the "gphotos" method with mirrortools' mirror
// package, so it's normally driven through the mirror CLI:
//
//	mirror gphotos -cookies cookies.txt -after 2026-09-06 someone@gmail.com ./photos
//	mirror gphotos -cookies cookies.txt -last 30d someone@gmail.com ./photos
package gphotos

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rpajarola/mirrortools/mirror"
)

func init() {
	mirror.Register(&mirror.Method{
		Name: "gphotos",
		Source: "Google account email — currently used only for logging; " +
			"the account actually mirrored is whichever one -cookies belongs to",
		Describe: "download original-quality Google Photos via the undocumented photos.google.com batchexecute API",
		SetupFlags: func(fs *flag.FlagSet) mirror.Func {
			cookiesPath := fs.String("cookies", "cookies.txt", "cookies for photos.google.com: either a Netscape cookies.txt export, "+
				"or (macOS) a path straight to a binarycookies file, e.g. ~/Library/Cookies/Cookies.binarycookies for Safari's live jar "+
				"— format is auto-detected")
			after := fs.String("after", "", "only download items taken on/after this date, YYYY-MM-DD (default: no lower bound)")
			before := fs.String("before", "", "only download items taken on/before this date, YYYY-MM-DD (default: now)")
			last := fs.String("last", "", "only download items taken in the last duration, e.g. 30d, 2w, 6m, 1y "+
				"(an alternative to -after, relative to -before or now; mutually exclusive with -after)")
			companions := fs.Bool("companions", true, "also fetch each item's RAW/DNG companion, for a Pixel RAW+JPEG "+
				"capture pair (filenames like PXL_..._RAW-01.COVER.jpg) — via Google's undocumented per-group lookup. "+
				"A bad response is always safely detected and skipped rather than saved. On by default; pass -companions=false to disable")
			return func(ctx context.Context, source, destDir string) error {
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
				return Mirror(ctx, source, destDir, *cookiesPath, effectiveAfter, *before, *companions)
			}
		},
	})
}

var lastRe = regexp.MustCompile(`^(\d+)([dwmy])$`)

// parseLast parses a relative range like "30d", "2w", "6m", "1y" into the
// time that many days/weeks/months/years before ref. Weeks are exactly 7
// days; months and years use calendar-aware time.Time.AddDate (so "1m"
// before March 31 lands on the right day for whatever month that is, rather
// than a fixed 30-day approximation).
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

// ---- global session data, scraped once from the photos.google.com HTML ----

type globalData struct {
	FSid   string // f.sid  - "FdrFJe"
	Bl     string // build label - "cfb2h"
	At     string // XSRF token - "SNlM0e"
	ImPath string // batchexecute path prefix - "Im6cmf", e.g. "/_/PhotosUi"
}

var globalDataRe = regexp.MustCompile(`(?s)window\.WIZ_global_data\s*=\s*(\{.*?\});`)

func fetchGlobalData(ctx context.Context, c *http.Client) (*globalData, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://photos.google.com/", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	m := globalDataRe.FindSubmatch(body)
	if m == nil {
		// Whatever page this actually is — a sign-in form, an account
		// chooser, a consent interstitial — its URL and a snippet of its
		// body says a lot more than "not found" about why: were we bounced
		// off photos.google.com entirely, and to where.
		log.Printf("gphotos: WIZ_global_data not found: http %d, landed on %s, body: %s",
			resp.StatusCode, resp.Request.URL, snippet(body, 300))
		return nil, fmt.Errorf("WIZ_global_data not found — cookies likely invalid/expired, re-export them")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(m[1], &raw); err != nil {
		return nil, err
	}
	get := func(k string) string {
		var s string
		if v, ok := raw[k]; ok {
			json.Unmarshal(v, &s)
		}
		return s
	}
	return &globalData{
		FSid:   get("FdrFJe"),
		Bl:     get("cfb2h"),
		At:     get("SNlM0e"),
		ImPath: get("Im6cmf"),
	}, nil
}

// ---- keeping a long-running session's cookies alive ----
//
// Google issues two kinds of cookies for photos.google.com (and every other
// first-party Google web app): long-lived account cookies (SID, HSID,
// SAPISID, __Secure-1PSID, ...) good for weeks, and a short-lived rotating
// session token (__Secure-1PSIDTS / __Secure-3PSIDTS) that a real browser
// silently refreshes every so often via a background request while the tab
// stays open. A cookies.txt export only captures a snapshot, so that
// rotating token starts going stale within minutes of export — that's
// almost certainly what "cookies expire after a few minutes" describes.
// Since this process doesn't run the page's JS, nothing refreshes it on its
// own; without doing this explicitly, a long mirror run's requests would
// just start failing partway through once the exported token ages out. The
// XSRF "at" token in globalData is tied to the same page-load session, so
// it's refreshed alongside it.
//
// The rotation endpoint itself (POST accounts.google.com/RotateCookies)
// is confirmed working — for a different Google product's web app
// (Gemini), by an actively maintained reverse-engineered client
// (github.com/HanaokaYuzu/Gemini-API) — not verified here against
// photos.google.com specifically. It lives on accounts.google.com, Google's
// shared identity domain rather than a product-specific one, which is why
// it's expected to refresh the session cookies for any first-party Google
// property, not just Gemini's. Failure here is graceful either way: it only
// refreshes cookies already in the jar and never touches downloaded
// content, so a wrong guess just means the session keeps aging on its
// existing cookies rather than corrupting anything — Mirror logs a warning
// and carries on with whatever session it already has.
const (
	rotateCookiesURL       = "https://accounts.google.com/RotateCookies"
	sessionRefreshInterval = 2 * time.Minute
)

// cookieWatchList is logged (fingerprints only, never full values) at key
// points — after the initial cookies.txt load and around every rotation —
// to help diagnose session problems from the log alone: are the long-lived
// account cookies even present, and does rotateCookies actually change the
// short-lived ones.
var cookieWatchList = []string{"SID", "__Secure-1PSID", "__Secure-1PSIDTS", "__Secure-3PSIDTS", "SAPISID"}

// cookieValue returns the named cookie's current value for host from c's
// jar, or "" if absent.
func cookieValue(c *http.Client, host, name string) string {
	if c.Jar == nil {
		return ""
	}
	for _, ck := range c.Jar.Cookies(&url.URL{Scheme: "https", Host: host}) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

// fingerprint summarizes a cookie value for logging without exposing
// anything an attacker could replay: presence and length, plus a few
// trailing characters as a change-detector (so two log lines can show
// "still the same value" vs. "this rotated") without logging enough to
// reconstruct the cookie.
func fingerprint(v string) string {
	if v == "" {
		return "<absent>"
	}
	tail := v
	if len(tail) > 6 {
		tail = tail[len(tail)-6:]
	}
	return fmt.Sprintf("<len=%d ...%s>", len(v), tail)
}

// logCookieStatus logs a fingerprint of every cookie in cookieWatchList,
// for host "google.com" (a domain cookie there covers photos.google.com,
// accounts.google.com, etc. — see loadCookiesFromNetscapeFile).
func logCookieStatus(c *http.Client, label string) {
	parts := make([]string, len(cookieWatchList))
	for i, name := range cookieWatchList {
		parts[i] = name + "=" + fingerprint(cookieValue(c, "google.com", name))
	}
	log.Printf("gphotos: %s: %s", label, strings.Join(parts, " "))
}

// rotateCookies asks Google to refresh the short-lived session cookies in
// c's jar. The request body is the literal payload the reference
// implementation above sends; its meaning isn't documented anywhere public.
func rotateCookies(ctx context.Context, c *http.Client, url string) error {
	before := fingerprint(cookieValue(c, "google.com", "__Secure-1PSIDTS"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`[000,"-0000000000000000000"]`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://accounts.google.com")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	after := fingerprint(cookieValue(c, "google.com", "__Secure-1PSIDTS"))
	log.Printf("gphotos: rotate cookies: http %d, __Secure-1PSIDTS before=%s after=%s, response body=%s",
		resp.StatusCode, before, after, snippet(body, 300))
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("session fully expired (401) — re-export cookies.txt")
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// refreshSession rotates the session cookie and re-fetches globalData (for
// a fresh XSRF "at" token) so a long-running Mirror doesn't start failing
// partway through. Call no more than once every sessionRefreshInterval —
// Google's rotation endpoint 429s if hit too often.
func refreshSession(ctx context.Context, c *http.Client) (*globalData, error) {
	if err := rotateCookies(ctx, c, rotateCookiesURL); err != nil {
		return nil, fmt.Errorf("rotating cookies: %w", err)
	}
	gd, err := fetchGlobalData(ctx, c)
	logCookieStatus(c, "cookies after session refresh")
	return gd, err
}

// ---- batchexecute transport ----

// callRPC sends one RPC and returns the raw JSON payload for its response
// (already unwrapped from the wrb.fr envelope and the outer string-escaping).
func callRPC(ctx context.Context, c *http.Client, gd *globalData, rpcid string, data any) (json.RawMessage, error) {
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	const payloadID = "gphotospull0" // arbitrary id; must match on request/response

	fReq, _ := json.Marshal([][]any{{[]any{rpcid, string(dataJSON), nil, payloadID}}})

	form := url.Values{"f.req": {string(fReq)}, "at": {gd.At}}
	q := url.Values{
		"rpcids":      {rpcid},
		"source-path": {"/"},
		"f.sid":       {gd.FSid},
		"bl":          {gd.Bl},
		"rt":          {"c"},
	}
	endpoint := "https://photos.google.com" + gd.ImPath + "/data/batchexecute?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")

	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	raw, err := parseWrbFr(body, payloadID)
	if err != nil {
		// Not always fatal to the caller (batchFilenames degrades to
		// media-key filenames on failure, for instance), but a bad RPC
		// response is the first symptom of a session gone stale, so this
		// logs regardless of what the caller ends up doing about it —
		// otherwise a listPage failure surfaces as just "list page: RPC
		// lcxiM: no matching wrb.fr response found", with no way to tell
		// from that alone whether it's an auth redirect, a rate limit, or
		// something else.
		log.Printf("gphotos: RPC %s failed (http %d): %v; response: %s", rpcid, resp.StatusCode, err, snippet(body, 300))
		return nil, fmt.Errorf("RPC %s: %w", rpcid, err)
	}
	return raw, nil
}

// snippet renders body as a single log line: newlines escaped, truncated to
// n bytes.
func snippet(body []byte, n int) string {
	s := strings.ReplaceAll(string(body), "\n", "\\n")
	if len(s) > n {
		s = s[:n] + "...(truncated)"
	}
	return s
}

// parseWrbFr replicates gpwc's trick: the response is Google's chunked
// batchexecute format ()]}' header + numeric length-prefix lines), but the
// actual data-bearing line is compact JSON containing "wrb.fr" and fits on
// one line, so splitting on "\n" and filtering for that substring works
// without a real chunk decoder.
func parseWrbFr(body []byte, wantPayloadID string) (json.RawMessage, error) {
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.Contains(line, "wrb.fr") {
			continue
		}
		var outer []json.RawMessage
		if err := json.Unmarshal([]byte(line), &outer); err != nil {
			continue
		}
		var entry []json.RawMessage // ["wrb.fr", rpcid, dataStr, null, null, null, payloadID]
		if err := json.Unmarshal(outer[0], &entry); err != nil || len(entry) < 7 {
			continue
		}
		var payloadID string
		json.Unmarshal(entry[6], &payloadID)
		if payloadID != wantPayloadID {
			continue
		}
		var dataStr string
		if err := json.Unmarshal(entry[2], &dataStr); err != nil || dataStr == "" {
			return nil, fmt.Errorf("rpc returned no data (likely an error response)")
		}
		return json.RawMessage(dataStr), nil
	}
	return nil, fmt.Errorf("no matching wrb.fr response found")
}

// ---- lcxiM: list library items by taken date, paginated, newest first ----

type libraryItem struct {
	MediaKey    string
	BaseURL     string // thumbnail/base url; append "=d" (photo) or "=dv" (video) for original
	TimestampMS int64
	DedupKey    string
	// TimezoneOffsetMS is the item's own capture timezone, as an offset
	// from UTC in milliseconds (e.g. 32400000 for UTC+9). Used to bucket
	// files by the date they were actually taken in local time, not
	// whatever UTC happens to say. Zero (UTC) if the response didn't
	// include one.
	TimezoneOffsetMS int64
}

// parseLibraryItems decodes a list of items in the common shape shared by
// lcxiM (library listing) and wgZjtc (RAW-group lookup): each item is
// [media_key, [base_url, width, height, ...], timestamp_ms, dedup_key,
// timezone_offset_ms, ...].
func parseLibraryItems(rawItems []json.RawMessage) []libraryItem {
	items := make([]libraryItem, 0, len(rawItems))
	for _, ri := range rawItems {
		var arr []json.RawMessage
		if err := json.Unmarshal(ri, &arr); err != nil || len(arr) < 4 {
			continue
		}
		var mediaKey, dedupKey string
		var thumb []json.RawMessage
		var ts, tzOffsetMS int64
		json.Unmarshal(arr[0], &mediaKey)
		json.Unmarshal(arr[1], &thumb)
		json.Unmarshal(arr[2], &ts)
		json.Unmarshal(arr[3], &dedupKey)
		if len(arr) > 4 {
			json.Unmarshal(arr[4], &tzOffsetMS)
		}
		baseURL := ""
		if len(thumb) > 0 {
			json.Unmarshal(thumb[0], &baseURL)
		}
		items = append(items, libraryItem{
			MediaKey: mediaKey, BaseURL: baseURL, TimestampMS: ts, DedupKey: dedupKey,
			TimezoneOffsetMS: tzOffsetMS,
		})
	}
	return items
}

// listPage fetches one page starting at startTS (epoch ms, nil = most recent).
// Google returns items newest-first, so paging with a starting timestamp is
// how you jump into an arbitrary date range instead of walking the whole lib.
func listPage(ctx context.Context, c *http.Client, gd *globalData, startTS *int64, pageID *string) (items []libraryItem, nextPageID *string, err error) {
	// data = [page_id, timestamp, page_size, null, 1, source(3=both)]
	data := []any{pageID, startTS, 500, nil, 1, 3}
	raw, err := callRPC(ctx, c, gd, "lcxiM", data)
	if err != nil {
		return nil, nil, err
	}
	var page []json.RawMessage
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, nil, err
	}
	if len(page) > 0 && string(page[0]) != "null" {
		var rawItems []json.RawMessage
		json.Unmarshal(page[0], &rawItems)
		items = parseLibraryItems(rawItems)
	}
	if len(page) > 1 && string(page[1]) != "null" {
		var np string
		json.Unmarshal(page[1], &np)
		nextPageID = &np
	}
	return items, nextPageID, nil
}

// ---- EWgK9e: batch-resolve filenames for a set of media keys ----

func batchFilenames(ctx context.Context, c *http.Client, gd *globalData, mediaKeys []string) (map[string]string, error) {
	keys := make([][]string, len(mediaKeys))
	for i, k := range mediaKeys {
		keys[i] = []string{k}
	}
	tail := make([]any, 0, 35)
	for i := 0; i < 24; i++ {
		tail = append(tail, nil)
	}
	tail = append(tail, []any{})
	for i := 0; i < 10; i++ {
		tail = append(tail, nil)
	}
	tail = append(tail, []any{})
	data := []any{[]any{[]any{keys}, []any{tail}}}

	raw, err := callRPC(ctx, c, gd, "EWgK9e", data)
	if err != nil {
		return nil, err
	}
	return parseBatchFilenames(raw)
}

// parseBatchFilenames extracts media_key -> filename from an EWgK9e RPC
// response. The item list lives at raw[0][1], not at the top level —
// confirmed against the reference Python client's parse_response_data for
// EWgK9e (`safe_get(data, 0, 1)`). Unmarshaling raw directly as the item
// list, as this used to do, silently matched nothing: every filename lookup
// came back empty and every download fell back to the opaque media-key hash
// as its filename.
func parseBatchFilenames(raw json.RawMessage) (map[string]string, error) {
	var outer []json.RawMessage
	if err := json.Unmarshal(raw, &outer); err != nil || len(outer) == 0 {
		return nil, fmt.Errorf("unexpected EWgK9e response shape: %s", raw)
	}
	var wrapper []json.RawMessage
	if err := json.Unmarshal(outer[0], &wrapper); err != nil || len(wrapper) < 2 {
		return nil, fmt.Errorf("unexpected EWgK9e response shape: %s", raw)
	}
	var items []json.RawMessage
	json.Unmarshal(wrapper[1], &items) // absent/null -> no items, not an error

	out := make(map[string]string, len(items))
	for _, ri := range items {
		var arr []json.RawMessage
		if err := json.Unmarshal(ri, &arr); err != nil || len(arr) < 2 {
			continue
		}
		var mediaKey string
		json.Unmarshal(arr[0], &mediaKey)
		var inner []json.RawMessage
		json.Unmarshal(arr[1], &inner)
		if len(inner) > 3 {
			var fn string
			json.Unmarshal(inner[3], &fn)
			out[mediaKey] = fn
		}
	}
	return out, nil
}

// ---- cookies ----

// httpOnlyPrefix marks HttpOnly cookies in some Netscape cookies.txt
// exporters (e.g. "Get cookies.txt LOCALLY"): the domain field of an
// otherwise-normal cookie line gets this prepended, which makes it look like
// a "#"-comment line if you don't know to check for it. Google's actual auth
// cookies (SID, HSID, SSID, __Secure-1PSID, ...) are HttpOnly, so skipping
// these lines silently drops exactly the cookies that matter and produces an
// unauthenticated session.
const httpOnlyPrefix = "#HttpOnly_"

// loadCookies loads cookies from path into jar, auto-detecting the format:
// Apple's binarycookies format (magic "cook" — e.g. macOS's
// ~/Library/Cookies/Cookies.binarycookies, Safari/WebKit's live cookie
// store) or a Netscape-format cookies.txt export. Safari isn't subject to
// Chrome's Device Bound Session Credentials, so pointing this straight at a
// live Safari cookie jar is one way to get a session that isn't dead on
// arrival if that's what's blocking Chrome-exported cookies.
func loadCookies(jar *cookiejar.Jar, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	magic := make([]byte, 4)
	n, _ := io.ReadFull(f, magic)
	f.Close()

	if n == 4 && bytes.Equal(magic, binaryCookiesMagic) {
		return loadCookiesFromBinaryCookies(jar, path)
	}
	return loadCookiesFromNetscapeFile(jar, path)
}

// applyCookies sets byHost's cookies (keyed by bare host — leading "."
// already stripped, see the two loaders below) into jar, failing clearly if
// none of Google's core auth cookies showed up for google.com rather than
// deferring to the much more confusing "WIZ_global_data not found" error
// that would otherwise only surface much later.
func applyCookies(jar *cookiejar.Jar, path string, byHost map[string][]*http.Cookie) error {
	const sidCookie = "SID" // one of the core Google auth cookies; always HttpOnly
	haveSID := false
	for _, c := range byHost["google.com"] {
		if c.Name == sidCookie {
			haveSID = true
			break
		}
	}
	if !haveSID {
		return fmt.Errorf("no %q cookie found for google.com in %s — re-export cookies.txt "+
			"while logged into photos.google.com (make sure the export includes HttpOnly cookies)", sidCookie, path)
	}

	for host, cookies := range byHost {
		u := &url.URL{Scheme: "https", Host: host}
		jar.SetCookies(u, cookies)
	}
	return nil
}

// domainCookie builds an *http.Cookie from a stored domain string that may
// carry a leading dot, splitting it into the bare host to key byHost by and
// the http.Cookie itself. A leading dot means "this domain and all
// subdomains" — a domain cookie; Cookie.Domain has to be set to tell
// cookiejar to treat it that way. Leaving it unset makes a host-only cookie
// good for exactly "google.com" and never sent on requests to
// photos.google.com, which is why every request used to look logged-out no
// matter how fresh the cookies were (see loadCookiesFromNetscapeFile's git
// history for the incident this fixed).
func domainCookie(domain, name, value string) (host string, cookie *http.Cookie) {
	host = strings.TrimPrefix(domain, ".")
	cookie = &http.Cookie{Name: name, Value: value}
	if strings.HasPrefix(domain, ".") {
		cookie.Domain = host
	}
	return host, cookie
}

// loadCookiesFromNetscapeFile loads a Netscape-format cookies.txt (e.g. from
// the "Get cookies.txt LOCALLY" Chrome extension, exported after a manual
// login to photos.google.com). This is the one step that still needs a real
// browser — everything after this is pure HTTP.
func loadCookiesFromNetscapeFile(jar *cookiejar.Jar, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	byHost := map[string][]*http.Cookie{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, httpOnlyPrefix) {
			line = strings.TrimPrefix(line, httpOnlyPrefix)
		} else if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 7 {
			continue
		}
		domain, _, _, _, _, name, value := fields[0], fields[1], fields[2], fields[3], fields[4], fields[5], fields[6]
		host, cookie := domainCookie(domain, name, value)
		byHost[host] = append(byHost[host], cookie)
	}
	if err := sc.Err(); err != nil {
		return err
	}

	return applyCookies(jar, path, byHost)
}

// ---- macOS/Safari binary cookie jar (Cookies.binarycookies) ----
//
// Format reverse-engineered by the community — not documented by Apple —
// cross-checked byte-for-byte against github.com/cixtor/binarycookies, an
// actively maintained independent Go implementation, rather than
// implemented from memory alone:
//
//	"cook" magic (4 bytes)
//	page count (4 bytes, big-endian)
//	one page size per page (4 bytes each, big-endian)
//	for each page:
//	  page tag 00 00 01 00 (4 bytes)
//	  cookie count in this page (4 bytes, little-endian)
//	  one cookie offset per cookie (4 bytes each, little-endian — informational; cookies are read sequentially regardless)
//	  page end 00 00 00 00 (4 bytes)
//	  for each cookie, sequentially:
//	    size (4 bytes LE) — total record size
//	    unknown (4 bytes), flags (4 bytes LE), unknown (4 bytes)
//	    comment/domain/name/path/value offsets (4 bytes LE each, relative to the record start)
//	    end header 00 00 00 00 (4 bytes)
//	    expires, creation (8 bytes LE float64 each — Mac absolute time, seconds since 2001-01-01)
//	    comment, domain, name, path, value: null-terminated strings back to
//	    back in that order; each one's length is the delta to the next
//	    field's offset (the value field's end is the record's overall size)
//	8-byte checksum, optionally followed by a trailing bplist — both ignored
var binaryCookiesMagic = []byte("cook")

// macEpochOffset is the number of seconds from the Unix epoch to
// 2001-01-01, when Mac absolute time (used for this format's timestamps)
// starts.
const macEpochOffset = 978307200

// loadCookiesFromBinaryCookies loads cookies from an Apple binarycookies
// file into jar. Unlike loadCookiesFromNetscapeFile, this reads the
// browser's whole cookie store, not a single site's export, so it's
// filtered to google.com and its subdomains — everything else in there is
// no business of ours to load.
func loadCookiesFromBinaryCookies(jar *cookiejar.Jar, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	r := bytes.NewReader(data)

	magic := make([]byte, 4)
	if _, err := io.ReadFull(r, magic); err != nil || !bytes.Equal(magic, binaryCookiesMagic) {
		return fmt.Errorf("%s: not a binarycookies file (bad magic)", path)
	}

	var pageCount uint32
	if err := binary.Read(r, binary.BigEndian, &pageCount); err != nil {
		return fmt.Errorf("%s: reading page count: %w", path, err)
	}
	pageSizes := make([]uint32, pageCount)
	for i := range pageSizes {
		if err := binary.Read(r, binary.BigEndian, &pageSizes[i]); err != nil {
			return fmt.Errorf("%s: reading page sizes: %w", path, err)
		}
	}

	byHost := map[string][]*http.Cookie{}
	now := time.Now()
	for pageNum, size := range pageSizes {
		page := make([]byte, size)
		if _, err := io.ReadFull(r, page); err != nil {
			return fmt.Errorf("%s: reading page %d: %w", path, pageNum, err)
		}
		cookies, err := parseBinaryCookiesPage(page)
		if err != nil {
			return fmt.Errorf("%s: page %d: %w", path, pageNum, err)
		}
		for _, bc := range cookies {
			if bc.expires.Before(now) {
				continue // stale; a live browser wouldn't send this either
			}
			if host := strings.TrimPrefix(bc.domain, "."); host != "google.com" && !strings.HasSuffix(host, ".google.com") {
				continue
			}
			host, cookie := domainCookie(bc.domain, bc.name, bc.value)
			byHost[host] = append(byHost[host], cookie)
		}
	}

	return applyCookies(jar, path, byHost)
}

// binCookie is one cookie as decoded from a binarycookies page, before
// domain-filtering and conversion to an *http.Cookie.
type binCookie struct {
	domain, name, value string
	expires             time.Time
}

// parseBinaryCookiesPage decodes every cookie in one page.
func parseBinaryCookiesPage(page []byte) ([]binCookie, error) {
	r := bytes.NewReader(page)

	var tag [4]byte
	if _, err := io.ReadFull(r, tag[:]); err != nil {
		return nil, fmt.Errorf("reading page tag: %w", err)
	}
	if tag != [4]byte{0x00, 0x00, 0x01, 0x00} {
		return nil, fmt.Errorf("unexpected page tag %x", tag)
	}

	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, fmt.Errorf("reading cookie count: %w", err)
	}
	offsets := make([]uint32, count)
	for i := range offsets {
		if err := binary.Read(r, binary.LittleEndian, &offsets[i]); err != nil {
			return nil, fmt.Errorf("reading cookie offset: %w", err)
		}
	}
	var end [4]byte
	if _, err := io.ReadFull(r, end[:]); err != nil {
		return nil, fmt.Errorf("reading page end: %w", err)
	}

	cookies := make([]binCookie, 0, count)
	for i := uint32(0); i < count; i++ {
		c, err := parseBinaryCookieRecord(r)
		if err != nil {
			return nil, fmt.Errorf("cookie %d: %w", i, err)
		}
		cookies = append(cookies, c)
	}
	return cookies, nil
}

// parseBinaryCookieRecord decodes one cookie record from r, which must be
// positioned at the record's first byte.
func parseBinaryCookieRecord(r io.Reader) (binCookie, error) {
	var hdr struct {
		Size, Unknown1, Flags, Unknown2                   uint32
		DomainOff, NameOff, PathOff, ValueOff, CommentOff uint32
		End                                               [4]byte
		ExpiresRaw, CreationRaw                           uint64
	}
	if err := binary.Read(r, binary.LittleEndian, &hdr); err != nil {
		return binCookie{}, fmt.Errorf("reading cookie header: %w", err)
	}

	// Fields are laid out back to back starting right here (comment,
	// domain, name, path, value, each null-terminated); each one's nominal
	// length is the delta to the next field's offset. For the value field
	// specifically, current Safari appends a fixed-size trailing
	// "bplist00"-format blob (an NSDate under key "AccessTime") AFTER its
	// null terminator, still within that delta — confirmed by inspecting a
	// real Cookies.binarycookies file's raw bytes byte-for-byte, since
	// neither Apple nor any community write-up of this format documents
	// it. Truncating at the first NUL found, rather than trusting the
	// whole span, discards that trailer instead of appending it onto the
	// cookie value — the earlier version of this function did the latter,
	// which corrupted every cookie's value with ~66 bytes of binary plist
	// data and made every request using them fail authentication.
	readField := func(from, to uint32) (string, error) {
		if to < from {
			return "", fmt.Errorf("invalid field bounds [%d,%d)", from, to)
		}
		buf := make([]byte, to-from)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		if i := bytes.IndexByte(buf, 0); i >= 0 {
			return string(buf[:i]), nil
		}
		return string(buf), nil
	}

	if hdr.CommentOff != 0 {
		if _, err := readField(hdr.CommentOff, hdr.DomainOff); err != nil {
			return binCookie{}, fmt.Errorf("reading comment: %w", err)
		}
	}
	domain, err := readField(hdr.DomainOff, hdr.NameOff)
	if err != nil {
		return binCookie{}, fmt.Errorf("reading domain: %w", err)
	}
	name, err := readField(hdr.NameOff, hdr.PathOff)
	if err != nil {
		return binCookie{}, fmt.Errorf("reading name: %w", err)
	}
	if _, err := readField(hdr.PathOff, hdr.ValueOff); err != nil { // path, unused
		return binCookie{}, fmt.Errorf("reading path: %w", err)
	}
	value, err := readField(hdr.ValueOff, hdr.Size)
	if err != nil {
		return binCookie{}, fmt.Errorf("reading value: %w", err)
	}

	expires := time.Unix(int64(math.Float64frombits(hdr.ExpiresRaw))+macEpochOffset, 0)
	return binCookie{domain: domain, name: name, value: value, expires: expires}, nil
}

// ---- download ----

// resolveFilename decides the local filename for an item, based on Google's
// own filename for it (name, which may be empty if the lookup failed or the
// item has none), and reports whether that file is already sitting in
// destDir from before.
//
// used tracks names already spoken for this run: both ones claimed earlier
// in this same run, and (seeded by the caller) ones already recorded for
// some other media key in the download index. The caller must pass the same
// map for every item.
//
//   - If the resulting name isn't in used and nothing exists at
//     destDir/name, it's free: claim it, alreadyOnDisk is false.
//   - If nothing exists at destDir/name but candidate IS in used, that name
//     provably belongs to a different, already-known item (either indexed
//     from a past run, or being downloaded earlier THIS run) — Google
//     filenames like "IMG_0001.jpg" collide constantly across devices, and
//     silently overwriting a previously-mirrored photo because an unrelated
//     one shares its name would be data loss. Append a numeric suffix and
//     try again rather than ever overwriting.
//   - If a file already exists at destDir/name and candidate is NOT in
//     used, nothing currently known claims it, so it's assumed to be this
//     same item's own file from a previous run — e.g. downloaded before the
//     local index existed, or after the index was lost. Adopt it:
//     alreadyOnDisk is true, and the caller should record it without
//     re-downloading rather than fetching a redundant "-2" copy of itself.
func resolveFilename(destDir, name, mediaKey string, used map[string]bool) (final string, alreadyOnDisk bool) {
	if name == "" {
		name = mediaKey
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	candidate := name
	for i := 2; ; i++ {
		if !used[candidate] {
			if _, err := os.Stat(filepath.Join(destDir, candidate)); os.IsNotExist(err) {
				alreadyOnDisk = false
				break
			}
			alreadyOnDisk = true
			break
		}
		candidate = fmt.Sprintf("%s-%d%s", base, i, ext)
	}
	used[candidate] = true
	return candidate, alreadyOnDisk
}

// tmpSuffix marks a file as a download in progress. writeAtomically never
// lets a file under this name be mistaken for a finished one: it writes
// here first and only renames to the real filename after the copy finishes
// successfully, so a download cut short — network failure, or the process
// getting killed outright (ctrl-C doesn't run deferred Close()s) — leaves
// only a "*.gphotos-tmp" behind, never a truncated file at the real name
// that resolveFilename's later runs could mistake for a complete one.
const tmpSuffix = ".gphotos-tmp"

// videoExtensions lists the file extensions (lowercased, with the leading
// dot) Google Photos uses for video items. Used only by downloadOriginal to
// decide which download suffix to request first, and — for a filename this
// recognizes as a video — to refuse to accept a photo response in its
// place; see downloadOriginal's comment for why that matters.
var videoExtensions = map[string]bool{
	".mp4": true, ".mov": true, ".avi": true, ".mkv": true, ".3gp": true,
	".3g2": true, ".webm": true, ".m4v": true, ".wmv": true, ".mts": true,
	".m2ts": true,
}

func isVideoFilename(name string) bool {
	return videoExtensions[strings.ToLower(filepath.Ext(name))]
}

func downloadOriginal(ctx context.Context, c *http.Client, item libraryItem, filename, destDir string) error {
	if item.BaseURL == "" {
		return fmt.Errorf("no base url for %s", item.MediaKey)
	}
	// "=d" = original quality photo bytes, "=dv" = original video bytes.
	// Distinguishing which an item actually is reliably requires its
	// feature-map (see parser.py in gpwc — LibraryItem.video_duration),
	// which this minimal port doesn't parse. It used to instead just accept
	// whichever suffix came back with any image/* or video/* content-type —
	// but for a genuine video item, "=d" doesn't error or redirect, it
	// returns 200 with an image/jpeg *thumbnail*, which that loose check
	// happily accepted, silently saving a thumbnail under the item's real
	// .mp4 (or similar) filename.
	//
	// Fixed by using the item's own filename extension — it comes straight
	// from Google (via batchFilenames), not guessed — to decide which
	// suffix to try first, and, when it recognizably names a video, to
	// require the response actually be video/*: an image/* response (the
	// thumbnail) is rejected rather than accepted as a fallback, so a
	// failure to fetch the real video bytes surfaces as a clear "could not
	// fetch" error instead of silently mis-saving a thumbnail. A filename
	// extension this doesn't recognize as a video (a photo, or an
	// unrecognized/absent extension) keeps the original loose either-
	// content-type behavior, since there's nothing here to know better with.
	//
	// Note this only ever returns the cover file. A Pixel RAW+JPEG pair's
	// RAW/DNG companion isn't reachable this way at all — see
	// downloadCompanions, which Mirror calls separately (additively, not as
	// a replacement for this) when -companions is on.
	suffixes := []string{"=d", "=dv"}
	requireVideo := isVideoFilename(filename)
	if requireVideo {
		suffixes = []string{"=dv", "=d"}
	}
	for _, suffix := range suffixes {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, item.BaseURL+suffix, nil)
		if err != nil {
			return err
		}
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		ct := resp.Header.Get("Content-Type")
		isVideo := strings.HasPrefix(ct, "video/")
		ok := resp.StatusCode == 200 && (isVideo || strings.HasPrefix(ct, "image/"))
		if ok && requireVideo && !isVideo {
			ok = false // the thumbnail-instead-of-video case described above
		}
		if ok {
			err := writeAtomically(destDir, filename, resp.Body)
			resp.Body.Close()
			return err
		}
		resp.Body.Close()
	}
	return fmt.Errorf("could not fetch original bytes for %s", item.MediaKey)
}

// downloadVideoThumbnail fetches the poster-frame thumbnail Google already
// renders for a video item — item.BaseURL with no suffix, the same bytes a
// bare "=d" request used to be mistaken for the video itself (see
// downloadOriginal) — and saves it alongside videoFilename with its
// extension replaced by ".jpg". Called for every video, unconditionally: a
// video file with no way to preview it short of decoding the video itself
// (most file browsers and galleries won't) is much less useful sitting in a
// mirror, and there's no local way to produce one for free — extracting a
// frame would mean shipping an ffmpeg dependency — whereas Google has
// already rendered exactly this and serves it for the price of one more
// GET.
func downloadVideoThumbnail(ctx context.Context, c *http.Client, item libraryItem, videoFilename, destDir string, used map[string]bool) (string, error) {
	if item.BaseURL == "" {
		return "", fmt.Errorf("no base url for %s", item.MediaKey)
	}
	ext := filepath.Ext(videoFilename)
	thumbName := strings.TrimSuffix(videoFilename, ext) + ".jpg"
	fn, alreadyOnDisk := resolveFilename(destDir, thumbName, item.MediaKey, used)
	if alreadyOnDisk {
		return fn, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, item.BaseURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != 200 || !strings.HasPrefix(ct, "image/") {
		return "", fmt.Errorf("unexpected thumbnail response (status %d, content-type %q)", resp.StatusCode, ct)
	}
	if err := writeAtomically(destDir, fn, resp.Body); err != nil {
		return "", err
	}
	return fn, nil
}

// writeAtomically writes r to destDir/filename via the tmpSuffix
// write-then-rename dance (see tmpSuffix's doc comment), so any partial
// write — network failure, cancellation, the process getting killed
// outright — never leaves a truncated file at the real name. filename may
// include subdirectory components (see dateSubdir); the parent directory is
// created if it doesn't exist yet.
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

// ---- RAW/DNG companions: Pixel RAW+JPEG capture groups (wgZjtc) ----
//
// A Pixel phone shooting RAW+JPEG saves two files sharing one timestamp
// "group ID" — e.g. "PXL_20260911_103945315.RAW-01.COVER.jpg" and
// "PXL_20260911_103945315.RAW-02.ORIGINAL.dng" both share the group ID
// "20260911_103945315". Only the JPEG "cover" is returned by the regular
// library listing (lcxiM); the RAW file has its own media key and base URL
// but isn't a top-level library item. wgZjtc, given the group ID, returns
// every file in the group — confirmed against real captured traffic. This
// replaces an earlier attempt that used Google's multi-select "download
// all" bundle-as-zip flow (yCLA7/dnv2s): that flow turned out to be for
// bundling multiple, possibly unrelated selected items, not for resolving
// one item's own RAW companion, and its download URLs always redirected to
// a Google sign-in page when fetched this way.
//
// This is scoped to the "PXL_...RAW-01(.MP)?.COVER..." filename Pixel
// itself uses; a RAW+JPEG pair from other camera brands, if named
// differently, won't be recognized.
var rawCoverRe = regexp.MustCompile(`(?i)^PXL_(\d{8}_\d+)\.RAW-01(?:\.MP)?\.COVER\.`)

// getRawGroup resolves every file (cover and companions) in the RAW+JPEG
// capture group identified by groupID (see rawCoverRe).
func getRawGroup(ctx context.Context, c *http.Client, gd *globalData, groupID string) ([]libraryItem, error) {
	raw, err := callRPC(ctx, c, gd, "wgZjtc", []any{groupID, nil, 1, 1})
	if err != nil {
		return nil, err
	}
	return parseRawGroup(raw)
}

// parseRawGroup extracts the member items from a wgZjtc response: raw[2] is
// the item list, in the same per-item shape parseLibraryItems already
// handles for lcxiM.
func parseRawGroup(raw json.RawMessage) ([]libraryItem, error) {
	var top []json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || len(top) < 3 {
		return nil, fmt.Errorf("unexpected wgZjtc response shape: %s", raw)
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(top[2], &rawItems); err != nil {
		return nil, fmt.Errorf("unexpected wgZjtc response shape: %s", raw)
	}
	return parseLibraryItems(rawItems), nil
}

// looksLikeHTML reports whether body looks like it starts with an HTML
// document rather than binary file content.
func looksLikeHTML(body []byte) bool {
	head := bytes.TrimSpace(body)
	if len(head) > 512 {
		head = head[:512]
	}
	head = bytes.ToLower(head)
	return bytes.HasPrefix(head, []byte("<!doctype")) || bytes.HasPrefix(head, []byte("<html"))
}

// fetchRawCompanionOriginal fetches the true original bytes for a RAW/DNG
// companion item directly from its own base URL, using suffix "=s0-d-I" —
// confirmed against captured traffic to return the actual stored file.
// downloadOriginal's "=d"/"=dv" is Google's normal preview/render suffix
// and, for a RAW item, isn't known to return the literal RAW bytes, so this
// doesn't reuse it. A DNG response comes back as application/octet-stream
// rather than image/*, so this also doesn't reuse downloadOriginal's
// image/video content-type whitelist — instead it relies on looksLikeHTML,
// same as everywhere else in this file, to catch an auth redirect rather
// than a real file.
func fetchRawCompanionOriginal(ctx context.Context, c *http.Client, item libraryItem, filename, destDir string) error {
	if item.BaseURL == "" {
		return fmt.Errorf("no base url for %s", item.MediaKey)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, item.BaseURL+"=s0-d-I", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Referer", "https://photos.google.com/")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("companion download returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if looksLikeHTML(body) {
		return fmt.Errorf("companion download returned an HTML page instead of file bytes (likely an auth redirect), content-type %q", resp.Header.Get("Content-Type"))
	}
	return writeAtomically(destDir, filename, bytes.NewReader(body))
}

// downloadCompanions looks for a RAW/DNG companion of item — a Pixel
// RAW+JPEG pair's "cover" JPEG, identified by coverFilename (its bare
// resolved name, not a destDir-relative path — see targetName) matching
// rawCoverRe — and downloads every companion found. subdir places
// companions in the same date subdirectory as the cover (see dateSubdir),
// rather than each companion's own capture time, so a RAW+JPEG pair can
// never end up split across two folders over a timestamp difference of a
// few hundred milliseconds around a month boundary. used is the same
// filename-collision map Mirror's loop already threads through
// resolveFilename. Returns the companion filenames written, so the caller
// can record them alongside the cover in the index; nil, nil if item isn't
// part of a recognized RAW+JPEG group.
func downloadCompanions(ctx context.Context, c *http.Client, gd *globalData, item libraryItem, coverFilename, subdir, destDir string, used map[string]bool) ([]string, error) {
	m := rawCoverRe.FindStringSubmatch(coverFilename)
	if m == nil {
		return nil, nil
	}
	groupID := m[1]

	siblings, err := getRawGroup(ctx, c, gd, groupID)
	if err != nil {
		return nil, fmt.Errorf("resolving RAW group %s: %w", groupID, err)
	}
	var companions []libraryItem
	for _, s := range siblings {
		if s.MediaKey != item.MediaKey {
			companions = append(companions, s)
		}
	}
	if len(companions) == 0 {
		return nil, nil
	}

	keys := make([]string, len(companions))
	for i, s := range companions {
		keys[i] = s.MediaKey
	}
	names, err := batchFilenames(ctx, c, gd, keys)
	if err != nil {
		return nil, fmt.Errorf("resolving companion filenames: %w", err)
	}

	written := make([]string, 0, len(companions))
	for _, s := range companions {
		fn, alreadyOnDisk := resolveFilename(destDir, targetName(subdir, names[s.MediaKey], s.MediaKey), s.MediaKey, used)
		if !alreadyOnDisk {
			if err := fetchRawCompanionOriginal(ctx, c, s, fn, destDir); err != nil {
				return written, fmt.Errorf("downloading companion %s: %w", s.MediaKey, err)
			}
		}
		written = append(written, fn)
	}
	return written, nil
}

// removeStaleTempFiles deletes any leftover "*.gphotos-tmp" files anywhere
// under destDir (files now live under per-item date subdirectories — see
// dateSubdir — so this has to walk, not just glob the top level): downloads
// interrupted before their rename to a final name could happen. They're
// always safe to delete — a media key only gets recorded in the local index
// after that rename succeeds, so nothing depends on a .gphotos-tmp file's
// contents, and leaving them around forever would just be clutter (and, if
// their name happened to already sit in usedNames, unnecessary
// disambiguation pressure on genuinely new items).
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

// ---- local download index: skip items already mirrored on a prior run ----

// indexFileName is written inside destDir. It's namespaced to this backend
// since destDir is generally dedicated to one mirror.Method, but nothing
// stops two methods from sharing a directory.
const indexFileName = ".gphotos-index.json"

// downloadIndex records, per Google Photos media key, that an item has
// already been downloaded into destDir — so a later run can skip it instead
// of re-fetching. It's keyed by MediaKey rather than filename since
// resolveFilename may rename an item to avoid a collision.
//
// The index is the source of truth for "already downloaded", not the
// filesystem: a run never re-checks whether the recorded file is still on
// disk. If you delete a file after it's been mirrored, it will NOT come back
// on a later run — the index still says it's done. Delete the index (or the
// specific entry) if you want an item re-downloaded.
type downloadIndex struct {
	path    string
	entries map[string]indexEntry
}

// An item normally downloads as a single file, recorded in Filename alone.
// One with a RAW/DNG companion (see downloadCompanions) additionally
// populates ExtraFilenames, so both files are tracked under the cover
// item's one media key and both stay protected from future name collisions
// (see Mirror's usedNames seeding).
type indexEntry struct {
	Filename       string   `json:"filename"`
	ExtraFilenames []string `json:"extra_filenames,omitempty"`
	TimestampMS    int64    `json:"timestamp_ms"`
}

// filenames returns every file this entry recorded, primary first.
func (e indexEntry) filenames() []string {
	if e.Filename == "" {
		return nil
	}
	return append([]string{e.Filename}, e.ExtraFilenames...)
}

func loadDownloadIndex(destDir string) (*downloadIndex, error) {
	idx := &downloadIndex{path: filepath.Join(destDir, indexFileName), entries: map[string]indexEntry{}}
	data, err := os.ReadFile(idx.path)
	if errors.Is(err, os.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &idx.entries); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", idx.path, err)
	}
	return idx, nil
}

func (idx *downloadIndex) has(mediaKey string) bool {
	_, ok := idx.entries[mediaKey]
	return ok
}

// record stores every filename an item downloaded as, primary first.
func (idx *downloadIndex) record(mediaKey string, filenames []string, timestampMS int64) {
	e := indexEntry{TimestampMS: timestampMS}
	if len(filenames) > 0 {
		e.Filename = filenames[0]
		e.ExtraFilenames = filenames[1:]
	}
	idx.entries[mediaKey] = e
}

// save writes the index atomically (write-then-rename) so a process killed
// mid-write can't leave a corrupt index behind.
func (idx *downloadIndex) save() error {
	data, err := json.MarshalIndent(idx.entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := idx.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, idx.path)
}

// ---- date-based subdirectories (destDir/YYYY/MM/filename) ----

// dateSubdir returns the "YYYY/MM" subdirectory (using the OS path
// separator) an item's files are organized under, based on the date it was
// actually taken in ITS OWN local time — not the UTC instant — using its
// TimezoneOffsetMS. A photo taken at 11pm local time but past midnight UTC
// would otherwise land in tomorrow's folder.
func dateSubdir(it libraryItem) string {
	local := time.UnixMilli(it.TimestampMS).UTC().Add(time.Duration(it.TimezoneOffsetMS) * time.Millisecond)
	return filepath.Join(fmt.Sprintf("%04d", local.Year()), fmt.Sprintf("%02d", int(local.Month())))
}

// targetName builds the destDir-relative path (subdirectory + filename) an
// item should be saved under, falling back to its media key if resolvedName
// (from batchFilenames) came back empty.
func targetName(subdir, resolvedName, mediaKey string) string {
	base := resolvedName
	if base == "" {
		base = mediaKey
	}
	return filepath.Join(subdir, base)
}

// ---- Mirror: walk the library newest-first, stop once older than after ----

// Mirror downloads Google Photos items into destDir, using the session
// captured in the cookies.txt at cookiesPath. source is the account's email
// — it isn't used to select the account (that's determined entirely by whose
// session the cookies belong to), only for logging, so it's fine to pass
// whatever human-readable label identifies the account to you.
//
// after and before are YYYY-MM-DD date strings restricting which items (by
// taken date) get downloaded; either may be empty, meaning no lower bound
// (after) or up to now (before).
//
// tryCompanions, if true, additionally looks for each item's RAW/DNG
// companion (see downloadCompanions) after downloading it normally. On by
// default, confirmed working against a real account.
func Mirror(ctx context.Context, source, destDir, cookiesPath, after, before string, tryCompanions bool) error {
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

	jar, _ := cookiejar.New(nil)
	if err := loadCookies(jar, cookiesPath); err != nil {
		return fmt.Errorf("loading cookies: %w", err)
	}
	client := &http.Client{Jar: jar, Timeout: 60 * time.Second}
	logCookieStatus(client, "cookies loaded from "+cookiesPath)

	gd, err := fetchGlobalData(ctx, client)
	if err != nil {
		// The short-lived __Secure-1PSIDTS/__Secure-3PSIDTS can already be
		// stale by the time this runs — even moments after export, if
		// export-to-run took long enough — well before sessionRefreshInterval
		// ever gets a chance to kick in. If the long-lived account cookies
		// (SID, SAPISID, ...) are still good, a rotation can mint a fresh
		// short-lived one and recover without making the user re-export.
		log.Printf("gphotos: initial session bootstrap failed (%v), trying a cookie rotation before giving up", err)
		if rotErr := rotateCookies(ctx, client, rotateCookiesURL); rotErr != nil {
			return fmt.Errorf("session bootstrap failed: %w (rotation also failed: %v)", err, rotErr)
		}
		gd, err = fetchGlobalData(ctx, client)
		if err != nil {
			return fmt.Errorf("session bootstrap failed even after rotating cookies: %w", err)
		}
	}

	idx, err := loadDownloadIndex(destDir)
	if err != nil {
		return fmt.Errorf("loading download index: %w", err)
	}
	if err := removeStaleTempFiles(destDir); err != nil {
		log.Printf("gphotos: cleaning up stale temp files: %v", err)
	}

	log.Printf("gphotos: mirroring %s into %s", source, destDir)

	firstTS := beforeT.UnixMilli()
	startTS := &firstTS
	afterTS := afterT.UnixMilli()

	var pageID *string
	total, adopted, alreadyHave, skipped := 0, 0, 0, 0
	// Seeded with every filename the index already knows about, so
	// resolveFilename can tell "this name belongs to some other,
	// already-known item" (needs disambiguating) apart from "nothing knows
	// about this name yet, but it's on disk" (adopt it as this item's own).
	usedNames := map[string]bool{}
	for _, e := range idx.entries {
		for _, fn := range e.filenames() {
			usedNames[fn] = true
		}
	}
	lastRefresh := time.Now() // session (incl. gd) is already fresh from the bootstrap above
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		if time.Since(lastRefresh) > sessionRefreshInterval {
			log.Printf("gphotos: refreshing session (%s since last refresh)", time.Since(lastRefresh).Round(time.Second))
			if freshGD, err := refreshSession(ctx, client); err != nil {
				log.Printf("gphotos: session refresh failed, continuing with the existing session: %v", err)
			} else {
				gd = freshGD
			}
			lastRefresh = time.Now()
		}

		items, next, err := listPage(ctx, client, gd, startTS, pageID)
		if err != nil {
			return fmt.Errorf("list page: %w", err)
		}
		if len(items) == 0 {
			break
		}

		// Stop bound applies to every item on the page regardless of
		// already-downloaded status, so it has to be checked before
		// filtering down to newItems.
		stop := false
		var newItems []libraryItem
		for _, it := range items {
			if it.TimestampMS < afterTS {
				stop = true
				break // items are newest-first; once we're past "after", we're done
			}
			if idx.has(it.MediaKey) {
				alreadyHave++
				continue
			}
			newItems = append(newItems, it)
		}

		if len(newItems) > 0 {
			// keys, batched, for filename resolution — only for items we
			// don't already have, so a mostly-synced re-run doesn't spend an
			// RPC resolving filenames it's just going to skip.
			keys := make([]string, len(newItems))
			for i, it := range newItems {
				keys[i] = it.MediaKey
			}
			names, err := batchFilenames(ctx, client, gd, keys)
			if err != nil {
				log.Printf("gphotos: filename lookup failed, falling back to media_key: %v", err)
				names = map[string]string{}
			}

			for _, it := range newItems {
				subdir := dateSubdir(it)
				baseName := names[it.MediaKey]
				fn, alreadyOnDisk := resolveFilename(destDir, targetName(subdir, baseName, it.MediaKey), it.MediaKey, usedNames)
				if alreadyOnDisk {
					log.Printf("gphotos: adopting existing %s into index (%s)", fn, time.UnixMilli(it.TimestampMS).Format(time.RFC3339))
					idx.record(it.MediaKey, []string{fn}, it.TimestampMS)
					adopted++
				} else {
					log.Printf("gphotos: downloading %s (%s)", fn, time.UnixMilli(it.TimestampMS).Format(time.RFC3339))
					if err := downloadOriginal(ctx, client, it, fn, destDir); err != nil {
						log.Printf("gphotos:   skip %s: %v", it.MediaKey, err)
						skipped++
						continue
					}
					filenames := []string{fn}
					if isVideoFilename(fn) {
						if thumbFn, err := downloadVideoThumbnail(ctx, client, it, fn, destDir, usedNames); err != nil {
							log.Printf("gphotos:   thumbnail fetch failed for %s: %v", it.MediaKey, err)
						} else {
							log.Printf("gphotos:   saved thumbnail %s", thumbFn)
							filenames = append(filenames, thumbFn)
						}
					}
					if tryCompanions {
						extra, err := downloadCompanions(ctx, client, gd, it, baseName, subdir, destDir, usedNames)
						if err != nil {
							log.Printf("gphotos:   companion lookup failed for %s: %v", it.MediaKey, err)
						} else if len(extra) > 0 {
							log.Printf("gphotos:   %s came with %d companion file(s): %v", it.MediaKey, len(extra), extra)
							filenames = append(filenames, extra...)
						}
					}
					idx.record(it.MediaKey, filenames, it.TimestampMS)
					total++
				}

				// Save after every item, not just at the end of a page, so
				// a run interrupted mid-page doesn't lose credit for what
				// it already downloaded and re-fetch it next time.
				if err := idx.save(); err != nil {
					log.Printf("gphotos: saving download index: %v", err)
				}
			}
		}

		if stop || next == nil {
			break
		}
		pageID = next
		startTS = nil // only the first page needs the explicit start timestamp
	}

	log.Printf("gphotos: done: %d downloaded, %d adopted from disk, %d already had, %d skipped", total, adopted, alreadyHave, skipped)
	return nil
}
