package groupsio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain shrinks the retry backoff/wait durations for the whole test
// binary so tests that exercise withRetry (directly or via apiGet/apiPost/
// download) don't pay real multi-second (or, for a rate limit, multi-minute)
// sleeps. maxAttempts and maxRateLimitTry are left at their production
// values so retry-count behavior is still meaningfully exercised.
func TestMain(m *testing.M) {
	initialBackoff = time.Millisecond
	rateLimitWait = time.Millisecond
	os.Exit(m.Run())
}

// ---- apiFile.isDir ----

func TestApiFileIsDir(t *testing.T) {
	cases := []struct {
		name string
		f    apiFile
		want bool
	}{
		{"is_folder flag", apiFile{IsFolder: true}, true},
		{"type file_type_dir", apiFile{Type: "file_type_dir"}, true},
		{"media_type folder", apiFile{MediaType: "folder"}, true},
		{"plain file", apiFile{Type: "file_type_normal", MediaType: "application/pdf"}, false},
		{"zero value", apiFile{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.f.isDir(); got != c.want {
				t.Errorf("isDir() = %v, want %v", got, c.want)
			}
		})
	}
}

// ---- flexString ----

func TestFlexStringUnmarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		json string
		want flexString
	}{
		{"string token", `"abc123"`, "abc123"},
		{"bare number zero (no next page)", `0`, "0"},
		{"bare positive number", `42`, "42"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var s flexString
			if err := json.Unmarshal([]byte(c.json), &s); err != nil {
				t.Fatal(err)
			}
			if s != c.want {
				t.Errorf("got %q, want %q", s, c.want)
			}
		})
	}
}

// ---- webPath ----

func TestWebPath(t *testing.T) {
	cases := []struct {
		name, dirPath, fname, want string
	}{
		{"simple join", "sub", "file.txt", "sub/file.txt"},
		{"empty dirPath", "", "file.txt", "file.txt"},
		{"segments are percent-escaped individually", "a b", "c d.txt", "a%20b/c%20d.txt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := webPath(c.dirPath, c.fname); got != c.want {
				t.Errorf("webPath(%q, %q) = %q, want %q", c.dirPath, c.fname, got, c.want)
			}
		})
	}
}

// ---- safeRelPath ----

func TestSafeRelPath(t *testing.T) {
	cases := []struct {
		name, dirPath, fname string
		wantErr              bool
		want                 string
	}{
		{"simple file at root", "", "file.txt", false, "file.txt"},
		{"nested file", "sub/dir", "file.txt", false, filepath.FromSlash("sub/dir/file.txt")},
		{"a .. that stays inside the tree resolves, doesn't error", "sub", "../file.txt", false, "file.txt"},
		{"a .. that empties out entirely is rejected", "", "..", true, ""},
		{"multiple .. collapsing to nothing is rejected", "a", "../..", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := safeRelPath(c.dirPath, c.fname)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if !c.wantErr && got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// ---- snippet ----

func TestSnippet(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"short body returned as-is", "hello", "hello"},
		{"newlines escaped so a log line stays one line", "line1\nline2", "line1\\nline2"},
		{"long body truncated with a marker", strings.Repeat("x", 400), strings.Repeat("x", 300) + "...(truncated)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := snippet([]byte(c.body)); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// ---- client.store / storeDirReadme / storeDesc ----

func TestClientStoreCreatesParentDirsAndWritesContent(t *testing.T) {
	c := &client{mirrorDir: t.TempDir()}
	if err := c.store("a/b/file.txt", "hello"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(c.mirrorDir, "a", "b", "file.txt"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestClientStoreDirReadmeAndDesc(t *testing.T) {
	c := &client{mirrorDir: t.TempDir()}
	if err := c.storeDirReadme("sub", "dir desc"); err != nil {
		t.Fatal(err)
	}
	if err := c.storeDesc("sub/file.txt", "file desc"); err != nil {
		t.Fatal(err)
	}
	checks := map[string]string{
		"sub/README":        "dir desc",
		"sub/file.txt.desc": "file desc",
	}
	for rel, want := range checks {
		got, err := os.ReadFile(filepath.Join(c.mirrorDir, filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			t.Errorf("%s: got %q, %v, want %q", rel, got, err, want)
		}
	}
}

// ---- client.download: atomic write, temp-file cleanup ----

func TestDownloadWritesAtomicallyAndRenames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("file content"))
	}))
	defer srv.Close()
	c := &client{http: srv.Client()}
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "sub", "file.txt")

	if err := c.download(context.Background(), srv.URL, finalPath); err != nil {
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

func TestDownloadNonOKStatusLeavesNoFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := &client{http: srv.Client()}
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "file.txt")

	if err := c.download(context.Background(), srv.URL, finalPath); err == nil {
		t.Fatal("expected an error for a 404 response")
	}
	if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
		t.Errorf("final file should not exist, stat err = %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*"+tmpSuffix)); len(matches) != 0 {
		t.Errorf("leftover temp file(s): %v", matches)
	}
}

// ---- withRetry ----

func TestWithRetrySucceedsOnFirstAttempt(t *testing.T) {
	calls := 0
	err := withRetry(context.Background(), func() error {
		calls++
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, want nil,1", err, calls)
	}
}

func TestWithRetryStopsImmediatelyWhenContextAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := withRetry(ctx, func() error {
		calls++
		return nil
	})
	if calls != 0 {
		t.Errorf("fn should never be called with an already-canceled context, calls=%d", calls)
	}
	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestWithRetrySucceedsAfterTransientFailures(t *testing.T) {
	calls := 0
	err := withRetry(context.Background(), func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("transient failure %d", calls)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestWithRetryExhaustsAttemptsAndReturnsLastError(t *testing.T) {
	calls := 0
	err := withRetry(context.Background(), func() error {
		calls++
		return fmt.Errorf("failure %d", calls)
	})
	if calls != maxAttempts {
		t.Errorf("calls = %d, want maxAttempts (%d)", calls, maxAttempts)
	}
	if err == nil || err.Error() != fmt.Sprintf("failure %d", maxAttempts) {
		t.Errorf("err = %v, want the last attempt's error", err)
	}
}

// Regression coverage for the fact that a rateLimitedError gets its own,
// independent attempt limit (maxRateLimitTry) rather than sharing
// maxAttempts — see withRetry's doc comment.
func TestWithRetryRateLimitUsesOwnAttemptLimit(t *testing.T) {
	origLimit := maxRateLimitTry
	maxRateLimitTry = 2
	t.Cleanup(func() { maxRateLimitTry = origLimit })

	calls := 0
	err := withRetry(context.Background(), func() error {
		calls++
		return rateLimitedError{}
	})
	if calls != 2 {
		t.Errorf("calls = %d, want maxRateLimitTry (2)", calls)
	}
	if _, ok := err.(rateLimitedError); !ok {
		t.Errorf("err = %v (%T), want rateLimitedError", err, err)
	}
}

// ---- doAPI: status classification ----

func TestDoAPIClassifiesTooManyRequestsAsRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := &client{http: srv.Client()}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)

	var out json.RawMessage
	err := c.doAPI(req, &out)
	if _, ok := err.(rateLimitedError); !ok {
		t.Fatalf("err = %v (%T), want rateLimitedError", err, err)
	}
}

func TestDoAPINonOKStatusIncludesBodySnippet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	}))
	defer srv.Close()
	c := &client{http: srv.Client()}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)

	var out json.RawMessage
	err := c.doAPI(req, &out)
	if err == nil || !strings.Contains(err.Error(), "http 500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want it to mention the status and body", err)
	}
}

// ---- apiPost: form-encoded body, not query params ----
//
// Regression test for the bug described in groupsio.go's package doc: the
// Python script this replaces put email/password in the query string even
// on a POST; the API expects a form-urlencoded body instead.
func TestApiPostSendsFormEncodedBodyNotQuery(t *testing.T) {
	var gotMethod, gotQuery, gotContentType, gotEmail string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		gotContentType = r.Header.Get("Content-Type")
		r.ParseForm()
		gotEmail = r.Form.Get("email")
		w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	var out json.RawMessage
	if err := c.apiPost(context.Background(), "login", url.Values{"email": {"a@b.com"}, "password": {"secret"}}, &out); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty — params belong in the body", gotQuery)
	}
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Errorf("content-type = %q", gotContentType)
	}
	if gotEmail != "a@b.com" {
		t.Errorf("form email = %q, want a@b.com — params must be readable from the body", gotEmail)
	}
}

// ---- login ----

func TestLogin(t *testing.T) {
	fs := newFakeServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()
	c := newTestClient(t, srv)

	if err := c.login(context.Background(), "a@b.com", "pw"); err != nil {
		t.Fatalf("login: %v", err)
	}

	fs.setFailLogin(true)
	if err := c.login(context.Background(), "a@b.com", "wrongpw"); err == nil {
		t.Fatal("expected an error for a rejected login")
	}
}

// ---- list: pagination and best-effort recovery ----

func TestListPagesThroughAllResults(t *testing.T) {
	fs := newFakeServer()
	fs.listings["/paged|"] = listResponse{
		TotalCount:    2,
		Data:          []apiFile{{Name: "a.txt"}},
		HasMore:       true,
		NextPageToken: "tok2",
	}
	fs.listings["/paged|tok2"] = listResponse{
		TotalCount: 2,
		Data:       []apiFile{{Name: "b.txt"}},
		HasMore:    false,
	}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	c := newTestClient(t, srv)
	c.groupID = "1"

	entries, err := c.list(context.Background(), "/paged")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "a.txt" || entries[1].Name != "b.txt" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestListMismatchedTotalCountStillReturnsData(t *testing.T) {
	fs := newFakeServer()
	fs.listings["/x|"] = listResponse{TotalCount: 5, Data: []apiFile{{Name: "only-one.txt"}}, HasMore: false}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	c := newTestClient(t, srv)
	c.groupID = "1"

	entries, err := c.list(context.Background(), "/x")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want the 1 entry actually returned despite a declared total of 5", entries)
	}
}

// ---- getFile ----

func TestGetFileSkipsBoxAndGenericLinkTypesWithoutRequesting(t *testing.T) {
	fs := newFakeServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()
	c := newTestClient(t, srv)

	c.getFile(context.Background(), "", apiFile{Name: "boxlink", Type: "file_type_box", DownloadURL: srv.URL + "/download/box"})
	c.getFile(context.Background(), "", apiFile{Name: "generic", MediaType: "Generic Link", DownloadURL: srv.URL + "/download/generic"})

	if n := fs.totalRequests(); n != 0 {
		t.Errorf("expected no HTTP requests for box/generic-link entries, got %d", n)
	}
}

func TestGetFileSkipsAlreadyDownloadedFileOfMatchingSize(t *testing.T) {
	fs := newFakeServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()
	c := newTestClient(t, srv)

	content := "already here"
	if err := os.WriteFile(filepath.Join(c.mirrorDir, "existing.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	fs.downloads["/download/existing"] = &downloadEntry{body: "should never be fetched"}

	c.getFile(context.Background(), "", apiFile{
		Name:        "existing.txt",
		Size:        int64(len(content)),
		DownloadURL: srv.URL + "/download/existing",
		Desc:        "a description that must not be written either",
	})

	if n := fs.totalRequests(); n != 0 {
		t.Errorf("expected no HTTP requests for an already-downloaded file, got %d", n)
	}
	if _, err := os.Stat(filepath.Join(c.mirrorDir, "existing.txt.desc")); !os.IsNotExist(err) {
		t.Error(".desc file should not be written when the download is skipped")
	}
}

// Regression test: the API's own download_url can be wrong for some files;
// getFile must fall back to the group's ordinary web-UI file URL.
func TestGetFileFallsBackToWebURLWhenDownloadURLFails(t *testing.T) {
	fs := newFakeServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()
	c := newTestClient(t, srv) // groupName defaults to "testgroup"

	fs.downloads["/g/testgroup/files/doc.txt"] = &downloadEntry{body: "fallback content"}
	// No entry registered for the primary path, so it 404s on every attempt.

	c.getFile(context.Background(), "", apiFile{
		Name:        "doc.txt",
		Size:        int64(len("fallback content")),
		DownloadURL: srv.URL + "/download/primary-that-fails",
	})

	got, err := os.ReadFile(filepath.Join(c.mirrorDir, "doc.txt"))
	if err != nil {
		t.Fatalf("expected the fallback download to succeed: %v", err)
	}
	if string(got) != "fallback content" {
		t.Errorf("content = %q, want %q", got, "fallback content")
	}
}

// ---- Mirror: end-to-end against a fake groups.io API ----

func TestMirrorEndToEnd(t *testing.T) {
	fs := newFakeServer()
	fs.groupName = "testgroup"
	fs.group = groupResponse{ID: "42", Desc: "Test Group Description"}
	fs.listings["|"] = listResponse{
		TotalCount: 3,
		Data: []apiFile{
			{Name: "sub", IsFolder: true, Desc: "Sub folder desc"},
			{Name: "doc.txt", Desc: "doc description", Size: int64(len("hello world")), DownloadURL: "https://files.groups.io/download/doc.txt"},
			{Name: "boxlink", Type: "file_type_box"},
		},
	}
	fs.listings["sub|"] = listResponse{
		TotalCount: 1,
		Data:       []apiFile{{Name: "nested.txt", Size: int64(len("nested content")), DownloadURL: "https://files.groups.io/download/nested.txt"}},
	}
	fs.downloads["/download/doc.txt"] = &downloadEntry{body: "hello world"}
	fs.downloads["/download/nested.txt"] = &downloadEntry{body: "nested content"}

	srv := httptest.NewServer(fs)
	defer srv.Close()
	useTestServerAsDefaultTransport(t, srv)

	destDir := t.TempDir()
	if err := Mirror(context.Background(), "testgroup", destDir, "a@b.com", "pw"); err != nil {
		t.Fatal(err)
	}

	groupDir := filepath.Join(destDir, "testgroup")
	checkFile := func(rel, want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(groupDir, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%s: got %q, want %q", rel, got, want)
		}
	}
	checkFile("README", "Test Group Description")
	checkFile("doc.txt", "hello world")
	checkFile("doc.txt.desc", "doc description")
	checkFile("sub/README", "Sub folder desc")
	checkFile("sub/nested.txt", "nested content")

	if _, err := os.Stat(filepath.Join(groupDir, "boxlink")); !os.IsNotExist(err) {
		t.Error("a box-type entry should never have been downloaded")
	}

	// Resume: a second run must not re-download an already-complete file.
	before := fs.requestCountFor("/download/doc.txt")
	if err := Mirror(context.Background(), "testgroup", destDir, "a@b.com", "pw"); err != nil {
		t.Fatal(err)
	}
	after := fs.requestCountFor("/download/doc.txt")
	if after != before {
		t.Errorf("already-downloaded doc.txt should not be re-fetched on resume; before=%d after=%d", before, after)
	}
}

func TestMirrorGroupNotFound(t *testing.T) {
	fs := newFakeServer()
	fs.groupName = "realgroup"
	srv := httptest.NewServer(fs)
	defer srv.Close()
	useTestServerAsDefaultTransport(t, srv)

	destDir := t.TempDir()
	if err := Mirror(context.Background(), "wronggroup", destDir, "a@b.com", "pw"); err == nil {
		t.Fatal("expected an error for a group the account can't see")
	}
}

// ---- test helpers: a fake groups.io API server, reachable via a
// transport that redirects any request to it regardless of the URL's own
// host — needed because groupsio.go builds several URLs (apiBase, and the
// getFile fallback URL) against the real groups.io domain. ----

// redirectTransport rewrites every outgoing request's scheme and host to
// target's before delegating to base, so code with hardcoded absolute URLs
// can be pointed at a local httptest.Server without changing that code.
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

// newTestClient builds a client whose http.Client redirects every request
// to srv, regardless of the URL the code under test built it for.
func newTestClient(t *testing.T, srv *httptest.Server) *client {
	t.Helper()
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	return &client{
		http:      &http.Client{Jar: jar, Transport: redirectTransport{target: target}},
		groupName: "testgroup",
		mirrorDir: t.TempDir(),
	}
}

// useTestServerAsDefaultTransport points http.DefaultTransport at srv for
// the duration of the test, restoring it on cleanup. Needed for exercising
// Mirror itself, which builds its own *http.Client with no Transport set.
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

// downloadEntry is one fake file a fakeServer can serve a GET for.
type downloadEntry struct {
	body string
}

// fakeServer is a minimal in-process stand-in for the groups.io REST API:
// login, getgroup, getfiledirectory, and arbitrary file downloads (both the
// API's own download_url and the web-UI fallback URL getFile tries next).
type fakeServer struct {
	mu        sync.Mutex
	requests  []string
	failLogin bool
	groupName string
	group     groupResponse
	listings  map[string]listResponse // key: dirPath + "|" + pageToken
	downloads map[string]*downloadEntry
}

func newFakeServer() *fakeServer {
	return &fakeServer{
		groupName: "testgroup",
		listings:  map[string]listResponse{},
		downloads: map[string]*downloadEntry{},
	}
}

func (s *fakeServer) setFailLogin(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failLogin = v
}

func (s *fakeServer) totalRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *fakeServer) requestCountFor(p string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if r == p {
			n++
		}
	}
	return n
}

func (s *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.URL.Path)
	s.mu.Unlock()

	switch r.URL.Path {
	case "/api/v1/login":
		s.handleLogin(w, r)
	case "/api/v1/getgroup":
		s.handleGetGroup(w, r)
	case "/api/v1/getfiledirectory":
		s.handleGetFileDirectory(w, r)
	default:
		s.handleDownload(w, r)
	}
}

func (s *fakeServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	fail := s.failLogin
	s.mu.Unlock()
	if fail || r.Form.Get("email") == "" || r.Form.Get("password") == "" {
		http.Error(w, `{"type":"unauthorized_error"}`, http.StatusBadRequest)
		return
	}
	w.Write([]byte("{}"))
}

func (s *fakeServer) handleGetGroup(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Query().Get("group_name") != s.groupName {
		// json.Number's zero value marshals as the bare number 0, not "",
		// so an empty groupResponse{} wouldn't actually exercise the
		// group.ID == "" check in Mirror. Write the empty-string form
		// directly, matching what that check assumes the real API sends
		// for an unknown/inaccessible group.
		w.Write([]byte(`{"id":"","desc":""}`))
		return
	}
	json.NewEncoder(w).Encode(s.group)
}

func (s *fakeServer) handleGetFileDirectory(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("path") + "|" + r.URL.Query().Get("page_token")
	s.mu.Lock()
	resp, ok := s.listings[key]
	s.mu.Unlock()
	if !ok {
		resp = listResponse{}
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *fakeServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	entry, ok := s.downloads[r.URL.Path]
	s.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Write([]byte(entry.body))
}
