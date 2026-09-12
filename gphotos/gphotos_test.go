package gphotos

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseBatchFilenames(t *testing.T) {
	// Shape: raw[0] = [ <ignored>, [items...] ], matching safe_get(data, 0,
	// 1) from the reference client — data[0][1] is the item list, each item
	// = [media_key, [desc_at_2, file_name_at_3, ...]] (0-indexed).
	raw := json.RawMessage(`[["ignored",[["mk1",[null,null,null,"IMG_0001.jpg"]],["mk2",[null,null,null,"video.mp4"]]]]]`)

	names, err := parseBatchFilenames(raw)
	if err != nil {
		t.Fatal(err)
	}
	if names["mk1"] != "IMG_0001.jpg" || names["mk2"] != "video.mp4" {
		t.Fatalf("got %#v", names)
	}
}

func TestParseBatchFilenamesBadShape(t *testing.T) {
	if _, err := parseBatchFilenames(json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for unexpected response shape")
	}
}

func TestLooksLikeHTML(t *testing.T) {
	cases := []struct {
		body []byte
		want bool
	}{
		{[]byte(`<!doctype html><html>...`), true},
		{[]byte("  \n<HTML><head>"), true},
		{[]byte("<!DOCTYPE HTML>"), true},
		{[]byte("\xff\xd8\xff\xe0JFIF"), false},         // real jpeg magic
		{[]byte("PK\x03\x04"), false},                   // real zip magic
		{[]byte("<?xml version=\"1.0\"?><rss>"), false}, // not html specifically
		{[]byte(""), false},
	}
	for _, c := range cases {
		if got := looksLikeHTML(c.body); got != c.want {
			t.Errorf("looksLikeHTML(%q) = %v, want %v", c.body, got, c.want)
		}
	}
}

func TestResolveFilenameFreeName(t *testing.T) {
	dir := t.TempDir()
	name, adopted := resolveFilename(dir, "IMG_0001.jpg", "mk1", map[string]bool{})
	if name != "IMG_0001.jpg" || adopted {
		t.Fatalf("got %q, adopted=%v", name, adopted)
	}
}

func TestResolveFilenameEmptyNameFallsBackToMediaKey(t *testing.T) {
	dir := t.TempDir()
	name, adopted := resolveFilename(dir, "", "mk1", map[string]bool{})
	if name != "mk1" || adopted {
		t.Fatalf("got %q, adopted=%v", name, adopted)
	}
}

func TestResolveFilenameAdoptsUnknownExistingFile(t *testing.T) {
	// A file already on disk that nothing (this run or the index) has
	// claimed yet is assumed to be this same item from before the index
	// existed — e.g. exactly what happens the first run after adding the
	// index to a directory a pre-index run already populated.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "IMG_0001.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, adopted := resolveFilename(dir, "IMG_0001.jpg", "mk1", map[string]bool{})
	if name != "IMG_0001.jpg" || !adopted {
		t.Fatalf("got %q, adopted=%v, want adopted", name, adopted)
	}
}

func TestResolveFilenameDisambiguatesKnownCollision(t *testing.T) {
	// A name already in `used` (claimed by a different, already-known item
	// — from the index or earlier this run) is a genuine collision between
	// two distinct items and must never be silently adopted or overwritten.
	dir := t.TempDir()
	used := map[string]bool{"IMG_0001.jpg": true} // claimed by some other media key

	name, adopted := resolveFilename(dir, "IMG_0001.jpg", "mk2", used)
	if name != "IMG_0001-2.jpg" || adopted {
		t.Fatalf("got %q, adopted=%v", name, adopted)
	}

	// A second distinct item wanting the same name gets the next suffix.
	name2, adopted2 := resolveFilename(dir, "IMG_0001.jpg", "mk3", used)
	if name2 != "IMG_0001-3.jpg" || adopted2 {
		t.Fatalf("got %q, adopted=%v", name2, adopted2)
	}
}

func TestRawCoverRegex(t *testing.T) {
	cases := []struct {
		name      string
		wantGroup string
		wantMatch bool
	}{
		{"PXL_20260911_103945315.RAW-01.COVER.jpg", "20260911_103945315", true},
		{"PXL_20260911_030743453.RAW-01.MP.COVER.jpg", "20260911_030743453", true},
		{"pxl_20260911_103945315.raw-01.cover.jpg", "20260911_103945315", true}, // case-insensitive
		{"PXL_20260912_113018488.RAW-02.ORIGINAL.dng", "", false},               // the companion itself, not a cover
		{"IMG_0001.jpg", "", false},
		{"PXL_20260911_103945315.jpg", "", false}, // plain Pixel photo, no RAW pair
	}
	for _, c := range cases {
		m := rawCoverRe.FindStringSubmatch(c.name)
		if c.wantMatch != (m != nil) {
			t.Errorf("rawCoverRe.MatchString(%q) = %v, want %v", c.name, m != nil, c.wantMatch)
			continue
		}
		if c.wantMatch && m[1] != c.wantGroup {
			t.Errorf("rawCoverRe group for %q = %q, want %q", c.name, m[1], c.wantGroup)
		}
	}
}

func TestParseRawGroup(t *testing.T) {
	// Shape confirmed against real captured traffic for wgZjtc: raw[2] is
	// the item list, each item in the same shape as an lcxiM library item.
	raw := json.RawMessage(`[null,null,[["mk-jpeg",["https://example.com/pw/AAA",4000,3000],1000,"dedup-jpeg"],["mk-dng",["https://example.com/pw/BBB",4140,3100],1000,"dedup-dng"]],"20260912_113018488"]`)

	items, err := parseRawGroup(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items: %#v", len(items), items)
	}
	if items[0].MediaKey != "mk-jpeg" || items[0].BaseURL != "https://example.com/pw/AAA" {
		t.Fatalf("item 0: got %#v", items[0])
	}
	if items[1].MediaKey != "mk-dng" || items[1].BaseURL != "https://example.com/pw/BBB" {
		t.Fatalf("item 1: got %#v", items[1])
	}
}

func TestParseRawGroupBadShape(t *testing.T) {
	if _, err := parseRawGroup(json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for unexpected response shape")
	}
}

func TestFetchRawCompanionOriginal(t *testing.T) {
	var gotPath, gotReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotReferer = r.Header.Get("Referer")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("raw bytes"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	item := libraryItem{MediaKey: "mk-dng", BaseURL: srv.URL + "/dl"}
	if err := fetchRawCompanionOriginal(context.Background(), srv.Client(), item, "photo.dng", dir); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "photo.dng"))
	if err != nil || !bytes.Equal(got, []byte("raw bytes")) {
		t.Fatalf("final file: got %q, %v", got, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*"+tmpSuffix)); len(matches) != 0 {
		t.Fatalf("leftover temp file(s): %v", matches)
	}
	if !strings.HasSuffix(gotPath, "=s0-d-I") {
		t.Fatalf("expected the =s0-d-I suffix, got path %q", gotPath)
	}
	if gotReferer != "https://photos.google.com/" {
		t.Fatalf("Referer header: got %q", gotReferer)
	}
}

func TestFetchRawCompanionOriginalRejectsSignInRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<!doctype html><html><body>sign in</body></html>"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	item := libraryItem{MediaKey: "mk-dng", BaseURL: srv.URL + "/dl"}
	if err := fetchRawCompanionOriginal(context.Background(), srv.Client(), item, "photo.dng", dir); err == nil {
		t.Fatal("expected an error for an HTML response")
	}
	if _, err := os.Stat(filepath.Join(dir, "photo.dng")); !os.IsNotExist(err) {
		t.Fatalf("HTML should never be written to the final name, stat err = %v", err)
	}
}

func TestDownloadCompanionsNoMatchReturnsNil(t *testing.T) {
	// A cover filename that doesn't match a Pixel RAW+JPEG pair must bail
	// out before making any network call (nil client would panic if it
	// tried).
	item := libraryItem{MediaKey: "mk1"}
	extra, err := downloadCompanions(context.Background(), nil, nil, item, "IMG_0001.jpg", t.TempDir(), map[string]bool{})
	if err != nil || extra != nil {
		t.Fatalf("got %v, %v; want nil, nil", extra, err)
	}
}

func TestIndexEntryFilenames(t *testing.T) {
	if got := (indexEntry{}).filenames(); got != nil {
		t.Fatalf("empty entry: got %v", got)
	}
	e := indexEntry{Filename: "IMG_0001.jpg", ExtraFilenames: []string{"IMG_0001.dng"}}
	got := e.filenames()
	if len(got) != 2 || got[0] != "IMG_0001.jpg" || got[1] != "IMG_0001.dng" {
		t.Fatalf("got %v", got)
	}
}

func TestDownloadOriginalWritesViaTempThenRename(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("photo bytes"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	item := libraryItem{MediaKey: "mk1", BaseURL: srv.URL + "/photo"}
	if err := downloadOriginal(context.Background(), srv.Client(), item, "photo.jpg", dir); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "photo.jpg"))
	if err != nil || !bytes.Equal(got, []byte("photo bytes")) {
		t.Fatalf("final file: got %q, %v", got, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*"+tmpSuffix)); len(matches) != 0 {
		t.Fatalf("leftover temp file(s) after successful download: %v", matches)
	}
}

func TestDownloadOriginalCanceledLeavesNoFinalFile(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-block // hang until the client gives up, simulating a cut-short download
	}))
	defer srv.Close()
	defer close(block)

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	item := libraryItem{MediaKey: "mk1", BaseURL: srv.URL + "/photo"}
	if err := downloadOriginal(ctx, srv.Client(), item, "photo.jpg", dir); err == nil {
		t.Fatal("expected an error from a canceled download")
	}

	if _, err := os.Stat(filepath.Join(dir, "photo.jpg")); !os.IsNotExist(err) {
		t.Fatalf("final file should not exist after a canceled download, stat err = %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*"+tmpSuffix)); len(matches) != 0 {
		t.Fatalf("temp file should have been cleaned up, found: %v", matches)
	}
}

func TestRemoveStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "IMG_0001.jpg"+tmpSuffix)
	keep := filepath.Join(dir, "IMG_0002.jpg")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := removeStaleTempFiles(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale temp file should be gone, stat err = %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unrelated file should be untouched: %v", err)
	}
}

func TestParseLast(t *testing.T) {
	ref := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		last string
		want time.Time
	}{
		{"30d", ref.AddDate(0, 0, -30)},
		{"2w", ref.AddDate(0, 0, -14)},
		// AddDate(0,-1,0) on Mar 31 lands on "Feb 31", which Go normalizes by
		// overflowing into March (Feb 2026 has 28 days) rather than clamping.
		{"1m", time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)},
		{"1y", time.Date(2025, 3, 31, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, err := parseLast(c.last, ref)
		if err != nil {
			t.Errorf("parseLast(%q): %v", c.last, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("parseLast(%q) = %v, want %v", c.last, got, c.want)
		}
	}
}

func TestParseLastInvalid(t *testing.T) {
	for _, bad := range []string{"", "30", "d", "30x", "-5d", "1.5m"} {
		if _, err := parseLast(bad, time.Now()); err == nil {
			t.Errorf("parseLast(%q): expected error", bad)
		}
	}
}

func TestDownloadIndexRoundTrip(t *testing.T) {
	dir := t.TempDir()

	idx, err := loadDownloadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if idx.has("mk1") {
		t.Fatal("fresh index should not have mk1")
	}
	idx.record("mk1", []string{"IMG_0001.jpg"}, 12345)
	if !idx.has("mk1") {
		t.Fatal("record then has should be true")
	}
	if err := idx.save(); err != nil {
		t.Fatal(err)
	}

	idx2, err := loadDownloadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !idx2.has("mk1") {
		t.Fatal("reloaded index should still have mk1")
	}
	if idx2.entries["mk1"].Filename != "IMG_0001.jpg" || idx2.entries["mk1"].TimestampMS != 12345 {
		t.Fatalf("got %#v", idx2.entries["mk1"])
	}
	if idx2.has("mk2") {
		t.Fatal("reloaded index should not have unrelated mk2")
	}
}
