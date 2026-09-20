package archiveorg

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ---- ParseIdentifier ----

func TestParseIdentifier(t *testing.T) {
	cases := []struct {
		name    string
		source  string
		want    string
		wantErr bool
	}{
		{"details URL", "https://archive.org/details/some-item", "some-item", false},
		{"download URL", "https://archive.org/download/some-item", "some-item", false},
		{"http (not https) is accepted too", "http://archive.org/details/some-item", "some-item", false},
		{"details URL with a trailing file path", "https://archive.org/details/some-item/some-item_files.xml", "some-item", false},
		{"bare identifier", "some-item", "some-item", false},
		{"a trailing path after the identifier is just ignored, not an error", "https://archive.org/details/some/item", "some", false},
		{"an identifier containing a colon (no recognized prefix) is rejected", "foo:bar", "", true},
		{"an unrelated URL is rejected", "https://example.com/foo", "", true},
		{"empty string is rejected", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseIdentifier(c.source)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if !c.wantErr && got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// ---- downloadURL ----

func TestDownloadURL(t *testing.T) {
	cases := []struct {
		name, identifier, fname, want string
	}{
		{"simple file", "my-item", "file.txt", "https://archive.org/download/my-item/file.txt"},
		{"nested path is preserved but each segment escaped", "my-item", "sub dir/file name.txt",
			"https://archive.org/download/my-item/sub%20dir/file%20name.txt"},
		{"identifier itself is escaped", "my item", "file.txt", "https://archive.org/download/my%20item/file.txt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := downloadURL(c.identifier, c.fname); got != c.want {
				t.Errorf("downloadURL(%q, %q) = %q, want %q", c.identifier, c.fname, got, c.want)
			}
		})
	}
}

// ---- safeRelPath ----

func TestSafeRelPath(t *testing.T) {
	cases := []struct {
		name, fname string
		wantErr     bool
		want        string
	}{
		{"simple file", "file.txt", false, "file.txt"},
		{"nested file", "sub/dir/file.txt", false, filepath.FromSlash("sub/dir/file.txt")},
		{"a .. that stays inside resolves", "sub/../file.txt", false, "file.txt"},
		{"bare ..", "..", true, ""},
		{"leading .. escaping destDir", "../evil.txt", true, ""},
		{"absolute path", "/etc/passwd", true, ""},
		{"resolves to the directory itself", ".", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := safeRelPath(t.TempDir(), c.fname)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if !c.wantErr && got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// ---- alreadyHave ----

func TestAlreadyHave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")

	if alreadyHave(path, 5) {
		t.Error("a missing file must not be reported as already had")
	}

	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !alreadyHave(path, 5) {
		t.Error("a file of the exact expected size should be reported as already had")
	}
	if alreadyHave(path, 999) {
		t.Error("a size mismatch must not be reported as already had")
	}
}

// ---- downloadFile ----

func TestDownloadFileWritesAtomicallyAndRenames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("file content"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	finalPath := filepath.Join(dir, "sub", "file.txt")
	if err := downloadFile(context.Background(), srv.Client(), srv.URL, finalPath, ""); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(finalPath)
	if err != nil || string(got) != "file content" {
		t.Fatalf("got %q, %v", got, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "sub", "*"+tmpSuffix)); len(matches) != 0 {
		t.Errorf("leftover temp file(s): %v", matches)
	}
}

func TestDownloadFileVerifiesMD5AndCleansUpOnMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("file content"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	finalPath := filepath.Join(dir, "file.txt")
	err := downloadFile(context.Background(), srv.Client(), srv.URL, finalPath, "0000000000000000000000000000000")
	if err == nil || !strings.Contains(err.Error(), "md5 mismatch") {
		t.Fatalf("err = %v, want an md5 mismatch error", err)
	}
	if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
		t.Errorf("final file should not exist after a checksum mismatch, stat err = %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*"+tmpSuffix)); len(matches) != 0 {
		t.Errorf("leftover temp file(s): %v", matches)
	}
}

func TestDownloadFileAcceptsCorrectMD5(t *testing.T) {
	body := "file content"
	sum := md5.Sum([]byte(body))
	wantMD5 := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	dir := t.TempDir()
	finalPath := filepath.Join(dir, "file.txt")
	if err := downloadFile(context.Background(), srv.Client(), srv.URL, finalPath, wantMD5); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(finalPath)
	if err != nil || string(got) != body {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestDownloadFileNonOKStatusLeavesNoFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	finalPath := filepath.Join(dir, "file.txt")
	if err := downloadFile(context.Background(), srv.Client(), srv.URL, finalPath, ""); err == nil {
		t.Fatal("expected an error for a 404 response")
	}
	if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
		t.Errorf("final file should not exist, stat err = %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*"+tmpSuffix)); len(matches) != 0 {
		t.Errorf("leftover temp file(s): %v", matches)
	}
}

// ---- fetchMetadata ----

// redirectingClient returns an *http.Client that sends every request to srv
// regardless of the URL's own scheme/host — needed because fetchMetadata
// builds a hardcoded https://archive.org/... URL.
func redirectingClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: redirectTransport{target: target}}
}

func TestFetchMetadataDecodesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(metadata{
			Files: []metaFile{{Name: "a.txt", Size: "5", MD5: "abc"}},
		})
	}))
	defer srv.Close()

	md, err := fetchMetadata(context.Background(), redirectingClient(t, srv), "some-item")
	if err != nil {
		t.Fatal(err)
	}
	if md.IsDark {
		t.Error("IsDark should be false")
	}
	if len(md.Files) != 1 || md.Files[0].Name != "a.txt" || md.Files[0].Size != "5" {
		t.Errorf("files = %+v", md.Files)
	}
}

func TestFetchMetadataNonOKStatusReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := fetchMetadata(context.Background(), redirectingClient(t, srv), "some-item"); err == nil {
		t.Fatal("expected an error for a 500 response")
	}
}

func TestFetchMetadataMalformedJSONReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	if _, err := fetchMetadata(context.Background(), redirectingClient(t, srv), "some-item"); err == nil {
		t.Fatal("expected an error for a malformed JSON body")
	}
}

// ---- Mirror: end-to-end against a fake archive.org ----

func TestMirrorEndToEnd(t *testing.T) {
	fs := newFakeServer()
	fs.md = metadata{
		Files: []metaFile{
			{Name: "a.txt", Size: "5"},
			{Name: "sub/b.txt", Size: "5"},
		},
	}
	fs.setFile("a.txt", "hello")
	fs.setFile("sub/b.txt", "world")

	srv := httptest.NewServer(fs)
	defer srv.Close()
	useTestServerAsDefaultTransport(t, srv)

	destDir := t.TempDir()
	if err := Mirror(context.Background(), "test-item", destDir, false); err != nil {
		t.Fatal(err)
	}

	itemDir := filepath.Join(destDir, "test-item")
	checkFile := func(rel, want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(itemDir, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%s: got %q, want %q", rel, got, want)
		}
	}
	checkFile("a.txt", "hello")
	checkFile(filepath.Join("sub", "b.txt"), "world")
	if _, err := os.Stat(filepath.Join(itemDir, doneFileName)); err != nil {
		t.Errorf(".done marker missing: %v", err)
	}

	// A second run must skip outright (the .done marker), never even
	// fetching metadata again.
	before := fs.requestCount("/metadata/test-item")
	if err := Mirror(context.Background(), "test-item", destDir, false); err != nil {
		t.Fatal(err)
	}
	if after := fs.requestCount("/metadata/test-item"); after != before {
		t.Errorf("a completed item should be skipped without a metadata fetch; before=%d after=%d", before, after)
	}

	// -f (force) must re-check even with .done present.
	if err := Mirror(context.Background(), "test-item", destDir, true); err != nil {
		t.Fatal(err)
	}
	if after := fs.requestCount("/metadata/test-item"); after == before {
		t.Errorf("force=true should re-fetch metadata even with .done present")
	}
	// The files already match on disk, so force shouldn't re-download them.
	if n := fs.requestCount("/download/test-item/a.txt"); n != 1 {
		t.Errorf("a.txt should only have been downloaded once (matches on disk after), got %d requests", n)
	}
}

func TestMirrorSkipsFileAlreadyOnDiskAtExpectedSize(t *testing.T) {
	fs := newFakeServer()
	fs.md = metadata{Files: []metaFile{{Name: "a.txt", Size: "5"}}}
	fs.setFile("a.txt", "should never be fetched")

	srv := httptest.NewServer(fs)
	defer srv.Close()
	useTestServerAsDefaultTransport(t, srv)

	destDir := t.TempDir()
	itemDir := filepath.Join(destDir, "test-item")
	if err := os.MkdirAll(itemDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(itemDir, "a.txt"), []byte("prior"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Mirror(context.Background(), "test-item", destDir, false); err != nil {
		t.Fatal(err)
	}
	if n := fs.requestCount("/download/test-item/a.txt"); n != 0 {
		t.Errorf("expected no download request for an already-correctly-sized file, got %d", n)
	}
	got, err := os.ReadFile(filepath.Join(itemDir, "a.txt"))
	if err != nil || string(got) != "prior" {
		t.Errorf("file should have been left untouched: got %q, %v", got, err)
	}
}

func TestMirrorDarkItemReturnsErrorWithoutDoneMarker(t *testing.T) {
	fs := newFakeServer()
	fs.md = metadata{IsDark: true, Files: []metaFile{{Name: "a.txt", Size: "5"}}}

	srv := httptest.NewServer(fs)
	defer srv.Close()
	useTestServerAsDefaultTransport(t, srv)

	destDir := t.TempDir()
	if err := Mirror(context.Background(), "test-item", destDir, false); err == nil {
		t.Fatal("expected an error for a dark item")
	}
	if _, err := os.Stat(filepath.Join(destDir, "test-item", doneFileName)); !os.IsNotExist(err) {
		t.Errorf("a dark item must not leave a .done marker, stat err = %v", err)
	}
}

func TestMirrorNoFilesReturnsError(t *testing.T) {
	fs := newFakeServer()
	fs.md = metadata{Files: nil}

	srv := httptest.NewServer(fs)
	defer srv.Close()
	useTestServerAsDefaultTransport(t, srv)

	destDir := t.TempDir()
	if err := Mirror(context.Background(), "test-item", destDir, false); err == nil {
		t.Fatal("expected an error for an item with no files listed")
	}
}

func TestMirrorRejectsPathTraversalFileName(t *testing.T) {
	fs := newFakeServer()
	fs.md = metadata{Files: []metaFile{{Name: "../evil.txt", Size: "5"}}}
	fs.setFile("../evil.txt", "hacked")

	srv := httptest.NewServer(fs)
	defer srv.Close()
	useTestServerAsDefaultTransport(t, srv)

	destDir := t.TempDir()
	if err := Mirror(context.Background(), "test-item", destDir, false); err == nil {
		t.Fatal("expected an error for a file name that escapes the item directory")
	}
	if _, err := os.Stat(filepath.Join(destDir, "evil.txt")); !os.IsNotExist(err) {
		t.Error("the traversal attempt must not have written a file outside the item directory")
	}
}

func TestMirrorSkipsChecksumForItemsFilesXML(t *testing.T) {
	const identifier = "test-item"
	fs := newFakeServer()
	body := "<xml>whatever</xml>"
	fs.md = metadata{Files: []metaFile{
		{Name: identifier + "_files.xml", Size: "20", MD5: "0000000000000000000000000000000"},
	}}
	fs.setFile(identifier+"_files.xml", body)

	srv := httptest.NewServer(fs)
	defer srv.Close()
	useTestServerAsDefaultTransport(t, srv)

	destDir := t.TempDir()
	if err := Mirror(context.Background(), identifier, destDir, false); err != nil {
		t.Fatalf("a wrong md5 on the item's own _files.xml must not fail the mirror: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(destDir, identifier, identifier+"_files.xml"))
	if err != nil || string(got) != body {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestMirrorFailedDownloadReturnsErrorWithoutDoneMarker(t *testing.T) {
	fs := newFakeServer()
	fs.md = metadata{Files: []metaFile{{Name: "missing.txt", Size: "5"}}}
	// No fake file registered for missing.txt, so its download 404s.

	srv := httptest.NewServer(fs)
	defer srv.Close()
	useTestServerAsDefaultTransport(t, srv)

	destDir := t.TempDir()
	if err := Mirror(context.Background(), "test-item", destDir, false); err == nil {
		t.Fatal("expected an error when a listed file can't be downloaded")
	}
	if _, err := os.Stat(filepath.Join(destDir, "test-item", doneFileName)); !os.IsNotExist(err) {
		t.Errorf("a failed mirror must not leave a .done marker, stat err = %v", err)
	}
}

// ---- test helpers ----

// redirectTransport rewrites every outgoing request's scheme and host to
// target's before delegating to base, so code with hardcoded absolute
// archive.org URLs can be pointed at a local httptest.Server.
type redirectTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (rt redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = rt.target.Scheme
	req.URL.Host = rt.target.Host
	req.Host = rt.target.Host
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// useTestServerAsDefaultTransport points http.DefaultTransport at srv for
// the duration of the test, restoring it on cleanup. Mirror builds its own
// *http.Client with no Transport set, so this is the only way to redirect
// its hardcoded archive.org URLs to a local fake server.
func useTestServerAsDefaultTransport(t *testing.T, srv *httptest.Server) {
	t.Helper()
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	orig := http.DefaultTransport
	http.DefaultTransport = redirectTransport{target: target, base: orig}
	t.Cleanup(func() { http.DefaultTransport = orig })
}

// fakeServer is a minimal in-process stand-in for archive.org's metadata and
// download endpoints, always serving item identifier "test-item" unless
// used through fetchMetadata's own base-URL override.
type fakeServer struct {
	mu       sync.Mutex
	requests []string
	md       metadata
	files    map[string]string // file name -> body
}

func newFakeServer() *fakeServer {
	return &fakeServer{files: map[string]string{}}
}

func (s *fakeServer) setFile(name, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[name] = body
}

func (s *fakeServer) requestCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if r == path {
			n++
		}
	}
	return n
}

func (s *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.URL.Path)
	s.mu.Unlock()

	switch {
	case strings.HasPrefix(r.URL.Path, "/metadata/"):
		s.mu.Lock()
		md := s.md
		s.mu.Unlock()
		json.NewEncoder(w).Encode(md)
	case strings.HasPrefix(r.URL.Path, "/download/"):
		// Path is "/download/<identifier>/<name...>"; the name is whatever
		// follows the identifier segment.
		rest := strings.TrimPrefix(r.URL.Path, "/download/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		name := parts[1]
		s.mu.Lock()
		body, ok := s.files[name]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(body))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}
