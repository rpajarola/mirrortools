package httpmirror

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/html"
)

func init() { initialBackoff = time.Millisecond }

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// ---- isListing ----

func TestIsListing(t *testing.T) {
	cases := []struct {
		name string
		page string
		want bool
	}{
		{"apache", `<!DOCTYPE HTML PUBLIC "-//W3C//DTD HTML 3.2 Final//EN">
<html><head><title>Index of /pub</title></head><body><h1>Index of /pub</h1>
<table><tr><th><a href="?C=N;O=D">Name</a></th><th><a href="?C=M;O=A">Last modified</a></th></tr>
<tr><td><a href="/">Parent Directory</a></td></tr><tr><td><a href="a.txt">a.txt</a></td></tr></table></body></html>`, true},
		{"nginx", `<html>
<head><title>Index of /pub/</title></head>
<body>
<h1>Index of /pub/</h1><hr><pre><a href="../">../</a>
<a href="a.txt">a.txt</a>                                              01-Jan-2024 00:00    5
</pre><hr></body>
</html>`, true},
		{"python http.server", `<!DOCTYPE HTML><html lang="en"><head><meta charset="utf-8">
<title>Directory listing for /pub/</title></head><body><h1>Directory listing for /pub/</h1><hr><ul>
<li><a href="a.txt">a.txt</a></li></ul><hr></body></html>`, true},
		{"jetty", `<html><head><title>Directory: /pub/</title></head><body></body></html>`, true},
		{"iis", `<html><head><title>example.com - /pub/</title></head><body><H1>example.com - /pub/</H1><hr>
<pre><A HREF="/">[To Parent Directory]</A><br><br> 1/1/2024 12:00 AM 5 <A HREF="/pub/a.txt">a.txt</A><br></pre><hr></body></html>`, true},
		{"go http.FileServer", "<!doctype html>\n<meta name=\"viewport\" content=\"width=device-width\">\n<pre>\n<a href=\"a.txt\">a.txt</a>\n<a href=\"sub/\">sub/</a>\n</pre>\n", true},
		{"caddy-style sort links", `<html><head><title>pub</title></head><body>
<a href="?sort=name&order=asc">Name</a><a href="?sort=size&order=desc">Size</a><a href="a.txt">a.txt</a></body></html>`, true},
		{"regular page", `<html><head><title>Welcome</title></head><body><p>Hi <a href="a.txt">file</a></p></body></html>`, false},
		{"regular page with a single pre", `<html><head><title>Code</title></head><body><pre><a href="x">x</a></pre></body></html>`, false},
		{"title merely mentioning an index", `<html><head><title>The index of all things</title></head><body></body></html>`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc, err := html.Parse(strings.NewReader(c.page))
			if err != nil {
				t.Fatal(err)
			}
			if got := isListing(doc, mustURL(t, "http://example.com/pub/")); got != c.want {
				t.Errorf("isListing = %v, want %v", got, c.want)
			}
		})
	}
}

// ---- extractHTML / extractCSS ----

func urlStrings(us []*url.URL) []string {
	var out []string
	for _, u := range us {
		out = append(out, u.String())
	}
	sort.Strings(out)
	return out
}

func TestExtractHTML(t *testing.T) {
	page := `<html><head>
<link rel="stylesheet" href="s.css"><link rel="icon" href="/favicon.ico"><link rel="alternate" href="feed.xml">
<script src="https://cdn.example.net/x.js"></script>
<style>body { background: url("bg.png") }</style>
</head><body background="tile.gif">
<a href="sub/page.html#frag">x</a><a href="mailto:a@b">m</a><a href="javascript:void(0)">j</a><a href="#top">t</a>
<img src="i.png" srcset="i-2x.png 2x, i-3x.png 3x"><div style="background-image: url('d.png')"></div>
<iframe src="frame.html"></iframe>
</body></html>`
	links, reqs, listing := extractHTML([]byte(page), mustURL(t, "http://example.com/pub/index.html"))
	if listing {
		t.Error("not a listing")
	}
	wantLinks := []string{"http://example.com/pub/frame.html", "http://example.com/pub/sub/page.html"}
	if got := urlStrings(links); fmt.Sprint(got) != fmt.Sprint(wantLinks) {
		t.Errorf("links = %v, want %v", got, wantLinks)
	}
	wantReqs := []string{
		"http://example.com/favicon.ico",
		"http://example.com/pub/bg.png",
		"http://example.com/pub/d.png",
		"http://example.com/pub/i-2x.png",
		"http://example.com/pub/i-3x.png",
		"http://example.com/pub/i.png",
		"http://example.com/pub/s.css",
		"http://example.com/pub/tile.gif",
		"https://cdn.example.net/x.js",
	}
	if got := urlStrings(reqs); fmt.Sprint(got) != fmt.Sprint(wantReqs) {
		t.Errorf("reqs = %v, want %v", got, wantReqs)
	}
}

func TestExtractHTMLHonorsBase(t *testing.T) {
	links, _, _ := extractHTML([]byte(`<html><head><base href="/other/"></head><body><a href="x.html">x</a></body></html>`),
		mustURL(t, "http://example.com/pub/"))
	if got := urlStrings(links); len(got) != 1 || got[0] != "http://example.com/other/x.html" {
		t.Errorf("links = %v", got)
	}
}

func TestExtractCSS(t *testing.T) {
	css := `@import "base.css"; @import url(more.css);
a { background: url(img/a.png) } b { background: url( 'b.png' ) } c { background: url("data:image/png;base64,xx") }`
	got := urlStrings(extractCSS(css, mustURL(t, "http://example.com/static/s.css")))
	want := []string{
		"http://example.com/static/b.png",
		"http://example.com/static/base.css",
		"http://example.com/static/img/a.png",
		"http://example.com/static/more.css",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// ---- localPath / inScope / normalize ----

func TestLocalPath(t *testing.T) {
	c := &crawler{destDir: "/dest"}
	cases := []struct{ in, want string }{
		{"http://example.com/", "/dest/example.com/index.html"},
		{"http://example.com/pub/", "/dest/example.com/pub/index.html"},
		{"http://example.com/pub/a.txt", "/dest/example.com/pub/a.txt"},
		{"http://example.com/pub/a%20b.txt", "/dest/example.com/pub/a b.txt"},
		{"http://example.com:8080/x", "/dest/example.com:8080/x"},
		{"http://example.com/page.php?id=3&p=a/b", "/dest/example.com/page.php?id=3&p=a%2Fb"},
		{"http://example.com/../../etc/passwd", "/dest/example.com/etc/passwd"},
	}
	for _, cs := range cases {
		got, err := c.localPath(mustURL(t, cs.in))
		if err != nil {
			t.Errorf("%s: %v", cs.in, err)
			continue
		}
		if got != filepath.FromSlash(cs.want) {
			t.Errorf("%s: got %q, want %q", cs.in, got, cs.want)
		}
	}
	if _, err := c.localPath(&url.URL{Scheme: "http", Host: "..", Path: "/x"}); err == nil {
		t.Error("host .. must be rejected")
	}
}

func TestInScopeAndNormalize(t *testing.T) {
	c := &crawler{scope: mustURL(t, "http://example.com/pub/")}
	cases := []struct {
		in   string
		want bool
	}{
		{"http://example.com/pub/", true},
		{"http://example.com/pub/a/b.txt", true},
		{"HTTP://EXAMPLE.COM:80/pub/x", true},
		{"http://example.com/pub", false},
		{"http://example.com/public/", false},
		{"https://example.com/pub/x", false},
		{"http://other.com/pub/x", false},
	}
	for _, cs := range cases {
		u := mustURL(t, cs.in)
		normalize(u)
		if got := c.inScope(u); got != cs.want {
			t.Errorf("inScope(%s) = %v, want %v", cs.in, got, cs.want)
		}
	}
}

// ---- Mirror: end-to-end ----

// site is a small fake web server that records every request.
type site struct {
	mu       sync.Mutex
	requests []string // path?query
	handlers map[string]http.HandlerFunc
}

func newSite() *site { return &site{handlers: map[string]http.HandlerFunc{}} }

func (s *site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	key := r.URL.Path
	if r.URL.RawQuery != "" {
		key += "?" + r.URL.RawQuery
	}
	s.requests = append(s.requests, key)
	h, ok := s.handlers[r.URL.Path]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	h(w, r)
}

func (s *site) handle(p string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[p] = h
}

func (s *site) count(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if r == key {
			n++
		}
	}
	return n
}

func (s *site) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

var modTime = time.Date(2020, 5, 17, 12, 0, 0, 0, time.UTC)

// file serves body with a Last-Modified (so conditional requests work).
func file(contentType, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		http.ServeContent(w, r, "", modTime, strings.NewReader(body))
	}
}

func page(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, body)
	}
}

func apacheListing(dir string, entries ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		fmt.Fprintf(&b, `<html><head><title>Index of %s</title></head><body><h1>Index of %s</h1><table>
<tr><th><a href="?C=N;O=D">Name</a></th><th><a href="?C=M;O=A">Last modified</a></th><th><a href="?C=S;O=A">Size</a></th></tr>
<tr><td><a href="../">Parent Directory</a></td></tr>`, dir, dir)
		for _, e := range entries {
			fmt.Fprintf(&b, `<tr><td><a href="%s">%s</a></td></tr>`, e, e)
		}
		b.WriteString("</table></body></html>")
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, b.String())
	}
}

func TestMirrorEndToEnd(t *testing.T) {
	cdn := newSite()
	cdnSrv := httptest.NewServer(cdn)
	defer cdnSrv.Close()
	cdn.handle("/lib.js", file("application/javascript", "js"))

	s := newSite()
	srv := httptest.NewServer(s)
	defer srv.Close()

	s.handle("/pub", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/pub/", http.StatusMovedPermanently)
	})
	s.handle("/pub/", apacheListing("/pub", "a.txt", "sub/", "page.html", "old.txt"))
	s.handle("/pub/a.txt", file("text/plain", "hello"))
	s.handle("/pub/old.txt", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/pub/new.txt", http.StatusFound) })
	s.handle("/pub/new.txt", file("text/plain", "new"))
	s.handle("/pub/sub/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><head><title>Index of /pub/sub/</title></head><body><pre><a href=\"../\">../</a>\n<a href=\"b.bin\">b.bin</a></pre></body></html>")
	})
	s.handle("/pub/sub/b.bin", file("application/octet-stream", "\x00\x01"))
	s.handle("/pub/page.html", page(fmt.Sprintf(`<html><head><title>Page</title>
<link rel="stylesheet" href="/static/s.css"><script src="%s/lib.js"></script></head><body>
<img src="/static/logo.png"><a href="/other/x.html">out of scope</a><a href="%s/away.html">other host</a>
<a href="a.txt">back</a></body></html>`, cdnSrv.URL, cdnSrv.URL)))
	s.handle("/static/s.css", file("text/css", "body { background: url(bg.png) }"))
	s.handle("/static/bg.png", file("image/png", "bg"))
	s.handle("/static/logo.png", file("image/png", "logo"))
	s.handle("/other/x.html", page("<html><body>nope</body></html>"))

	destDir := t.TempDir()
	if err := Mirror(context.Background(), srv.URL+"/pub", destDir, Options{}); err != nil {
		t.Fatal(err)
	}

	host := strings.TrimPrefix(srv.URL, "http://")
	cdnHost := strings.TrimPrefix(cdnSrv.URL, "http://")
	check := func(rel, want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(destDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			return
		}
		if string(got) != want {
			t.Errorf("%s: got %q, want %q", rel, got, want)
		}
	}
	missing := func(rel string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(destDir, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s should not exist (stat err %v)", rel, err)
		}
	}

	check(host+"/pub/a.txt", "hello")
	check(host+"/pub/new.txt", "new")
	check(host+"/pub/sub/b.bin", "\x00\x01")
	check(host+"/static/s.css", "body { background: url(bg.png) }")
	check(host+"/static/bg.png", "bg")
	check(host+"/static/logo.png", "logo")
	check(cdnHost+"/lib.js", "js")
	if _, err := os.Stat(filepath.Join(destDir, host, "pub", "page.html")); err != nil {
		t.Error(err)
	}
	missing(host + "/pub/index.html")
	missing(host + "/pub/sub/index.html")
	missing(host + "/pub/old.txt")
	missing(host + "/other/x.html")

	if fi, err := os.Stat(filepath.Join(destDir, host, "pub", "a.txt")); err != nil || !fi.ModTime().Equal(modTime) {
		t.Errorf("a.txt mtime should come from Last-Modified: %v, %v", fi.ModTime(), err)
	}
	for _, k := range []string{"/pub/?C=N;O=D", "/pub/?C=M;O=A", "/other/x.html", "/"} {
		if n := s.count(k); n != 0 {
			t.Errorf("%s should never be requested, got %d", k, n)
		}
	}
	if n := cdn.count("/away.html"); n != 0 {
		t.Errorf("links to other hosts must not be followed, got %d requests", n)
	}

	// A re-run has nothing left to do and must not hit the server at all.
	before := s.total()
	if err := Mirror(context.Background(), srv.URL+"/pub", destDir, Options{}); err != nil {
		t.Fatal(err)
	}
	if after := s.total(); after != before {
		t.Errorf("re-run made %d requests, want 0", after-before)
	}

	// -update re-reads listings (finding new files) and re-validates
	// files, which are unchanged and so not re-downloaded.
	s.handle("/pub/", apacheListing("/pub", "a.txt", "sub/", "page.html", "old.txt", "c.txt"))
	s.handle("/pub/c.txt", file("text/plain", "fresh"))
	s.handle("/pub/a.txt", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Modified-Since") == "" {
			t.Error("a.txt re-check should be a conditional request")
		}
		file("text/plain", "hello")(w, r)
	})
	if err := Mirror(context.Background(), srv.URL+"/pub", destDir, Options{Update: true}); err != nil {
		t.Fatal(err)
	}
	check(host+"/pub/c.txt", "fresh")
	check(host+"/pub/a.txt", "hello")
}

func TestMirrorResumesAfterTransientError(t *testing.T) {
	s := newSite()
	srv := httptest.NewServer(s)
	defer srv.Close()

	s.handle("/pub/", apacheListing("/pub", "a.txt", "gone.txt"))
	s.handle("/pub/a.txt", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })

	destDir := t.TempDir()
	if err := Mirror(context.Background(), srv.URL+"/pub/", destDir, Options{}); err == nil {
		t.Fatal("expected an error while a.txt keeps failing")
	}
	if n := s.count("/pub/a.txt"); n != maxAttempts {
		t.Errorf("a.txt requested %d times, want %d", n, maxAttempts)
	}

	s.handle("/pub/a.txt", file("text/plain", "hello"))
	before := s.total()
	if err := Mirror(context.Background(), srv.URL+"/pub/", destDir, Options{}); err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	if got, err := os.ReadFile(filepath.Join(destDir, host, "pub", "a.txt")); err != nil || string(got) != "hello" {
		t.Errorf("got %q, %v", got, err)
	}
	// Only the failed file is retried: no start URL probe, no listing.
	if n := s.total() - before; n != 1 {
		t.Errorf("resume made %d requests, want 1", n)
	}
	// The 404 is recorded as permanent and not retried without -update.
	if n := s.count("/pub/gone.txt"); n != 1 {
		t.Errorf("gone.txt requested %d times, want 1", n)
	}
}

func TestMirrorRejectsNonHTTP(t *testing.T) {
	if err := Mirror(context.Background(), "ftp://example.com/", t.TempDir(), Options{}); err == nil {
		t.Fatal("expected an error")
	}
}
