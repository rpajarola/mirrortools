package gphotos

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	idx.record("mk1", "IMG_0001.jpg", 12345)
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
