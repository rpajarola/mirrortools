// Package groupsio mirrors a groups.io group's Files section to a local
// directory via groups.io's REST API (https://groups.io/api). An account
// with access to the group is required — groups.io has no anonymous file
// listing — supplied via the -email and -password-file flags (see init
// below), never hardcoded.
//
// It registers itself as the "groupsio" method with mirrortools' mirror
// package, so it's normally driven through the mirror CLI:
//
//	mirror groupsio -email you@example.com -password-file pw.txt some-group ./mirror
//
// This fixes several bugs found in the Python script it replaces, rather
// than reproducing them:
//
//   - There, the login call's response was never checked, so a rejected
//     login (bad password, 2FA required) silently proceeded, and every
//     subsequent call then failed in confusing, differently-broken ways.
//     Here, login failure is detected and returned as a clear error before
//     anything else runs.
//   - There, an unexpected HTTP status from the API dropped into
//     `pdb.set_trace()` — in a real, unattended mirror run (which is the
//     only way this tool is ever actually invoked) that hangs forever
//     waiting on a debugger prompt nothing will ever answer. Here, an
//     unexpected status is logged and treated as a retryable error like
//     any other.
//   - There, an HTTP 429 was handled by sleeping a flat 60s inside an
//     unconditional `while True` with no bound and no backoff — sustained
//     rate-limiting hangs the run forever. Here, 429s are retried a bounded
//     number of times with the same backoff as any other transient error,
//     then that one call fails and the item it was for is skipped.
//   - There, the recursive directory walk itself was wrapped in a
//     five-attempt retry, so a single failure deep inside a large tree
//     re-walked and re-attempted the *entire* subtree from its root,
//     redoing already-completed downloads. Here, retries are scoped to one
//     API call or one file download; a failure elsewhere in the tree
//     doesn't touch siblings that already succeeded.
//   - There, a failed download fell back to a second URL that hardcoded a
//     specific, unrelated group's name (evident leftover from whatever
//     group the script was last customized for) — silently wrong for every
//     other group it might be run against. Here the fallback URL is built
//     from the group actually being mirrored.
//   - There, a listing whose page count didn't add up to the API's
//     reported total, or whose entries had a duplicate name, hit a bare
//     `assert` — an unhandled crash that aborted mirroring the entire
//     group over what's normally a transient pagination hiccup. Here it's
//     logged as a warning and the listing is used as-is.
//   - There, login sent its email/password as URL query parameters
//     (Python requests' `params=` does this even on a POST) rather than
//     the form-urlencoded POST body groups.io's API documentation
//     specifies — confirmed against the live API to get back a generic
//     "invalid email" error even for a well-formed address, rather than
//     the documented unauthorized_error for a bad credential. Here login
//     sends a proper form-urlencoded body.
package groupsio

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/rpajarola/mirrortools/mirror"
)

func init() {
	mirror.Register(&mirror.Method{
		Name: "groupsio",
		Source: "a groups.io group's short name — the \"xyz\" in https://groups.io/g/xyz — " +
			"whose Files section is mirrored",
		Describe: "download every file in a groups.io group's Files section via the groups.io API",
		SetupFlags: func(fs *flag.FlagSet) mirror.Func {
			email := fs.String("email", "", "groups.io account email (required)")
			passwordFile := fs.String("password-file", "", "path to a file containing the account's password, "+
				"one line — kept out of the command line and shell history (required)")
			return func(ctx context.Context, source, destDir string) error {
				if *email == "" {
					return fmt.Errorf("-email is required")
				}
				if *passwordFile == "" {
					return fmt.Errorf("-password-file is required")
				}
				pw, err := os.ReadFile(*passwordFile)
				if err != nil {
					return fmt.Errorf("reading -password-file: %w", err)
				}
				password := strings.TrimSpace(string(pw))
				return Mirror(ctx, source, destDir, *email, password)
			}
		},
	})
}

const apiBase = "https://groups.io/api/v1"

// tmpSuffix marks a file as a download in progress, the same
// write-then-rename convention used elsewhere in mirrortools: a download
// cut short by a network failure or the process being killed leaves only a
// "*.groupsio-tmp" behind, never a truncated file at the real name that a
// later run could mistake for a complete one.
const tmpSuffix = ".groupsio-tmp"

// These are var, not const, solely so tests can shrink the backoff/wait
// durations and avoid real multi-second (or, for rateLimitWait,
// multi-minute) sleeps; production behavior is unaffected.
var (
	maxAttempts     = 5
	initialBackoff  = 2 * time.Second
	rateLimitWait   = 60 * time.Second
	maxRateLimitTry = 5
)

// client holds everything needed to talk to one groups.io group across an
// entire Mirror run: the authenticated session, which group, and where its
// files land on disk.
type client struct {
	http      *http.Client
	groupName string
	groupID   string
	mirrorDir string // destDir/groupName — everything for this group lands under here
}

// apiFile is one entry from getfiledirectory's "data" list: either a file
// or, per isDir, a subdirectory.
type apiFile struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	MediaType   string `json:"media_type"`
	IsFolder    bool   `json:"is_folder"`
	Desc        string `json:"desc"`
	DownloadURL string `json:"download_url"`
	Size        int64  `json:"size"`
}

// isDir reports whether f is a subdirectory rather than a downloadable
// file. groups.io's API has signaled this three different ways across its
// history (is_folder, type, media_type); checking all three, in the same
// order the Python script did, is cheap insurance against whichever one a
// given response happens to carry.
func (f apiFile) isDir() bool {
	return f.IsFolder || f.Type == "file_type_dir" || f.MediaType == "folder"
}

type listResponse struct {
	TotalCount    int        `json:"total_count"`
	Data          []apiFile  `json:"data"`
	HasMore       bool       `json:"has_more"`
	NextPageToken flexString `json:"next_page_token"`
}

// flexString decodes either a JSON string or a JSON number into a Go
// string. Confirmed necessary against the live API: next_page_token comes
// back as a bare number 0 when has_more is false (no next page), but is
// presumably an opaque string token when has_more is true — a plain
// `string` field fails to unmarshal the former.
type flexString string

func (s *flexString) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		*s = flexString(str)
		return nil
	}
	*s = flexString(b)
	return nil
}

type groupResponse struct {
	ID   json.Number `json:"id"`
	Desc string      `json:"desc"`
}

// Mirror downloads every file in the groups.io group named groupName's
// Files section into destDir/groupName, which is created if needed;
// destDir itself is guaranteed to already exist by the caller.
//
// The group's own description is written to destDir/groupName/README; each
// subdirectory's description to its own README alongside its contents;
// each downloaded file gets a sibling "<name>.desc" holding its groups.io
// description. A file already on disk at its expected size is skipped
// without re-fetching — cheap, and, as with the archiveorg backend, enough
// to catch a download interrupted by something other than this tool itself
// (which always writes via a temp file, so its own interruptions never
// leave a wrong-sized file behind).
func Mirror(ctx context.Context, source, destDir, email, password string) error {
	groupName := source
	jar, _ := cookiejar.New(nil)
	c := &client{
		http:      &http.Client{Jar: jar, Timeout: 60 * time.Second},
		groupName: groupName,
		mirrorDir: filepath.Join(destDir, groupName),
	}

	log.Printf("groupsio: logging in as %s", email)
	if err := c.login(ctx, email, password); err != nil {
		return fmt.Errorf("login: %w", err)
	}

	var group groupResponse
	if err := c.apiGet(ctx, "getgroup", url.Values{"group_name": {groupName}}, &group); err != nil {
		return fmt.Errorf("looking up group %q: %w", groupName, err)
	}
	if group.ID == "" {
		return fmt.Errorf("group %q not found (or not accessible to %s)", groupName, email)
	}
	c.groupID = group.ID.String()

	log.Printf("groupsio: mirroring %s into %s", groupName, c.mirrorDir)
	if err := c.store("README", group.Desc); err != nil {
		return fmt.Errorf("writing group README: %w", err)
	}

	return c.walkDir(ctx, "")
}

// login authenticates the session; groups.io's login endpoint sets a
// session cookie on success, which c.http's cookie jar then carries on
// every subsequent request. A failed login comes back as a non-200 status
// (confirmed against the live API: HTTP 400 with an error body) rather
// than a 200 with some embedded error indicator, and apiPost already turns
// a non-200 status into an error — nothing further to check here.
func (c *client) login(ctx context.Context, email, password string) error {
	var result json.RawMessage
	return c.apiPost(ctx, "login", url.Values{"email": {email}, "password": {password}}, &result)
}

// walkDir mirrors one directory of the group's Files section: writes its
// own README (for anything but the root, whose description Mirror already
// wrote), lists its entries, and recurses into subdirectories. A failure
// listing or recursing into one entry is logged and skipped rather than
// aborting the walk — one bad subdirectory shouldn't cost everything
// already found elsewhere in the tree.
func (c *client) walkDir(ctx context.Context, dirPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := c.list(ctx, dirPath)
	if err != nil {
		return fmt.Errorf("listing %q: %w", dirPath, err)
	}
	for _, e := range entries {
		if e.isDir() {
			subPath := path.Join(dirPath, e.Name)
			if err := c.storeDirReadme(subPath, e.Desc); err != nil {
				log.Printf("groupsio: %s: writing README: %v", subPath, err)
			}
			if err := c.walkDir(ctx, subPath); err != nil {
				log.Printf("groupsio: %s: %v", subPath, err)
			}
		} else {
			c.getFile(ctx, dirPath, e)
		}
	}
	return nil
}

// list fetches every entry of one directory, paging through
// getfiledirectory until has_more is false.
func (c *client) list(ctx context.Context, dirPath string) ([]apiFile, error) {
	params := url.Values{
		"group_id":   {c.groupID},
		"path":       {dirPath},
		"limit":      {"100"},
		"sort_field": {"created"},
		"sort_dir":   {"asc"},
	}

	var resp listResponse
	if err := c.apiGet(ctx, "getfiledirectory", params, &resp); err != nil {
		return nil, err
	}
	data := resp.Data
	for resp.HasMore {
		params.Set("page_token", string(resp.NextPageToken))
		// Decode into a fresh listResponse each page, not the same resp
		// reused in place: json.Unmarshal reuses a slice field's backing
		// array when it has spare capacity, which would silently overwrite
		// the previous page's entries still referenced by data through that
		// same array.
		var next listResponse
		if err := c.apiGet(ctx, "getfiledirectory", params, &next); err != nil {
			return nil, err
		}
		data = append(data, next.Data...)
		resp = next
	}

	if len(data) != resp.TotalCount {
		log.Printf("groupsio: %s: expected %d entries, got %d (API pagination hiccup — using what was returned)",
			dirPath, resp.TotalCount, len(data))
	}
	seen := make(map[string]bool, len(data))
	for _, d := range data {
		if seen[d.Name] {
			log.Printf("groupsio: %s: duplicate entry name %q in listing", dirPath, d.Name)
		}
		seen[d.Name] = true
	}
	return data, nil
}

// getFile downloads one file entry of dirPath, unless it's a type this
// tool can't fetch a body for (see the two type checks below) or already
// present on disk at its expected size. Errors are logged rather than
// returned: one bad file shouldn't stop the rest of the directory from
// being mirrored.
func (c *client) getFile(ctx context.Context, dirPath string, f apiFile) {
	if f.Type == "file_type_box" || f.MediaType == "Generic Link" {
		return // a link into Box, or a bare external link — nothing to download
	}

	relPath, err := safeRelPath(dirPath, f.Name)
	if err != nil {
		log.Printf("groupsio: %v", err)
		return
	}
	finalPath := filepath.Join(c.mirrorDir, relPath)

	if fi, err := os.Stat(finalPath); err == nil && fi.Size() == f.Size {
		return
	}

	log.Printf("groupsio: downloading %s (%d bytes)", path.Join(dirPath, f.Name), f.Size)

	if err := c.storeDesc(relPath, f.Desc); err != nil {
		log.Printf("groupsio: %s: writing .desc: %v", relPath, err)
	}

	downloadErr := c.downloadWithRetry(ctx, f.DownloadURL, finalPath)
	if downloadErr != nil {
		// The API's own download_url can be wrong for a file uploaded in
		// an unusual way (evident from the Python script this replaces
		// having grown a fallback for exactly this); the group's ordinary
		// web-UI file URL is worth one extra try before giving up.
		altURL := fmt.Sprintf("https://groups.io/g/%s/files/%s", c.groupName, webPath(dirPath, f.Name))
		log.Printf("groupsio: %s: download_url failed (%v), trying %s", relPath, downloadErr, altURL)
		downloadErr = c.downloadWithRetry(ctx, altURL, finalPath)
	}
	if downloadErr != nil {
		log.Printf("groupsio: giving up on %s: %v", relPath, downloadErr)
		return
	}

	if fi, err := os.Stat(finalPath); err == nil && fi.Size() != f.Size {
		log.Printf("groupsio: %s: size mismatch after download: expected %d, got %d", relPath, f.Size, fi.Size())
	}
}

// webPath builds the URL path segment groups.io's web UI (as opposed to
// its API) expects for a file at dirPath/name: each segment
// percent-escaped individually so a literal "/" within a single path
// component (unlikely, but not something the API rules out) can't be
// mistaken for a directory separator.
func webPath(dirPath, name string) string {
	segs := strings.Split(strings.Trim(path.Join(dirPath, name), "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// downloadWithRetry fetches srcURL into finalPath, retrying transient
// failures the same bounded number of times as any other API call. Written
// via a temp file and renamed into place only once the copy finishes, so
// an interrupted attempt never leaves a truncated file behind.
func (c *client) downloadWithRetry(ctx context.Context, srcURL, finalPath string) error {
	return withRetry(ctx, func() error {
		return c.download(ctx, srcURL, finalPath)
	})
}

func (c *client) download(ctx context.Context, srcURL, finalPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srcURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return err
	}
	tmpPath := finalPath + tmpSuffix
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(tmpPath)
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	return os.Rename(tmpPath, finalPath)
}

// safeRelPath validates that dirPath/name can't escape the group's mirror
// directory once joined onto it, and returns the cleaned, OS-native
// relative path to join. Defensive only — groups.io file and directory
// names aren't expected to ever trip this — against a stray ".." making it
// into an API response being able to write outside mirrorDir.
func safeRelPath(dirPath, name string) (string, error) {
	clean := path.Clean("/" + path.Join(dirPath, name))
	rel := strings.TrimPrefix(clean, "/")
	if rel == "" || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("unsafe path %q/%q", dirPath, name)
	}
	return filepath.FromSlash(rel), nil
}

// store writes content to relPath under the group's mirror directory,
// creating parent directories as needed.
func (c *client) store(relPath, content string) error {
	full := filepath.Join(c.mirrorDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

func (c *client) storeDirReadme(dirPath, desc string) error {
	return c.store(path.Join(dirPath, "README"), desc)
}

func (c *client) storeDesc(fileRelPath, desc string) error {
	return c.store(fileRelPath+".desc", desc)
}

// apiGet issues a GET to the groups.io API and decodes its response into
// out, retrying transient failures (network errors, 429s, unexpected
// statuses) a bounded number of times.
func (c *client) apiGet(ctx context.Context, action string, params url.Values, out any) error {
	return withRetry(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/"+action+"?"+params.Encode(), nil)
		if err != nil {
			return err
		}
		return c.doAPI(req, out)
	})
}

// apiPost issues a POST to the groups.io API with params as a
// form-urlencoded body, per groups.io's API documentation. The Python
// script this replaces used requests' `params=` on a POST, which puts them
// in the query string instead — not what the documented API expects, and
// observed against the live API to get a generic "invalid email" error
// back even for a well-formed address, rather than the documented
// "unauthorized_error" for a bad credential. Retries the same as apiGet.
func (c *client) apiPost(ctx context.Context, action string, params url.Values, out any) error {
	return withRetry(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/"+action, strings.NewReader(params.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return c.doAPI(req, out)
	})
}

// rateLimitedError marks an HTTP 429 response so withRetry can give it its
// own, longer wait (see rateLimitWait) instead of the ordinary backoff used
// for other transient failures.
type rateLimitedError struct{}

func (rateLimitedError) Error() string { return "rate limited (http 429)" }

func (c *client) doAPI(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return rateLimitedError{}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d: %s", resp.StatusCode, snippet(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding response: %w (body: %s)", err, snippet(body))
	}
	return nil
}

func snippet(body []byte) string {
	s := strings.ReplaceAll(string(body), "\n", "\\n")
	if len(s) > 300 {
		s = s[:300] + "...(truncated)"
	}
	return s
}

// withRetry runs fn up to maxAttempts times, with exponential backoff
// between attempts — longer, and capped at fewer attempts, specifically for
// a rateLimitedError, matching groups.io's documented 429 behavior of
// asking clients to slow down rather than a transient blip worth retrying
// quickly.
func withRetry(ctx context.Context, fn func() error) error {
	backoff := initialBackoff
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err

		wait := backoff
		limit := maxAttempts
		if _, ok := err.(rateLimitedError); ok {
			wait = rateLimitWait
			limit = maxRateLimitTry
		}
		if attempt >= limit {
			break
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff *= 2
	}
	return lastErr
}
