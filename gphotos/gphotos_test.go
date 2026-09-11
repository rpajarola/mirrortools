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

func TestClaimFilenameCollisions(t *testing.T) {
	dir := t.TempDir()
	used := map[string]bool{}

	a := claimFilename(dir, "IMG_0001.jpg", used)
	if a != "IMG_0001.jpg" {
		t.Fatalf("first claim: got %q", a)
	}
	if err := os.WriteFile(filepath.Join(dir, a), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Same name claimed again within the same run must not collide with the
	// first claim, or with the file it wrote to disk.
	b := claimFilename(dir, "IMG_0001.jpg", used)
	if b != "IMG_0001-2.jpg" {
		t.Fatalf("second claim (in-run collision): got %q", b)
	}

	// A file already on disk from a prior run must also never be silently
	// overwritten, even with a fresh `used` map.
	c := claimFilename(dir, "IMG_0001.jpg", map[string]bool{})
	if c == "IMG_0001.jpg" {
		t.Fatalf("would overwrite existing file: got %q", c)
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
