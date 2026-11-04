package sourceforge

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseSource(t *testing.T) {
	for _, tc := range []struct {
		source, project, path string
		wantErr               bool
	}{
		{"https://sourceforge.net/projects/fsd-amoeba/", "fsd-amoeba", "", false},
		{"https://sourceforge.net/projects/fsd-amoeba", "fsd-amoeba", "", false},
		{"http://www.sourceforge.net/projects/fsd-amoeba/", "fsd-amoeba", "", false},
		{"https://sf.net/projects/fsd-amoeba/", "fsd-amoeba", "", false},
		{"https://sourceforge.net/p/fsd-amoeba/", "fsd-amoeba", "", false},
		{"https://sourceforge.net/p/fsd-amoeba/files/x/", "fsd-amoeba", "", false},
		{"https://sourceforge.net/projects/fsd-amoeba/reviews/", "fsd-amoeba", "", false},
		{"https://sourceforge.net/projects/fsd-amoeba/files/", "fsd-amoeba", "", false},
		{"https://sourceforge.net/projects/fsd-amoeba/files/fsd-amoeba/fsd-2002a/", "fsd-amoeba", "fsd-amoeba/fsd-2002a", false},
		{"https://sourceforge.net/projects/fsd-amoeba/files/fsd-amoeba/fsd-2002a/README/download", "fsd-amoeba", "fsd-amoeba/fsd-2002a/README", false},
		{"https://sourceforge.net/projects/x/files/a%20b/c.zip?source=navbar#top", "x", "a b/c.zip", false},
		{"https://downloads.sourceforge.net/project/fsd-amoeba/fsd-amoeba/fsd-2002a/README", "fsd-amoeba", "fsd-amoeba/fsd-2002a/README", false},
		{"https://sourceforge.net/projects/x/files/../../etc/", "", "", true},
		{"https://sourceforge.net/", "", "", true},
		{"https://sourceforge.net/directory/", "", "", true},
		{"https://example.com/projects/x/", "", "", true},
		{"ftp://sourceforge.net/projects/x/", "", "", true},
		{"fsd-amoeba", "", "", true},
	} {
		project, p, err := ParseSource(tc.source)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tc.source, err, tc.wantErr)
			continue
		}
		if project != tc.project || p != tc.path {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.source, project, p, tc.project, tc.path)
		}
	}
}

func TestParseListing(t *testing.T) {
	page := []byte(`<html><script>
net.sf.files = {"b":{"name":"b","type":"f","md5":"abc","downloadable":true,"link":""},"a":{"name":"a","type":"d"}};
net.sf.staging_days = 3;
</script></html>`)
	entries, err := parseListing(page)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "a" || entries[0].Type != "d" ||
		entries[1].Name != "b" || entries[1].MD5 != "abc" || !entries[1].Downloadable {
		t.Errorf("got %+v", entries)
	}
	if _, err := parseListing([]byte("<html>not a files page</html>")); err != errNoListing {
		t.Errorf("page without a listing: got %v, want errNoListing", err)
	}
}

// fakeSF serves a fake SourceForge project, both its files-area pages and
// downloads, the latter through a redirect to a "mirror" as the real
// downloads.sourceforge.net does.
type fakeSF struct {
	project string
	// tree maps a folder path ("" for the top) to its entries.
	tree map[string][]entry
	// content maps a file path to its contents.
	content map[string]string
	// broken lists files whose downloads fail with a 404.
	broken map[string]bool

	mu        sync.Mutex
	downloads map[string]int
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

var uploaded = time.Date(2002, 2, 5, 9, 29, 48, 0, time.UTC)

func (f *fakeSF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	filesPrefix := "/projects/" + f.project + "/files/"
	switch {
	case strings.HasPrefix(r.URL.Path, filesPrefix):
		p := strings.Trim(strings.TrimPrefix(r.URL.Path, filesPrefix), "/")
		if _, ok := f.content[p]; ok {
			http.Redirect(w, r, r.URL.Path+"download", http.StatusMovedPermanently)
			return
		}
		entries, ok := f.tree[p]
		if !ok {
			http.NotFound(w, r)
			return
		}
		byName := map[string]entry{}
		for _, e := range entries {
			byName[e.Name] = e
		}
		js, _ := json.Marshal(byName)
		fmt.Fprintf(w, "<html><head><title>boilerplate</title></head><body><script>\nnet.sf.files = %s;\n</script></body></html>", js)
	case strings.HasPrefix(r.URL.Path, "/project/"+f.project+"/"):
		p := strings.TrimPrefix(r.URL.Path, "/project/"+f.project+"/")
		http.Redirect(w, r, "/mirror/"+p+"?viasf=1", http.StatusFound)
	case strings.HasPrefix(r.URL.Path, "/mirror/"):
		p := strings.TrimPrefix(r.URL.Path, "/mirror/")
		f.mu.Lock()
		f.downloads[p]++
		f.mu.Unlock()
		c, ok := f.content[p]
		if !ok || f.broken[p] {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Last-Modified", uploaded.Format(http.TimeFormat))
		fmt.Fprint(w, c)
	default:
		http.NotFound(w, r)
	}
}

func newFake(t *testing.T) *fakeSF {
	f := &fakeSF{
		project: "proj",
		content: map[string]string{
			"README":              "top readme",
			"rel/1.0/a.tgz":       "a contents",
			"rel/1.0/b c.txt":     "b contents",
			"rel/2.0/no-md5.bin":  "no checksum here",
			"rel/2.0/staged.bin":  "not yet",
			"rel/2.0/bad-md5.bin": "corrupted",
		},
		downloads: map[string]int{},
	}
	file := func(name, p string) entry {
		return entry{Name: name, Type: "f", MD5: md5hex(f.content[p]), Downloadable: true}
	}
	f.tree = map[string][]entry{
		"": {
			file("README", "README"),
			{Name: "rel", Type: "d"},
			{Name: "elsewhere", Type: "f", Link: "https://example.com/x", Downloadable: true},
		},
		"rel": {{Name: "1.0", Type: "d"}, {Name: "2.0", Type: "d"}},
		"rel/1.0": {
			file("a.tgz", "rel/1.0/a.tgz"),
			file("b c.txt", "rel/1.0/b c.txt"),
		},
		"rel/2.0": {
			{Name: "no-md5.bin", Type: "f", Downloadable: true},
			{Name: "staged.bin", Type: "f", MD5: md5hex("not yet")},
			{Name: "bad-md5.bin", Type: "f", MD5: md5hex("the real thing"), Downloadable: true},
		},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	oldSite, oldDL, oldBase := siteBase, downloadBase, retryBase
	siteBase, downloadBase, retryBase = srv.URL, srv.URL, time.Millisecond
	t.Cleanup(func() { siteBase, downloadBase, retryBase = oldSite, oldDL, oldBase })
	return f
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMirror(t *testing.T) {
	f := newFake(t)
	dest := t.TempDir()
	ctx := context.Background()

	err := Mirror(ctx, "https://sourceforge.net/projects/proj/", dest, Options{Retries: 1})
	if err == nil || !strings.Contains(err.Error(), "/rel/2.0/bad-md5.bin") {
		t.Fatalf("got %v, want an error naming bad-md5.bin", err)
	}
	if strings.Contains(err.Error(), "README") || strings.Contains(err.Error(), "staged") {
		t.Errorf("error names files that didn't fail: %v", err)
	}
	for p, want := range map[string]string{
		"README":             "top readme",
		"rel/1.0/a.tgz":      "a contents",
		"rel/1.0/b c.txt":    "b contents",
		"rel/2.0/no-md5.bin": "no checksum here",
	} {
		local := filepath.Join(dest, "proj", filepath.FromSlash(p))
		if got := readFile(t, local); got != want {
			t.Errorf("%s: got %q, want %q", p, got, want)
		}
		fi, _ := os.Stat(local)
		if !fi.ModTime().Equal(uploaded) {
			t.Errorf("%s: mtime %v, want %v", p, fi.ModTime(), uploaded)
		}
	}
	for _, p := range []string{"elsewhere", "rel/2.0/staged.bin", "rel/2.0/bad-md5.bin", "rel/2.0/bad-md5.bin" + tmpSuffix} {
		if _, err := os.Stat(filepath.Join(dest, "proj", filepath.FromSlash(p))); !os.IsNotExist(err) {
			t.Errorf("%s: should not exist, stat err = %v", p, err)
		}
	}
	if n := f.downloads["rel/2.0/bad-md5.bin"]; n != 2 {
		t.Errorf("bad-md5.bin downloaded %d times, want 2 (one retry)", n)
	}

	// A second run downloads only what is missing or changed.
	f.tree["rel/2.0"] = f.tree["rel/2.0"][:1]
	local := filepath.Join(dest, "proj", "rel", "1.0", "a.tgz")
	if err := os.WriteFile(local, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	for k := range f.downloads {
		delete(f.downloads, k)
	}
	if err := Mirror(ctx, "https://sourceforge.net/projects/proj/files/", dest, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, local); got != "a contents" {
		t.Errorf("changed file not fetched again: got %q", got)
	}
	if len(f.downloads) != 1 || f.downloads["rel/1.0/a.tgz"] != 1 {
		t.Errorf("second run downloaded %v, want only rel/1.0/a.tgz", f.downloads)
	}
}

func TestMirrorSubtree(t *testing.T) {
	f := newFake(t)
	dest := t.TempDir()
	if err := Mirror(context.Background(), "https://sourceforge.net/projects/proj/files/rel/1.0/", dest, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dest, "proj", "rel", "1.0", "b c.txt")); got != "b contents" {
		t.Errorf("got %q", got)
	}
	if len(f.downloads) != 2 {
		t.Errorf("downloaded %v, want just the two files of rel/1.0", f.downloads)
	}
}

func TestMirrorSingleFile(t *testing.T) {
	f := newFake(t)
	dest := t.TempDir()
	if err := Mirror(context.Background(), "https://sourceforge.net/projects/proj/files/rel/1.0/a.tgz/download", dest, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dest, "proj", "rel", "1.0", "a.tgz")); got != "a contents" {
		t.Errorf("got %q", got)
	}
	if len(f.downloads) != 1 {
		t.Errorf("downloaded %v, want just a.tgz", f.downloads)
	}
}

func TestMirrorMissingProject(t *testing.T) {
	newFake(t)
	err := Mirror(context.Background(), "https://sourceforge.net/projects/nope/", t.TempDir(), Options{})
	if err == nil || !strings.Contains(err.Error(), "does the project exist") {
		t.Errorf("got %v, want a missing-project error", err)
	}
}

func TestMirrorUnsafeName(t *testing.T) {
	f := newFake(t)
	f.tree[""] = []entry{{Name: "..", Type: "d"}, {Name: "a/../../x", Type: "f", Downloadable: true}}
	dest := t.TempDir()
	err := Mirror(context.Background(), "https://sourceforge.net/projects/proj/", dest, Options{})
	if err == nil || !strings.Contains(err.Error(), "2 failed") {
		t.Errorf("got %v, want both unsafe entries to fail", err)
	}
	if len(f.downloads) != 0 {
		t.Errorf("downloaded %v", f.downloads)
	}
}

func TestDownloadRejectsWebPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html>please wait</html>")
	}))
	defer srv.Close()
	final := filepath.Join(t.TempDir(), "file.zip")
	err := downloadFile(context.Background(), srv.Client(), srv.URL+"/file.zip", final, "")
	if err == nil || !strings.Contains(err.Error(), "web page") {
		t.Errorf("got %v, want a web page error", err)
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Errorf("file written despite the error")
	}
}
