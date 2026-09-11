// Package gphotos downloads original-quality Google Photos in a date range
// by talking directly to the same undocumented batchexecute API the
// photos.google.com web app uses. Ported from
// github.com/xob0t/google_photos_web_client (RPC IDs: lcxiM = list by taken
// date, EWgK9e = batch item info/filenames).
//
// This is NOT the official API. It uses your logged-in browser session
// (cookies), not OAuth. Cookies come from a one-time manual export — see
// loadCookiesFromNetscapeFile. There's no browser/Playwright dependency at
// runtime, only for that initial cookie grab.
//
// It registers itself as the "gphotos" method with mirrortools' mirror
// package, so it's normally driven through the mirror CLI:
//
//	mirror gphotos -cookies cookies.txt -after 2026-09-06 someone@gmail.com ./photos
package gphotos

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
			cookiesPath := fs.String("cookies", "cookies.txt", "Netscape cookies.txt exported after logging into photos.google.com")
			after := fs.String("after", "", "only download items taken on/after this date, YYYY-MM-DD (default: no lower bound)")
			before := fs.String("before", "", "only download items taken on/before this date, YYYY-MM-DD (default: now)")
			return func(ctx context.Context, source, destDir string) error {
				return Mirror(ctx, source, destDir, *cookiesPath, *after, *before)
			}
		},
	})
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

	return parseWrbFr(body, payloadID)
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
		for _, ri := range rawItems {
			var arr []json.RawMessage
			if err := json.Unmarshal(ri, &arr); err != nil || len(arr) < 4 {
				continue
			}
			var mediaKey, dedupKey string
			var thumb []json.RawMessage
			var ts int64
			json.Unmarshal(arr[0], &mediaKey)
			json.Unmarshal(arr[1], &thumb)
			json.Unmarshal(arr[2], &ts)
			json.Unmarshal(arr[3], &dedupKey)
			baseURL := ""
			if len(thumb) > 0 {
				json.Unmarshal(thumb[0], &baseURL)
			}
			items = append(items, libraryItem{MediaKey: mediaKey, BaseURL: baseURL, TimestampMS: ts, DedupKey: dedupKey})
		}
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
		host := strings.TrimPrefix(domain, ".")
		cookie := &http.Cookie{Name: name, Value: value}
		if strings.HasPrefix(domain, ".") {
			// Leading dot in the Netscape format means "this domain and all
			// subdomains" — a domain cookie. Setting Cookie.Domain tells
			// cookiejar to treat it that way; leaving it empty (as before)
			// makes it a host-only cookie good for exactly "google.com" and
			// never sent on requests to photos.google.com, which is why
			// every request looked logged-out no matter how fresh the
			// cookies were.
			cookie.Domain = host
		}
		byHost[host] = append(byHost[host], cookie)
	}
	if err := sc.Err(); err != nil {
		return err
	}

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

func downloadOriginal(ctx context.Context, c *http.Client, item libraryItem, filename, destDir string) error {
	if item.BaseURL == "" {
		return fmt.Errorf("no base url for %s", item.MediaKey)
	}
	// "=d" = original quality photo bytes. Videos need "=dv"; distinguishing
	// them reliably requires the item's feature-map (see parser.py in gpwc —
	// LibraryItem.video_duration) which this minimal port skips. As a cheap
	// fallback, retry with =dv if =d comes back as a non-media content type.
	for _, suffix := range []string{"=d", "=dv"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, item.BaseURL+suffix, nil)
		if err != nil {
			return err
		}
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		ct := resp.Header.Get("Content-Type")
		if resp.StatusCode == 200 && (strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "video/")) {
			defer resp.Body.Close()
			out, err := os.Create(filepath.Join(destDir, filename))
			if err != nil {
				return err
			}
			defer out.Close()
			_, err = io.Copy(out, resp.Body)
			return err
		}
		resp.Body.Close()
	}
	return fmt.Errorf("could not fetch original bytes for %s", item.MediaKey)
}

// ---- local download index: skip items already mirrored on a prior run ----

// indexFileName is written inside destDir. It's namespaced to this backend
// since destDir is generally dedicated to one mirror.Method, but nothing
// stops two methods from sharing a directory.
const indexFileName = ".gphotos-index.json"

// downloadIndex records, per Google Photos media key, that an item has
// already been downloaded into destDir — so a later run can skip it instead
// of re-fetching. It's keyed by MediaKey rather than filename since
// claimFilename may rename an item to avoid a collision.
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

type indexEntry struct {
	Filename    string `json:"filename"`
	TimestampMS int64  `json:"timestamp_ms"`
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

func (idx *downloadIndex) record(mediaKey, filename string, timestampMS int64) {
	idx.entries[mediaKey] = indexEntry{Filename: filename, TimestampMS: timestampMS}
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
func Mirror(ctx context.Context, source, destDir, cookiesPath, after, before string) error {
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
	if err := loadCookiesFromNetscapeFile(jar, cookiesPath); err != nil {
		return fmt.Errorf("loading cookies: %w", err)
	}
	client := &http.Client{Jar: jar, Timeout: 60 * time.Second}

	gd, err := fetchGlobalData(ctx, client)
	if err != nil {
		return fmt.Errorf("session bootstrap failed: %w", err)
	}

	idx, err := loadDownloadIndex(destDir)
	if err != nil {
		return fmt.Errorf("loading download index: %w", err)
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
		usedNames[e.Filename] = true
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
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
				fn, alreadyOnDisk := resolveFilename(destDir, names[it.MediaKey], it.MediaKey, usedNames)
				if alreadyOnDisk {
					log.Printf("gphotos: adopting existing %s into index (%s)", fn, time.UnixMilli(it.TimestampMS).Format(time.RFC3339))
					idx.record(it.MediaKey, fn, it.TimestampMS)
					adopted++
				} else {
					log.Printf("gphotos: downloading %s (%s)", fn, time.UnixMilli(it.TimestampMS).Format(time.RFC3339))
					if err := downloadOriginal(ctx, client, it, fn, destDir); err != nil {
						log.Printf("gphotos:   skip %s: %v", it.MediaKey, err)
						skipped++
						continue
					}
					idx.record(it.MediaKey, fn, it.TimestampMS)
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
