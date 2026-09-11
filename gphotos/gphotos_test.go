package gphotos

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
