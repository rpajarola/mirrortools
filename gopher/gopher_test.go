package gopher

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- fakeConn: a net.Conn stand-in for tests that don't need a real socket ----

// fakeConn lets writeFile/readMenu be exercised directly, without a real
// TCP connection: reads come from r, and once r is exhausted, err is
// returned instead of the usual io.EOF (nil err behaves like a clean
// close). Every other net.Conn method is a no-op; only Read is ever
// called by the code under test here.
type fakeConn struct {
	r   strings.Reader
	err error
}

func newFakeConn(body string, err error) *fakeConn {
	c := &fakeConn{err: err}
	c.r = *strings.NewReader(body)
	return c
}

func (f *fakeConn) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err != nil && f.err != nil {
		return n, f.err
	}
	return n, err
}
func (f *fakeConn) Write(p []byte) (int, error)      { return len(p), nil }
func (f *fakeConn) Close() error                     { return nil }
func (f *fakeConn) LocalAddr() net.Addr              { return nil }
func (f *fakeConn) RemoteAddr() net.Addr             { return nil }
func (f *fakeConn) SetDeadline(time.Time) error      { return nil }
func (f *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fakeConn) SetWriteDeadline(time.Time) error { return nil }

// ---- sniffIsMenu ----

func TestSniffIsMenu(t *testing.T) {
	cases := []struct {
		name  string
		chunk string
		want  bool
	}{
		{
			name:  "well-formed menu",
			chunk: "1A submenu\t/sub\thost\t70\r\n0A text file\t/text.txt\thost\t70\r\n.\r\n",
			want:  true,
		},
		{
			name:  "single info line still counts as a menu",
			chunk: "iHello\t\terror.host\t1\r\n",
			want:  true,
		},
		{
			name:  "plain text content is not a menu",
			chunk: "This is just a text file.\nIt has no tabs at all.\n",
			want:  false,
		},
		{
			name:  "binary-ish content is not a menu",
			chunk: "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR",
			want:  false,
		},
		{
			name:  "empty chunk has nothing to match, not a menu",
			chunk: "",
			want:  false,
		},
		{
			name: "a truncated last line isn't held against the guess",
			// This whole chunk is exactly the read-size boundary in
			// practice; the key thing under test is that a garbled last
			// line doesn't flip the verdict when earlier lines matched.
			chunk: "1A submenu\t/sub\thost\t70\r\n0A text file\t/text.txt\thost\t7", // cut off mid-port
			want:  true,
		},
		{
			name:  "a malformed line that ISN'T last must fail the guess",
			chunk: "1A submenu\t/sub\thost\t70\r\nthis has no tabs at all\r\n0Trailing well-formed line\t/x\thost\t70\r\n",
			want:  false,
		},
		{
			name:  "the lone terminator line alone is not evidence of a menu",
			chunk: ".\r\n",
			want:  false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sniffIsMenu([]byte(c.chunk)); got != c.want {
				t.Errorf("sniffIsMenu(%q) = %v, want %v", c.chunk, got, c.want)
			}
		})
	}
}

// ---- direntRe ----

func TestDirentRe(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		wantMatch bool
		wantType  byte
		wantSel   string
		wantHost  string
		wantPort  string
	}{
		{
			name:      "ordinary entry",
			line:      "1A submenu\t/sub\thost.example\t70",
			wantMatch: true, wantType: '1', wantSel: "/sub", wantHost: "host.example", wantPort: "70",
		},
		{
			name: "Gopher+ trailing field must not break the match",
			// Regression test: gopher.quux.org appends a "+" field after
			// the port on every line to advertise Gopher+ support. Before
			// direntRe allowed an optional trailing field, this made every
			// single line on that server fail to match, so the menu was
			// logged as entirely unparseable and nothing under it was ever
			// downloaded.
			line:      "1Software\t/Software\tgopher.quux.org\t70\t+",
			wantMatch: true, wantType: '1', wantSel: "/Software", wantHost: "gopher.quux.org", wantPort: "70",
		},
		{
			name:      "info line with empty selector/host and a fake port",
			line:      "iHello there\t\terror.host\t1",
			wantMatch: true, wantType: 'i', wantSel: "", wantHost: "error.host", wantPort: "1",
		},
		{
			name:      "empty port is allowed by the format",
			line:      "1A submenu\t/sub\thost.example\t",
			wantMatch: true, wantType: '1', wantSel: "/sub", wantHost: "host.example", wantPort: "",
		},
		{
			name:      "not enough tab-separated fields",
			line:      "1A submenu\t/sub",
			wantMatch: false,
		},
		{
			name:      "unknown type character",
			line:      "zUnknown type\t/x\thost\t70",
			wantMatch: false,
		},
		{
			name:      "plain text, no tabs at all",
			line:      "just some plain text",
			wantMatch: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := direntRe.FindStringSubmatch(c.line)
			if (m != nil) != c.wantMatch {
				t.Fatalf("match = %v, want %v (line %q)", m != nil, c.wantMatch, c.line)
			}
			if !c.wantMatch {
				return
			}
			if got := m[1][0]; got != c.wantType {
				t.Errorf("type = %q, want %q", got, c.wantType)
			}
			if m[3] != c.wantSel {
				t.Errorf("selector = %q, want %q", m[3], c.wantSel)
			}
			if m[4] != c.wantHost {
				t.Errorf("host = %q, want %q", m[4], c.wantHost)
			}
			if m[5] != c.wantPort {
				t.Errorf("port = %q, want %q", m[5], c.wantPort)
			}
		})
	}
}

// ---- crawl.localPath: scope boundary and path-traversal protection ----

func TestCrawlLocalPathScopeBoundary(t *testing.T) {
	destDir := t.TempDir()
	c := &crawl{
		hostname: "host.example",
		port:     "70",
		basedir:  filepath.Join(destDir, "host.example"),
		seen:     map[string]bool{},
	}
	// Mirrors exactly what Mirror computes for a "/1/foo" URL: the leading
	// "/1" is stripped before it's ever used as a selector, and rootDir is
	// derived from that same stripped selector. This is a regression test
	// for a real bug in the Python script this replaces, where the scope
	// boundary was instead derived from the raw, unstripped URL path (still
	// containing "/1") and so could never match any selector the fetches
	// actually used — the very first fetch of any "/1/..." URL failed this
	// check and the whole run aborted immediately.
	selector := "/foo"
	c.rootDir = filepath.Join(c.basedir, "foo")

	if _, err := c.localPath(selector); err != nil {
		t.Errorf("root selector itself must resolve inside its own scope: %v", err)
	}
	if _, err := c.localPath("/foo/bar"); err != nil {
		t.Errorf("a selector nested under the root must be allowed: %v", err)
	}
	if _, err := c.localPath("/foo"); err != nil {
		t.Errorf("the root selector's own directory form must be allowed: %v", err)
	}
	if _, err := c.localPath("/"); err == nil {
		t.Error("a selector above the root must be rejected")
	}
	if _, err := c.localPath("/other"); err == nil {
		t.Error("a sibling selector must be rejected")
	}
	if _, err := c.localPath("/foo/../../etc/passwd"); err == nil {
		t.Error("a selector escaping the root via .. must be rejected")
	}
	if _, err := c.localPath("../../etc/passwd"); err == nil {
		t.Error("a relative (non-rooted) selector escaping via .. must be rejected")
	}
}

// ---- writeFile: atomic write, temp-file cleanup ----

func TestWriteFileWritesFirstThenRestThenRenames(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "sub", "file.txt")
	conn := newFakeConn("-rest-of-body", nil)

	if err := writeFile(conn, []byte("first-chunk"), localPath); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(localPath)
	if err != nil || string(got) != "first-chunk-rest-of-body" {
		t.Fatalf("final file: got %q, %v", got, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "sub", "*"+tmpSuffix)); len(matches) != 0 {
		t.Fatalf("leftover temp file(s): %v", matches)
	}
}

func TestWriteFileErrorCleansUpTempFile(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "file.txt")
	wantErr := errors.New("connection reset")
	conn := newFakeConn("partial-body", wantErr)

	err := writeFile(conn, []byte("first"), localPath)
	if err == nil {
		t.Fatal("expected an error from a failed read")
	}
	if _, err := os.Stat(localPath); !os.IsNotExist(err) {
		t.Fatalf("final file should not exist, stat err = %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*"+tmpSuffix)); len(matches) != 0 {
		t.Fatalf("temp file should have been cleaned up, found: %v", matches)
	}
}

// ---- readMenu: line classification and README assembly, no network ----

func TestReadMenuClassifiesEntriesAndBuildsReadme(t *testing.T) {
	dir := t.TempDir()
	c := &crawl{
		hostname: "example.com",
		port:     "70",
		basedir:  dir,
		rootDir:  dir,
		seen:     map[string]bool{"/already-seen.txt": true}, // pre-seeded so get() no-ops without dialing
	}

	menu := strings.Join([]string{
		"iinfo line one\t\terror.host\t1",
		"iinfo line two\t\terror.host\t1",
		"3an error entry\t/err\terror.host\t1",
		"7a search entry, not followed\t/search\texample.com\t70",
		"han http link, not followed\tURL:http://x.example/\texample.com\t70",
		"1cross-host entry, not followed\t/\tother.example\t70",
		"1wrong-port entry, not followed\t/\texample.com\t7070",
		"0already-seen entry, no dial needed\t/already-seen.txt\texample.com\t70",
		"this line has no tabs at all and can't be parsed",
		".",
		"", // trailing CRLF produces one empty split element
	}, "\r\n")

	// The rest-of-connection reader is empty: readMenu drains until EOF, so
	// passing the whole crafted menu as `first` and nothing more is enough.
	conn := newFakeConn("", nil)

	if err := c.readMenu(context.Background(), conn, []byte(menu), dir); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "00README"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "info line one\ninfo line two\n"; string(got) != want {
		t.Errorf("00README: got %q, want %q", got, want)
	}
}

// ---- fakeGopherServer: a minimal in-process Gopher server for integration tests ----

type fakeGopherServer struct {
	mu        sync.Mutex
	responses map[string]string // selector -> raw response bytes (no request marked here gets an immediate close)
	requests  []string
}

// newFakeGopherServer starts the server with no responses configured; call
// srv.setResponses once the caller knows its own ephemeral port (needed to
// build menu entries that point back at the server).
func newFakeGopherServer(t *testing.T) (srv *fakeGopherServer, host, port string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	srv = &fakeGopherServer{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.handle(conn)
		}
	}()

	host, port, err = net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return srv, host, port
}

func (s *fakeGopherServer) handle(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return
	}
	selector := strings.TrimRight(line, "\r\n")

	s.mu.Lock()
	s.requests = append(s.requests, selector)
	resp, ok := s.responses[selector]
	s.mu.Unlock()

	if !ok {
		return // close with no bytes written, same as a "not found" selector in the wild
	}
	conn.Write([]byte(resp))
}

func (s *fakeGopherServer) requestedSelectors() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *fakeGopherServer) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

func (s *fakeGopherServer) setResponses(m map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses = m
}

// ---- Mirror: end-to-end integration tests against a real local socket ----

// TestMirrorEndToEnd mirrors a small menu tree from a real local Gopher
// server and checks the resulting directory layout, README assembly, and
// that every kind of entry the crawl is supposed to skip (search, HTTP
// link, cross-host, and a same-host escape attempt back above the mirrored
// subtree) is genuinely never requested — not just logged as skipped. It
// also includes a Gopher+-style trailing field (see TestDirentRe) so the
// exact real-world failure mode that motivated that fix is covered
// end-to-end, not just at the regex level.
func TestMirrorEndToEnd(t *testing.T) {
	srv, host, port := newFakeGopherServer(t)

	srv.setResponses(map[string]string{
		"/root": strings.Join([]string{
			"1A submenu\t/root/sub\t" + host + "\t" + port,
			"0A text file\t/root/text.txt\t" + host + "\t" + port,
			"gA binary blob\t/root/pic.gif\t" + host + "\t" + port + "\t+", // Gopher+ trailing field
			"iInfo line one\t\terror.host\t1",
			"iInfo line two\t\terror.host\t1",
			"7A search, not followed\t/search\t" + host + "\t" + port,
			"hAn http link, not followed\tURL:http://example.com/\t" + host + "\t" + port,
			"1Cross-host, not followed\t/elsewhere\tother.example\t70",
			"1Escape attempt, not followed\t/\t" + host + "\t" + port,
			".",
			"",
		}, "\r\n"),
		"/root/sub": strings.Join([]string{
			"0Nested text\t/root/sub/deep.txt\t" + host + "\t" + port,
			".",
			"",
		}, "\r\n"),
		"/root/text.txt":     "hello text file",
		"/root/pic.gif":      "GIF89a-fake-binary-data",
		"/root/sub/deep.txt": "deep content",
	})

	destDir := t.TempDir()
	source := "gopher://" + net.JoinHostPort(host, port) + "/1/root"

	if err := Mirror(context.Background(), source, destDir); err != nil {
		t.Fatal(err)
	}

	hostDir := filepath.Join(destDir, host)
	checkFile := func(rel, want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(hostDir, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%s: got %q, want %q", rel, got, want)
		}
	}
	checkFile("root/text.txt", "hello text file")
	checkFile("root/pic.gif", "GIF89a-fake-binary-data")
	checkFile("root/sub/deep.txt", "deep content")
	checkFile("root/00README", "Info line one\nInfo line two\n")

	requested := srv.requestedSelectors()
	for _, sel := range []string{"/root", "/root/sub", "/root/text.txt", "/root/pic.gif", "/root/sub/deep.txt"} {
		if !containsString(requested, sel) {
			t.Errorf("expected selector %q to have been requested; got %v", sel, requested)
		}
	}
	for _, sel := range []string{"/search", "URL:http://example.com/", "/elsewhere", "/"} {
		if containsString(requested, sel) {
			t.Errorf("selector %q should never have been requested (must be filtered before any dial); got %v", sel, requested)
		}
	}

	// Resume: a second run against the same destDir must not re-download
	// already-complete plain files, but must revisit menus (directories) —
	// that's how a later run picks up anything a Gopher menu has added
	// since the last mirror.
	srv.reset()
	if err := Mirror(context.Background(), source, destDir); err != nil {
		t.Fatal(err)
	}
	requested2 := srv.requestedSelectors()
	for _, sel := range []string{"/root/text.txt", "/root/pic.gif", "/root/sub/deep.txt"} {
		if containsString(requested2, sel) {
			t.Errorf("already-downloaded file %q should not be re-fetched on resume; requested = %v", sel, requested2)
		}
	}
	for _, sel := range []string{"/root", "/root/sub"} {
		if !containsString(requested2, sel) {
			t.Errorf("menu %q should be revisited on resume; requested = %v", sel, requested2)
		}
	}
}

// TestMirrorEmptyResponseIsSkippedNotFatal covers a selector the server
// closes immediately with no bytes at all (e.g. a stale or unresolvable
// entry) — this must be logged and skipped, not treated as an error that
// aborts the rest of the crawl.
func TestMirrorEmptyResponseIsSkippedNotFatal(t *testing.T) {
	srv, host, port := newFakeGopherServer(t)
	srv.setResponses(map[string]string{
		"/root": strings.Join([]string{
			"0A dangling entry with no response\t/root/gone\t" + host + "\t" + port,
			"0A text file\t/root/text.txt\t" + host + "\t" + port,
			".",
			"",
		}, "\r\n"),
		"/root/text.txt": "still here",
	})

	destDir := t.TempDir()
	source := "gopher://" + net.JoinHostPort(host, port) + "/1/root"
	if err := Mirror(context.Background(), source, destDir); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(destDir, host, "root", "text.txt"))
	if err != nil || string(got) != "still here" {
		t.Fatalf("sibling entry should still have downloaded: got %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(destDir, host, "root", "gone")); !os.IsNotExist(err) {
		t.Fatalf("the empty-response entry should not have created a file, stat err = %v", err)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
