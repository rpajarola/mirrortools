// Package sourceforge mirrors the files area of a SourceForge project
// (https://sourceforge.net/projects/<name>/files/) to a local directory:
// every file in every folder, and nothing else — not the project's summary,
// reviews, support or other site pages around it.
//
// Folders are listed by reading the JSON description of their entries that
// every files-area page embeds for its own scripts (net.sf.files), which
// gives each entry's name, whether it is a folder or a file, and a file's
// md5 checksum, so no HTML table needs to be scraped. Files are fetched
// through downloads.sourceforge.net, which redirects to a download mirror.
//
// It registers itself as the "sourceforge" method with mirrortools' mirror
// package, so it's normally driven through the mirror CLI:
//
//	mirror https://sourceforge.net/projects/some-project/ ./sf
package sourceforge

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rpajarola/mirrortools/internal/version"
	"github.com/rpajarola/mirrortools/mirror"
)

func init() {
	mirror.Register(&mirror.Method{
		Name:     "sourceforge",
		Source:   "a SourceForge project URL (https://sourceforge.net/projects/<name>/), or a folder or file in its files area",
		Describe: "download every file in a SourceForge project's files area",
		SetupFlags: func(fs *flag.FlagSet) mirror.Func {
			wait := fs.Duration("wait", 0, "delay between requests")
			retries := fs.Int("retries", 5, "times to retry a request that failed with 429, a 5xx status, a network error or a bad checksum")
			return func(ctx context.Context, source, destDir string) error {
				return Mirror(ctx, source, destDir, Options{Wait: *wait, Retries: *retries})
			}
		},
		Match: func(source string) bool {
			_, _, err := ParseSource(source)
			return err == nil
		},
	})
}

// The SourceForge site, whose files-area pages list folders, and the host
// that redirects file downloads to a mirror. Vars so tests can point them
// at a local server.
var (
	siteBase     = "https://sourceforge.net"
	downloadBase = "https://downloads.sourceforge.net"
)

// userAgent identifies this tool to SourceForge.
var userAgent = "mirrortools/" + version.String() + " (+https://github.com/rpajarola/mirrortools)"

// projectRe matches a project's URLs on the SourceForge site, capturing the
// project name and whatever follows it.
var projectRe = regexp.MustCompile(`^/(?:projects|p)/([A-Za-z0-9][A-Za-z0-9._-]*)(?:/(.*))?$`)

// downloadRe matches a downloads.sourceforge.net file or folder path.
var downloadRe = regexp.MustCompile(`^/project/([A-Za-z0-9][A-Za-z0-9._-]*)(?:/(.*))?$`)

// ParseSource splits a SourceForge URL into the project name and the path
// of a folder or file within the project's files area, "" for all of it.
// It accepts project pages (https://sourceforge.net/projects/<name>/...,
// also /p/<name>/ and sf.net), files-area folder and file URLs (with or
// without a trailing /download), and downloads.sourceforge.net URLs. Any
// project page outside the files area means the whole files area.
func ParseSource(source string) (project, filePath string, err error) {
	u, err := url.Parse(source)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", fmt.Errorf("not a SourceForge URL: %q", source)
	}
	host := strings.ToLower(u.Hostname())
	var m []string
	switch host {
	case "sourceforge.net", "www.sourceforge.net", "sf.net", "www.sf.net":
		m = projectRe.FindStringSubmatch(u.Path)
		if m == nil {
			break
		}
		rest, ok := strings.CutPrefix(m[2], "files/")
		if !ok || !strings.HasPrefix(u.Path, "/projects/") {
			return m[1], "", nil
		}
		m[2] = strings.TrimSuffix(strings.TrimSuffix(rest, "/"), "/download")
	case "downloads.sourceforge.net", "downloads.sf.net":
		m = downloadRe.FindStringSubmatch(u.Path)
	}
	if m == nil {
		return "", "", fmt.Errorf("not a SourceForge project URL: %q", source)
	}
	filePath = strings.Trim(m[2], "/")
	for _, elem := range strings.Split(filePath, "/") {
		if elem == "." || elem == ".." {
			return "", "", fmt.Errorf("unsafe path in %q", source)
		}
	}
	return m[1], filePath, nil
}

// escapePath escapes each "/"-separated element of p for use in a URL.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// listURL is the files-area page of the folder dir ("" for the top) of
// project.
func listURL(project, dir string) string {
	u := siteBase + "/projects/" + url.PathEscape(project) + "/files/"
	if dir != "" {
		u += escapePath(dir) + "/"
	}
	return u
}

// downloadURL is where the file at filePath of project is downloaded from:
// downloads.sourceforge.net, which redirects to a mirror that has it. Go's
// http.Client follows the redirect.
func downloadURL(project, filePath string) string {
	return downloadBase + "/project/" + url.PathEscape(project) + "/" + escapePath(filePath)
}

// entry is one file or folder in a files-area listing, as described by the
// net.sf.files object a listing page embeds.
type entry struct {
	Name string `json:"name"`
	// Type is "d" for a folder and "f" for a file.
	Type string `json:"type"`
	// Link, if set, is an external URL the entry points to instead of a
	// file hosted on SourceForge.
	Link string `json:"link"`
	MD5  string `json:"md5"`
	// Downloadable is false for files that can't be downloaded yet, such
	// as ones staged for a later release.
	Downloadable bool `json:"downloadable"`
}

// filesMarker introduces the JSON object describing a listing's entries in
// a files-area page.
var filesMarker = []byte("net.sf.files = ")

// errNoListing means a page didn't contain a listing of files.
var errNoListing = errors.New("no file listing in the page")

// parseListing extracts the entries of a files-area page, sorted by name.
func parseListing(page []byte) ([]entry, error) {
	i := bytes.Index(page, filesMarker)
	if i < 0 {
		return nil, errNoListing
	}
	var byName map[string]entry
	if err := json.NewDecoder(bytes.NewReader(page[i+len(filesMarker):])).Decode(&byName); err != nil {
		return nil, fmt.Errorf("decoding the file listing: %w", err)
	}
	entries := make([]entry, 0, len(byName))
	for _, e := range byName {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// errNotFolder means a files-area path is a file, not a folder: SourceForge
// redirects a file's page to its download.
var errNotFolder = errors.New("not a folder")

// errNotFound means a files-area page doesn't exist.
var errNotFound = errors.New("not found")

// get sends a GET request for u. Failures that are worth trying again —
// the request not getting through, 429 Too Many Requests, or a 5xx status
// — come back as a *retryableError. With follow false, the request is for
// a files-area page: a redirect is returned as errNotFolder, and a 404 as
// errNotFound.
func get(ctx context.Context, c *http.Client, u string, follow bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	if !follow {
		cc := *c
		cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		c = &cc
	}
	resp, err := c.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &retryableError{err: err}
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	resp.Body.Close()
	if !follow && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, errNotFolder
	}
	if !follow && resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	err = fmt.Errorf("http %d", resp.StatusCode)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, &retryableError{err: err, after: retryAfter(resp.Header.Get("Retry-After"), time.Now())}
	}
	return nil, err
}

// fetchListing lists the folder dir ("" for the top) of project's files
// area.
func fetchListing(ctx context.Context, c *http.Client, project, dir string) ([]entry, error) {
	resp, err := get(ctx, c, listURL(project, dir), false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	page, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &retryableError{err: err}
	}
	return parseListing(page)
}

// localPath turns a files-area path into the OS-native path relative to
// the project's directory, rejecting any that could escape it. Defensive
// only: entry names come from SourceForge, which isn't expected to send a
// name containing "/" or one that is "..".
func localPath(filePath string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(filePath))
	if !filepath.IsLocal(clean) {
		return "", fmt.Errorf("unsafe file name %q", filePath)
	}
	return clean, nil
}

// fileMD5 returns the hex md5 of the file at p.
func fileMD5(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// alreadyHave reports whether finalPath holds the file a previous run
// downloaded: present and, if the listing gave a checksum, matching it, so
// a file that changed on SourceForge since is fetched again.
func alreadyHave(finalPath, wantMD5 string) bool {
	fi, err := os.Stat(finalPath)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if wantMD5 == "" {
		return true
	}
	got, err := fileMD5(finalPath)
	return err == nil && strings.EqualFold(got, wantMD5)
}

// tmpSuffix marks a file as a download in progress: a download cut short
// leaves only a "*.sourceforge-tmp" behind, never a truncated file at the
// real name.
const tmpSuffix = ".sourceforge-tmp"

// downloadFile fetches srcURL into finalPath, verifying the result against
// wantMD5 if non-empty, and sets its modification time to the response's
// Last-Modified, when SourceForge says the file was uploaded. It writes to
// a temp file first and renames it into place only once complete and
// verified.
func downloadFile(ctx context.Context, c *http.Client, srcURL, finalPath, wantMD5 string) error {
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return err
	}
	resp, err := get(ctx, c, srcURL, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if wantMD5 == "" && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") &&
		!strings.HasSuffix(resp.Request.URL.Path, ".html") && !strings.HasSuffix(resp.Request.URL.Path, ".htm") {
		// Without a checksum to catch it, make sure SourceForge didn't
		// answer with a web page (an interstitial, or an error page with a
		// 200 status) rather than the file.
		return fmt.Errorf("got a web page from %s instead of the file", resp.Request.URL.Redacted())
	}

	tmpPath := finalPath + tmpSuffix
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	h := md5.New()
	_, copyErr := io.Copy(io.MultiWriter(out, h), resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(tmpPath)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &retryableError{err: copyErr}
	}
	if closeErr != nil {
		os.Remove(tmpPath)
		return closeErr
	}
	if wantMD5 != "" {
		if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, wantMD5) {
			os.Remove(tmpPath)
			// Most likely corrupted in transit, or a bad mirror, so worth
			// another try.
			return &retryableError{err: fmt.Errorf("md5 mismatch: got %s, want %s", got, wantMD5)}
		}
	}
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		if err := os.Chtimes(tmpPath, t, t); err != nil {
			os.Remove(tmpPath)
			return err
		}
	}
	return os.Rename(tmpPath, finalPath)
}

// retryableError is a failure that may go away if the request is made
// again later: after the server's Retry-After, if it sent one.
type retryableError struct {
	err   error
	after time.Duration
}

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// retryAfter parses a Retry-After header, either seconds or an HTTP date,
// into how long to wait from now. It returns 0 if there is none.
func retryAfter(h string, now time.Time) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil {
		return max(time.Duration(secs)*time.Second, 0)
	}
	if t, err := http.ParseTime(h); err == nil {
		return max(t.Sub(now), 0)
	}
	return 0
}

// The delays between retries: retryBase doubled with every attempt, and a
// cap on that and on a server's Retry-After. Vars so tests can shrink them.
var (
	retryBase     = 2 * time.Second
	maxRetryDelay = 10 * time.Minute
)

// pacer runs requests one after the other, at least wait apart, retrying
// those that fail with a *retryableError.
type pacer struct {
	wait    time.Duration
	retries int
	last    time.Time // when the previous request finished
}

// do runs fn, a single request described by what, until it succeeds, fails
// for good, or has been retried p.retries times.
func (p *pacer) do(ctx context.Context, what string, fn func() error) error {
	for attempt := 0; ; attempt++ {
		if !p.last.IsZero() {
			if err := sleep(ctx, time.Until(p.last.Add(p.wait))); err != nil {
				return err
			}
		}
		err := fn()
		p.last = time.Now()
		var re *retryableError
		if err == nil || !errors.As(err, &re) || attempt >= p.retries {
			return err
		}
		delay := min(retryBase<<attempt, maxRetryDelay)
		if re.after > 0 {
			delay = min(re.after, maxRetryDelay)
		}
		log.Printf("sourceforge: %s: %v; retrying in %v", what, err, delay)
		if err := sleep(ctx, delay); err != nil {
			return err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Options control a Mirror run.
type Options struct {
	// Wait is the delay between requests.
	Wait time.Duration
	// Retries is how many times a request is tried again after a 429, a
	// 5xx status, a network error or a bad checksum, waiting longer each
	// time, or as long as the server's Retry-After says.
	Retries int
}

// mirrorer holds the state of one Mirror run.
type mirrorer struct {
	client     *http.Client
	pacer      *pacer
	project    string
	projectDir string

	downloaded, skipped int
	failed              []string
}

// Mirror downloads every file in the files area of the SourceForge project
// that source names (see ParseSource) into destDir/<project>, keeping the
// files area's folder structure. If source names a folder or file within
// the files area, only that is mirrored, still at its place under
// destDir/<project>.
//
// Files already present from an earlier run are skipped if they still match
// the checksum SourceForge lists for them; the others are downloaded, their
// checksums verified, and their modification times set to when they were
// uploaded. A file or folder that fails doesn't stop the run: the rest is
// still mirrored, and Mirror then returns an error naming what failed, so
// that running it again picks those up.
func Mirror(ctx context.Context, source, destDir string, opts Options) error {
	project, root, err := ParseSource(source)
	if err != nil {
		return err
	}
	if _, err := localPath(path.Join(project, root)); err != nil {
		return err
	}

	// A flat http.Client.Timeout would also cap how long reading a large
	// file may take; ResponseHeaderTimeout only gives up on a server that
	// never starts to answer.
	transport := http.DefaultTransport
	if t, ok := transport.(*http.Transport); ok {
		t = t.Clone()
		t.ResponseHeaderTimeout = 10 * time.Minute
		transport = t
	}
	m := &mirrorer{
		client:     &http.Client{Transport: transport},
		pacer:      &pacer{wait: opts.Wait, retries: opts.Retries},
		project:    project,
		projectDir: filepath.Join(destDir, project),
	}

	what := project
	if root != "" {
		what += " under /" + root
	}
	log.Printf("sourceforge: mirroring the files of %s into %s", what, m.projectDir)
	if err := m.mirrorRoot(ctx, root); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	log.Printf("sourceforge: done: %d downloaded, %d already present, %d failed", m.downloaded, m.skipped, len(m.failed))
	if len(m.failed) > 0 {
		return fmt.Errorf("%d failed: %s", len(m.failed), strings.Join(m.failed, ", "))
	}
	return nil
}

// mirrorRoot mirrors root, the folder or file of the files area that the
// source names.
func (m *mirrorer) mirrorRoot(ctx context.Context, root string) error {
	entries, err := m.list(ctx, root)
	if errors.Is(err, errNotFolder) && root != "" {
		// A file: find its entry in the folder that holds it.
		dir, name := path.Split(root)
		dir = strings.TrimSuffix(dir, "/")
		siblings, err := m.list(ctx, dir)
		if err != nil {
			return fmt.Errorf("listing /%s: %w", dir, err)
		}
		for _, e := range siblings {
			if e.Name == name && e.Type == "f" {
				m.file(ctx, dir, e)
				return nil
			}
		}
		return fmt.Errorf("/%s: no such file or folder", root)
	}
	if errors.Is(err, errNotFound) && root != "" {
		return fmt.Errorf("/%s: no such file or folder", root)
	}
	if err != nil {
		if root == "" {
			return fmt.Errorf("listing the files of %s (does the project exist?): %w", m.project, err)
		}
		return fmt.Errorf("listing /%s: %w", root, err)
	}
	if len(entries) == 0 {
		log.Printf("sourceforge: /%s: no files", root)
	}
	m.folder(ctx, root, entries)
	return nil
}

// list lists the folder dir, retrying as needed.
func (m *mirrorer) list(ctx context.Context, dir string) ([]entry, error) {
	var entries []entry
	err := m.pacer.do(ctx, "listing /"+dir, func() (err error) {
		entries, err = fetchListing(ctx, m.client, m.project, dir)
		return err
	})
	return entries, err
}

// folder mirrors the entries of the folder dir, recursing into subfolders.
func (m *mirrorer) folder(ctx context.Context, dir string, entries []entry) {
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if e.Name == "" || e.Name == "." || e.Name == ".." || strings.ContainsAny(e.Name, `/\`) {
			m.fail(path.Join(dir, e.Name), fmt.Errorf("unsafe name %q", e.Name))
			continue
		}
		p := path.Join(dir, e.Name)
		switch e.Type {
		case "d":
			sub, err := m.list(ctx, p)
			if err != nil {
				m.fail(p+"/", fmt.Errorf("listing: %w", err))
				continue
			}
			m.folder(ctx, p, sub)
		case "f":
			m.file(ctx, dir, e)
		default:
			log.Printf("sourceforge: /%s: skipping entry of unknown type %q", p, e.Type)
		}
	}
}

// file mirrors e, a file in the folder dir.
func (m *mirrorer) file(ctx context.Context, dir string, e entry) {
	p := path.Join(dir, e.Name)
	switch {
	case e.Link != "":
		log.Printf("sourceforge: /%s: skipping link to %s", p, e.Link)
		return
	case !e.Downloadable:
		log.Printf("sourceforge: /%s: skipping, not downloadable (staged?)", p)
		return
	}
	rel, err := localPath(p)
	if err != nil {
		m.fail(p, err)
		return
	}
	finalPath := filepath.Join(m.projectDir, rel)
	if alreadyHave(finalPath, e.MD5) {
		m.skipped++
		return
	}
	log.Printf("sourceforge: downloading /%s", p)
	err = m.pacer.do(ctx, "/"+p, func() error {
		return downloadFile(ctx, m.client, downloadURL(m.project, p), finalPath, e.MD5)
	})
	if err != nil {
		m.fail(p, err)
		return
	}
	m.downloaded++
}

// fail records that the file or folder p couldn't be mirrored, and carries
// on with the rest.
func (m *mirrorer) fail(p string, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	log.Printf("sourceforge: /%s: %v", p, err)
	m.failed = append(m.failed, "/"+p)
}
