// Package httpmirror recursively mirrors everything under a path of an
// HTTP(S) server to a local directory, keeping the server's directory
// structure.
//
// It registers itself as the "http" method with mirrortools' mirror
// package, so it's normally driven through the mirror CLI:
//
//	mirror http https://example.com/pub/ ./mirror
//
// The crawl is scoped to the start URL's directory: links (<a href>,
// <frame src>, redirects, ...) are only followed if they stay on the same
// scheme and host and under that path. The one exception is page
// requisites — images, stylesheets, scripts, icons, and whatever a fetched
// stylesheet itself references — which are fetched wherever they live, so
// a mirrored page still renders.
//
// Server-generated directory listings (Apache, nginx, lighttpd, IIS,
// Python's http.server, Go's http.FileServer, Tomcat, Jetty, ...) are
// recognized and only used to discover the files they list: their HTML
// isn't saved, and their column-sorting links aren't followed.
//
// Crawl state lives in a SQLite database in destDir, so an interrupted run
// resumes where it left off, and a re-run doesn't touch what's already been
// mirrored unless -update is given.
package httpmirror

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
	_ "modernc.org/sqlite"

	"github.com/rpajarola/mirrortools/mirror"
)

func init() {
	mirror.Register(&mirror.Method{
		Name:     "http",
		Source:   "an http:// or https:// URL; everything under its directory is mirrored",
		Describe: "recursively mirror a path of an HTTP(S) server, skipping directory listing pages",
		SetupFlags: func(fs *flag.FlagSet) mirror.Func {
			update := fs.Bool("update", false, "re-check everything already mirrored (conditional requests) and pick up new files")
			wait := fs.Duration("wait", 0, "delay between requests")
			return func(ctx context.Context, source, destDir string) error {
				return Mirror(ctx, source, destDir, Options{Update: *update, Wait: *wait})
			}
		},
	})
}

// Options are Mirror's tunables, one per CLI flag.
type Options struct {
	// Update re-queues every URL already handled by an earlier run, so
	// changed files are re-downloaded (unchanged ones cost a 304 only) and
	// directory listings are re-read for new entries. Without it, a re-run
	// only works on what earlier runs didn't finish.
	Update bool
	// Wait is slept before every request but the first.
	Wait time.Duration
}

// dbFileName is the crawl state database, kept in destDir itself — above
// the per-host directories, so it can't collide with a mirrored file.
const dbFileName = ".httpmirror.db"

// tmpSuffix marks a download in progress, the same write-then-rename
// convention used elsewhere in mirrortools: an interrupted download never
// leaves a truncated file at the real name.
const tmpSuffix = ".httpmirror-tmp"

const (
	userAgent   = "mirrortools-httpmirror/1"
	maxAttempts = 3
)

// initialBackoff is the delay before the first in-run retry of a
// transiently failing URL, doubling after each attempt. A var so tests can
// shorten it.
var initialBackoff = 5 * time.Second

// URL states in the database.
const (
	statePending = "pending" // not handled yet
	stateDone    = "done"    // saved, or (listings, redirects) fully processed
	stateError   = "error"   // transient failure (network, 5xx, local I/O); retried on the next run
	stateFailed  = "failed"  // permanent failure (4xx); retried only with -update
)

const schema = `
CREATE TABLE IF NOT EXISTS urls (
	id            INTEGER PRIMARY KEY,
	url           TEXT NOT NULL UNIQUE,
	requisite     INTEGER NOT NULL,          -- 1: only needed as a page requisite, may be out of scope
	state         TEXT NOT NULL DEFAULT 'pending',
	status        INTEGER,                   -- last HTTP status
	error         TEXT,
	etag          TEXT,
	last_modified TEXT
);
CREATE INDEX IF NOT EXISTS urls_state ON urls(state, id);
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// crawler holds the state of one Mirror run.
type crawler struct {
	db       *sql.DB
	client   *http.Client
	destDir  string
	scope    *url.URL // scheme, host and directory path (ending in "/") links must stay within
	opts     Options
	requests int
	errors   int
}

// statusError is a non-retryable HTTP response (a 4xx other than 408/429).
type statusError struct{ code int }

func (e *statusError) Error() string { return fmt.Sprintf("http %d", e.code) }

// localError is a failure on our side (bad local path, disk I/O) that
// retrying the request right away won't fix.
type localError struct{ err error }

func (e *localError) Error() string { return e.err.Error() }
func (e *localError) Unwrap() error { return e.err }

// Mirror recursively downloads everything under source's directory into
// destDir/<host>/<path>, plus the page requisites of every HTML page it
// saves. destDir must exist.
//
// The scope is the directory of source after following any redirects:
// https://host/pub/ and https://host/pub/index.html both mirror /pub/, and
// https://host/pub (which servers typically redirect to /pub/) does too. A
// URL whose path ends in "/" is saved as index.html in that directory.
//
// It returns an error if any URL failed transiently (network errors, 5xx);
// those are retried on the next run. URLs that failed with a 4xx are logged
// and recorded but don't fail the run.
func Mirror(ctx context.Context, source, destDir string, opts Options) error {
	start, err := url.Parse(source)
	if err != nil {
		return fmt.Errorf("parsing %q: %w", source, err)
	}
	if start.Scheme != "http" && start.Scheme != "https" {
		return fmt.Errorf("not an http:// or https:// URL: %q", source)
	}
	if start.Host == "" {
		return fmt.Errorf("no host in %q", source)
	}
	normalize(start)

	db, err := openDB(filepath.Join(destDir, dbFileName))
	if err != nil {
		return err
	}
	defer db.Close()

	// Plenty for a server that's alive at all, without capping how long a
	// large download that's still making progress may take — see the same
	// reasoning in archiveorg.
	transport := http.DefaultTransport
	if t, ok := transport.(*http.Transport); ok {
		t = t.Clone()
		t.ResponseHeaderTimeout = 5 * time.Minute
		transport = t
	}
	c := &crawler{
		db: db,
		client: &http.Client{
			Transport: transport,
			// Redirects are handled as links (see fetch), so their
			// targets are subject to the same scope check and dedup as
			// everything else.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		destDir: destDir,
		opts:    opts,
	}

	root, err := c.resolveStart(ctx, start)
	if err != nil {
		return err
	}
	c.scope = &url.URL{Scheme: root.Scheme, Host: root.Host, Path: dirOf(root.Path)}
	log.Printf("http: mirroring %s into %s", c.scope, destDir)

	reset := `UPDATE urls SET state = 'pending' WHERE state = 'error'`
	if opts.Update {
		reset = `UPDATE urls SET state = 'pending'`
	}
	if _, err := db.ExecContext(ctx, reset); err != nil {
		return err
	}
	if err := c.enqueue(ctx, root, false); err != nil {
		return err
	}

	if err := c.run(ctx); err != nil {
		return err
	}

	var done int
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM urls WHERE state = 'done'`).Scan(&done)
	log.Printf("http: %d requests this run, %d URLs done in total, %d errors", c.requests, done, c.errors)
	if c.errors > 0 {
		return fmt.Errorf("%d URLs failed, re-run to retry them", c.errors)
	}
	return nil
}

func openDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	// One connection: the crawl is sequential, and it sidesteps SQLite's
	// locking between connections of the same process.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initializing %s: %w", dbPath, err)
	}
	return db, nil
}

// resolveStart follows source's redirects to find the URL the crawl
// actually starts from (and whose directory is the scope). The result is
// remembered in the database, so a re-run doesn't have to ask the server
// again.
func (c *crawler) resolveStart(ctx context.Context, start *url.URL) (*url.URL, error) {
	key := "start:" + start.String()
	var s string
	if err := c.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&s); err == nil {
		return url.Parse(s)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	u := start
	for range 10 {
		req, err := c.newRequest(ctx, u)
		if err != nil {
			return nil, err
		}
		resp, err := c.client.Do(req)
		c.requests++
		if err != nil {
			return nil, fmt.Errorf("fetching %s: %w", u, err)
		}
		resp.Body.Close()
		loc, err := resp.Location()
		if !isRedirect(resp.StatusCode) || err != nil {
			break
		}
		normalize(loc)
		u = loc
	}
	if _, err := c.db.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, key, u.String()); err != nil {
		return nil, err
	}
	return u, nil
}

// run works through pending URLs in the order they were discovered until
// none are left. URLs discovered meanwhile get higher ids, so they're
// picked up by the same loop. A pending non-requisite URL outside the
// current scope (left over from a run with a different start URL into the
// same destDir) is left alone.
func (c *crawler) run(ctx context.Context) error {
	var lastID int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var (
			id        int64
			raw       string
			requisite bool
		)
		err := c.db.QueryRowContext(ctx,
			`SELECT id, url, requisite FROM urls WHERE state = 'pending' AND id > ? ORDER BY id LIMIT 1`,
			lastID).Scan(&id, &raw, &requisite)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		lastID = id

		u, err := url.Parse(raw)
		if err != nil {
			c.setState(ctx, id, stateFailed, 0, err.Error())
			continue
		}
		if !requisite && !c.inScope(u) {
			continue
		}
		if err := c.process(ctx, id, u, requisite); err != nil {
			return err
		}
	}
}

// process fetches one URL, retrying transient failures a bounded number of
// times before recording it as an error and moving on. Only a canceled
// context or a database failure is returned.
func (c *crawler) process(ctx context.Context, id int64, u *url.URL, requisite bool) error {
	backoff := initialBackoff
	for attempt := 1; ; attempt++ {
		err := c.fetch(ctx, id, u, requisite)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var se *statusError
		if errors.As(err, &se) {
			log.Printf("http: %s: %v", u, err)
			return c.setState(ctx, id, stateFailed, se.code, err.Error())
		}
		var le *localError
		if errors.As(err, &le) || attempt == maxAttempts {
			log.Printf("http: giving up on %s: %v", u, err)
			c.errors++
			return c.setState(ctx, id, stateError, 0, err.Error())
		}
		log.Printf("http: %s: %v, retrying in %s", u, err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff *= 2
	}
}

func (c *crawler) newRequest(ctx context.Context, u *url.URL) (*http.Request, error) {
	if c.requests > 0 && c.opts.Wait > 0 {
		select {
		case <-time.After(c.opts.Wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	// Setting this explicitly stops Go's transport from requesting gzip
	// and transparently decompressing it, which would otherwise also
	// decompress e.g. a .tar.gz a server mislabels with
	// Content-Encoding: gzip — the mirror should hold the file's bytes as
	// served.
	req.Header.Set("Accept-Encoding", "identity")
	return req, nil
}

func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// fetch makes one attempt at u, saving it (or, for a directory listing,
// just reading it) and queuing whatever it links to.
func (c *crawler) fetch(ctx context.Context, id int64, u *url.URL, requisite bool) error {
	local, err := c.localPath(u)
	if err != nil {
		return &localError{err}
	}

	req, err := c.newRequest(ctx, u)
	if err != nil {
		return err
	}
	var etag, lastMod sql.NullString
	if err := c.db.QueryRowContext(ctx, `SELECT etag, last_modified FROM urls WHERE id = ?`, id).Scan(&etag, &lastMod); err != nil {
		return err
	}
	if fi, err := os.Stat(local); err == nil && fi.Mode().IsRegular() {
		if etag.String != "" {
			req.Header.Set("If-None-Match", etag.String)
		}
		if lastMod.String != "" {
			req.Header.Set("If-Modified-Since", lastMod.String)
		}
	}

	log.Printf("http: get %s", u)
	resp, err := c.client.Do(req)
	c.requests++
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch code := resp.StatusCode; {
	case code == http.StatusNotModified:
		return c.markDone(ctx, id, code, etag.String, lastMod.String)
	case isRedirect(code):
		loc, err := resp.Location()
		if err != nil {
			return &statusError{code}
		}
		normalize(loc)
		if err := c.enqueue(ctx, loc, requisite); err != nil {
			return err
		}
		return c.markDone(ctx, id, code, "", "")
	case code/100 == 2:
		return c.save(ctx, id, u, resp, local)
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500:
		return fmt.Errorf("http %d", code)
	default:
		return &statusError{code}
	}
}

// save streams a successful response to local (via a temp file), parses
// it for links if it's an in-scope HTML page or a stylesheet, and records
// it as done. A directory listing is parsed but not kept.
func (c *crawler) save(ctx context.Context, id int64, u *url.URL, resp *http.Response, local string) error {
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	isHTML := (mt == "text/html" || mt == "application/xhtml+xml") && c.inScope(u)
	isCSS := mt == "text/css"

	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return &localError{err}
	}
	tmp := local + tmpSuffix
	out, err := os.Create(tmp)
	if err != nil {
		return &localError{err}
	}
	var body bytes.Buffer
	var w io.Writer = out
	if isHTML || isCSS {
		w = io.MultiWriter(out, &body)
	}
	_, copyErr := io.Copy(w, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		os.Remove(tmp)
		return &localError{closeErr}
	}

	var links, reqs []*url.URL
	switch {
	case isHTML:
		var listing bool
		links, reqs, listing = extractHTML(body.Bytes(), u)
		if listing {
			os.Remove(tmp)
			for _, l := range links {
				// Column-sorting links (Apache's ?C=N;O=D, Caddy's
				// ?sort=name&order=asc, ...) just re-list the same
				// directory; real entries never carry a query.
				if l.RawQuery != "" {
					continue
				}
				if err := c.enqueue(ctx, l, false); err != nil {
					return err
				}
			}
			return c.markDone(ctx, id, resp.StatusCode, "", "")
		}
	case isCSS:
		reqs = extractCSS(body.String(), u)
	}

	if err := os.Rename(tmp, local); err != nil {
		os.Remove(tmp)
		return &localError{err}
	}
	lastMod := resp.Header.Get("Last-Modified")
	if t, err := http.ParseTime(lastMod); err == nil {
		os.Chtimes(local, t, t)
	}

	for _, l := range links {
		if err := c.enqueue(ctx, l, false); err != nil {
			return err
		}
	}
	for _, r := range reqs {
		if err := c.enqueue(ctx, r, true); err != nil {
			return err
		}
	}
	return c.markDone(ctx, id, resp.StatusCode, resp.Header.Get("ETag"), lastMod)
}

// enqueue records u as to-be-fetched, unless it's a link (not a requisite)
// leading out of scope. A URL already known keeps its state; one first seen
// as a requisite and now as a link is upgraded to a link.
func (c *crawler) enqueue(ctx context.Context, u *url.URL, requisite bool) error {
	if !requisite && !c.inScope(u) {
		return nil
	}
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO urls(url, requisite) VALUES (?, ?)
		 ON CONFLICT(url) DO UPDATE SET requisite = MIN(requisite, excluded.requisite)`,
		u.String(), requisite)
	return err
}

func (c *crawler) markDone(ctx context.Context, id int64, status int, etag, lastMod string) error {
	_, err := c.db.ExecContext(ctx,
		`UPDATE urls SET state = 'done', status = ?, error = NULL, etag = ?, last_modified = ? WHERE id = ?`,
		status, etag, lastMod, id)
	return err
}

func (c *crawler) setState(ctx context.Context, id int64, state string, status int, msg string) error {
	_, err := c.db.ExecContext(ctx, `UPDATE urls SET state = ?, status = ?, error = ? WHERE id = ?`, state, status, msg, id)
	return err
}

// inScope reports whether u is on the scope's scheme and host and under its
// path.
func (c *crawler) inScope(u *url.URL) bool {
	return u.Scheme == c.scope.Scheme && u.Host == c.scope.Host && strings.HasPrefix(u.Path, c.scope.Path)
}

// localPath maps u to destDir/<host>/<path>. A path ending in "/" gets
// index.html appended; a query string is kept as part of the file name
// ("page.php?id=3"), with any "/" in it escaped.
func (c *crawler) localPath(u *url.URL) (string, error) {
	host := u.Host
	if host == "" || host == "." || host == ".." || strings.ContainsAny(host, `/\`) {
		return "", fmt.Errorf("unusable host %q", host)
	}
	p := u.Path
	if p == "" || strings.HasSuffix(p, "/") {
		p += "index.html"
	}
	if u.RawQuery != "" {
		p += "?" + strings.ReplaceAll(u.RawQuery, "/", "%2F")
	}
	// Cleaning a rooted path drops any ".." that would climb above it.
	p = path.Clean("/" + p)
	if strings.HasSuffix(p, tmpSuffix) {
		return "", fmt.Errorf("path %q collides with temp file naming", u.Path)
	}
	return filepath.Join(c.destDir, host, filepath.FromSlash(p)), nil
}

// dirOf returns p up to and including its last "/".
func dirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i+1]
	}
	return "/"
}

// normalize canonicalizes u in place so the same resource is always stored
// under the same key: lowercase scheme and host, no default port, no
// fragment, "/" for an empty path.
func normalize(u *url.URL) {
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if (u.Scheme == "http" && strings.HasSuffix(u.Host, ":80")) || (u.Scheme == "https" && strings.HasSuffix(u.Host, ":443")) {
		u.Host = u.Host[:strings.LastIndexByte(u.Host, ':')]
	}
	u.Fragment, u.RawFragment = "", ""
	u.User = nil
	if u.Path == "" {
		u.Path, u.RawPath = "/", ""
	}
}

// resolve parses ref relative to base, returning nil for anything that
// isn't a fetchable http(s) URL (mailto:, javascript:, data:, ...).
func resolve(base *url.URL, ref string) *url.URL {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "#") {
		return nil
	}
	u, err := base.Parse(ref)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil
	}
	normalize(u)
	return u
}

// requisiteRels are the <link rel> values whose href a page needs to
// render (or that browsers fetch along with it).
var requisiteRels = map[string]bool{
	"stylesheet": true, "icon": true, "shortcut": true, "apple-touch-icon": true,
	"apple-touch-icon-precomposed": true, "mask-icon": true, "manifest": true,
	"preload": true, "modulepreload": true,
}

// extractHTML parses an HTML page fetched from pageURL and returns the
// links to follow, the page requisites to fetch, and whether the page is a
// server-generated directory listing.
func extractHTML(data []byte, pageURL *url.URL) (links, reqs []*url.URL, listing bool) {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, nil, false
	}

	base := pageURL
	if b := findBase(doc); b != "" {
		if bu, err := pageURL.Parse(strings.TrimSpace(b)); err == nil {
			base = bu
		}
	}

	addLink := func(v string) {
		if u := resolve(base, v); u != nil {
			links = append(links, u)
		}
	}
	addReq := func(v string) {
		if u := resolve(base, v); u != nil {
			reqs = append(reqs, u)
		}
	}
	addSrcset := func(v string) {
		for _, cand := range strings.Split(v, ",") {
			if f := strings.Fields(cand); len(f) > 0 {
				addReq(f[0])
			}
		}
	}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			attr := func(k string) string { return getAttr(n, k) }
			switch n.Data {
			case "a", "area":
				addLink(attr("href"))
			case "frame", "iframe":
				addLink(attr("src"))
			case "img", "source":
				addReq(attr("src"))
				addSrcset(attr("srcset"))
			case "script", "audio", "track", "embed":
				addReq(attr("src"))
			case "video":
				addReq(attr("src"))
				addReq(attr("poster"))
			case "object":
				addReq(attr("data"))
			case "input":
				if strings.EqualFold(attr("type"), "image") {
					addReq(attr("src"))
				}
			case "link":
				for _, rel := range strings.Fields(strings.ToLower(attr("rel"))) {
					if requisiteRels[rel] {
						addReq(attr("href"))
						break
					}
				}
			case "body", "table", "tr", "td", "th":
				addReq(attr("background"))
			case "style":
				if n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
					reqs = append(reqs, extractCSS(n.FirstChild.Data, base)...)
				}
			}
			if s := attr("style"); s != "" {
				reqs = append(reqs, extractCSS(s, base)...)
			}
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(doc)

	return links, reqs, isListing(doc, pageURL)
}

func findBase(n *html.Node) string {
	if n.Type == html.ElementNode && n.Data == "base" {
		if href := getAttr(n, "href"); href != "" {
			return href
		}
	}
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if b := findBase(ch); b != "" {
			return b
		}
	}
	return ""
}

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == key {
			return a.Val
		}
	}
	return ""
}

// listingTitleRe matches the <title> of the common servers' directory
// listings: "Index of /x" (Apache, nginx, lighttpd, ...), "Directory
// listing for /x" (Python's http.server, Tomcat), "Directory: /x" (Jetty).
var listingTitleRe = regexp.MustCompile(`(?i)^(index of|directory listing for|directory listing of|directory:)\s`)

// sortQueryRe matches the query of a column-sorting link: Apache's
// "C=N;O=D" (also "C=M&O=A") and Caddy's "sort=name&order=asc".
var sortQueryRe = regexp.MustCompile(`^(?:C=[NMSD][;&]O=[AD]|sort=\w+&order=\w+|order=\w+&sort=\w+)$`)

// isListing reports whether doc, fetched from pageURL, is a server-generated
// directory listing.
func isListing(doc *html.Node, pageURL *url.URL) bool {
	var title strings.Builder
	var anchors []*html.Node
	var body *html.Node
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, inTitle bool) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "title":
				inTitle = true
			case "a":
				anchors = append(anchors, n)
			case "body":
				body = n
			}
		}
		if inTitle && n.Type == html.TextNode {
			title.WriteString(n.Data)
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch, inTitle)
		}
	}
	walk(doc, false)

	if listingTitleRe.MatchString(strings.Join(strings.Fields(title.String()), " ")) {
		return true
	}

	// IIS: "<A HREF="/">[To Parent Directory]</A>".
	// Others (Caddy, ...): at least two links re-sorting this same page.
	sortLinks := 0
	for _, a := range anchors {
		if strings.TrimSpace(textOf(a)) == "[To Parent Directory]" {
			return true
		}
		if u, err := pageURL.Parse(getAttr(a, "href")); err == nil && u.Path == pageURL.Path && sortQueryRe.MatchString(u.RawQuery) {
			sortLinks++
		}
	}
	if sortLinks >= 2 {
		return true
	}

	// Go's http.FileServer: no title, the body is a single <pre> of links.
	if body != nil && title.Len() == 0 {
		var elems []*html.Node
		for ch := body.FirstChild; ch != nil; ch = ch.NextSibling {
			if ch.Type == html.ElementNode {
				elems = append(elems, ch)
			}
		}
		if len(elems) == 1 && elems[0].Data == "pre" {
			onlyAnchors := true
			for ch := elems[0].FirstChild; ch != nil; ch = ch.NextSibling {
				if ch.Type == html.ElementNode && ch.Data != "a" {
					onlyAnchors = false
				}
			}
			if onlyAnchors {
				return true
			}
		}
	}
	return false
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(n)
	return b.String()
}

var (
	cssURLRe    = regexp.MustCompile(`url\(\s*(?:"([^"]*)"|'([^']*)'|([^)\s]*))\s*\)`)
	cssImportRe = regexp.MustCompile(`@import\s+(?:"([^"]*)"|'([^']*)')`)
)

// extractCSS returns the URLs a stylesheet (or style attribute/element)
// references through url(...) and @import "...", resolved against base.
func extractCSS(css string, base *url.URL) []*url.URL {
	var out []*url.URL
	for _, re := range []*regexp.Regexp{cssURLRe, cssImportRe} {
		for _, m := range re.FindAllStringSubmatch(css, -1) {
			for _, g := range m[1:] {
				if g != "" {
					if u := resolve(base, g); u != nil {
						out = append(out, u)
					}
					break
				}
			}
		}
	}
	return out
}
