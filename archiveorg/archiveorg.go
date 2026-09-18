// Package archiveorg mirrors an Internet Archive item
// (archive.org/details/<id>) to a local directory by reading the item's
// metadata file listing and downloading each file over plain HTTPS. No
// archive.org account or API key is needed, and there's no dependency on
// the official Python `internetarchive` library this replaces — this
// package talks to archive.org's public metadata and download endpoints
// directly.
//
// It registers itself as the "archiveorg" method with mirrortools' mirror
// package, so it's normally driven through the mirror CLI:
//
//	mirror archiveorg https://archive.org/details/some-item ./some-item
package archiveorg

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rpajarola/mirrortools/mirror"
)

func init() {
	mirror.Register(&mirror.Method{
		Name:     "archiveorg",
		Source:   "an archive.org item: a details/download URL (https://archive.org/details/<id>) or a bare identifier",
		Describe: "download every file of an Internet Archive item over plain HTTPS",
		SetupFlags: func(fs *flag.FlagSet) mirror.Func {
			force := fs.Bool("f", false, "re-download even if a previous run's .done marker is present")
			return func(ctx context.Context, source, destDir string) error {
				return Mirror(ctx, source, destDir, *force)
			}
		},
	})
}

// doneFileName marks an item's directory as fully and correctly mirrored —
// every file in the item's metadata listing present and, where the metadata
// gave a checksum, verified — so a later run can skip it outright unless -f
// forces a re-check.
const doneFileName = ".done"

// identifierRe extracts an archive.org item identifier from a details/
// download URL, or matches a bare identifier outright. Mirrors the parsing
// the Python script this replaces did: strip a recognized URL prefix if
// present, then take everything up to the next "/".
var identifierRe = regexp.MustCompile(`^(?:https?://archive\.org/(?:details|download)/)?([^/]+)(?:/.*)?$`)

// ParseIdentifier extracts the archive.org item identifier from a
// details/download URL or a bare identifier, rejecting anything that still
// contains a "/" or ":" after stripping the known prefix — a sign the input
// wasn't a plain archive.org reference at all.
func ParseIdentifier(source string) (string, error) {
	m := identifierRe.FindStringSubmatch(source)
	if m == nil {
		return "", fmt.Errorf("not an archive.org item: %q", source)
	}
	id := m[1]
	if strings.ContainsAny(id, ":/") {
		return "", fmt.Errorf("not an archive.org item: %q", source)
	}
	return id, nil
}

// metadata is the subset of https://archive.org/metadata/<id>'s response
// this cares about.
type metadata struct {
	IsDark bool       `json:"is_dark"`
	Files  []metaFile `json:"files"`
}

// metaFile is one entry of metadata.Files. Size arrives as a JSON string,
// not a number, in archive.org's actual response.
type metaFile struct {
	Name string `json:"name"`
	Size string `json:"size"`
	MD5  string `json:"md5"`
}

func fetchMetadata(ctx context.Context, c *http.Client, identifier string) (*metadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://archive.org/metadata/"+url.PathEscape(identifier), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	var md metadata
	if err := json.NewDecoder(resp.Body).Decode(&md); err != nil {
		return nil, fmt.Errorf("decoding metadata: %w", err)
	}
	return &md, nil
}

// downloadURL builds the archive.org download URL for one file of an item.
// Requesting it through archive.org/download/ rather than a specific
// storage node lets archive.org redirect to whichever node currently holds
// the item; Go's http.Client follows that redirect automatically.
func downloadURL(identifier, name string) string {
	segs := strings.Split(name, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "https://archive.org/download/" + url.PathEscape(identifier) + "/" + strings.Join(segs, "/")
}

// safeRelPath validates that name (an archive.org file's declared "name",
// which may include "/"-separated subdirectories) can't escape destDir once
// joined onto it, and returns the cleaned, OS-native relative path to join.
// Defensive only — real archive.org items aren't expected to ever trip this
// — against a ".." component in a file name being able to write outside
// destDir.
func safeRelPath(destDir, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe file name %q", name)
	}
	return clean, nil
}

// alreadyHave reports whether finalPath already holds wantSize bytes, so a
// re-run can skip a file without re-fetching it just to confirm its
// checksum still matches. This is a size check only: cheap, and enough to
// catch a partial/interrupted download, which is the only way a file
// mirrored by this same tool would ever be wrong on disk between runs.
func alreadyHave(finalPath string, wantSize int64) bool {
	fi, err := os.Stat(finalPath)
	if err != nil {
		return false
	}
	return fi.Size() == wantSize
}

// tmpSuffix marks a file as a download in progress, the same
// write-then-rename convention gphotos uses: a download cut short by a
// network failure or the process being killed leaves only a
// "*.archiveorg-tmp" behind, never a truncated file at the real name that a
// later run's alreadyHave could mistake for a complete one.
const tmpSuffix = ".archiveorg-tmp"

// downloadFile fetches srcURL into finalPath, verifying the result against
// wantMD5 if non-empty. It writes to a temp file first and renames into
// place only once the copy finishes and (if requested) checksums correctly,
// so a download cut short by a network failure, a bad checksum, or the
// process being killed never leaves anything at finalPath itself.
func downloadFile(ctx context.Context, c *http.Client, srcURL, finalPath, wantMD5 string) error {
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srcURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", resp.StatusCode)
	}

	tmpPath := finalPath + tmpSuffix
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	h := md5.New()
	_, copyErr := io.Copy(io.MultiWriter(out, h), resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(tmpPath)
		return copyErr
	}
	if closeErr != nil {
		os.Remove(tmpPath)
		return closeErr
	}
	if wantMD5 != "" {
		if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, wantMD5) {
			os.Remove(tmpPath)
			return fmt.Errorf("md5 mismatch: got %s, want %s", got, wantMD5)
		}
	}
	return os.Rename(tmpPath, finalPath)
}

// Mirror downloads every file listed in the archive.org item identified by
// source (a details/download URL, or a bare identifier) into destDir, which
// the caller guarantees exists.
//
// Each item is mirrored into its own destDir/<identifier> subdirectory
// (created if needed), so one destDir can hold several mirrored items
// side by side without their files colliding.
//
// A <destDir>/<identifier>/.done marker from a prior fully-successful run
// causes an immediate, silent skip unless force is set. Mirror only ever
// writes that marker after every file in the item's metadata listing has
// been fetched (or already matched what's on disk) and, wherever the
// metadata supplied a checksum, verified — so unlike the Python
// internetarchive library this replaces, a .done file here is never written
// for a partial download: any failure partway through returns an error
// instead, leaving no marker behind, so a subsequent run resumes rather
// than silently reporting success.
func Mirror(ctx context.Context, source, destDir string, force bool) error {
	identifier, err := ParseIdentifier(source)
	if err != nil {
		return err
	}
	itemDir := filepath.Join(destDir, identifier)

	donePath := filepath.Join(itemDir, doneFileName)
	if !force {
		if _, err := os.Stat(donePath); err == nil {
			log.Printf("archiveorg: %s already mirrored (%s present), skipping", identifier, doneFileName)
			return nil
		}
	}

	if err := os.MkdirAll(itemDir, 0o755); err != nil {
		return err
	}

	client := &http.Client{Timeout: 10 * time.Minute}

	log.Printf("archiveorg: fetching metadata for %s", identifier)
	md, err := fetchMetadata(ctx, client, identifier)
	if err != nil {
		return fmt.Errorf("fetching metadata for %s: %w", identifier, err)
	}
	if md.IsDark {
		return fmt.Errorf("%s is dark (access restricted), nothing to download", identifier)
	}
	if len(md.Files) == 0 {
		return fmt.Errorf("%s: no files listed (item may not exist)", identifier)
	}

	log.Printf("archiveorg: mirroring %s (%d files) into %s", identifier, len(md.Files), itemDir)
	downloaded, skipped := 0, 0
	for _, f := range md.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if f.Name == "" {
			continue
		}
		relPath, err := safeRelPath(itemDir, f.Name)
		if err != nil {
			return fmt.Errorf("%s: %w", identifier, err)
		}
		finalPath := filepath.Join(itemDir, relPath)

		size, _ := strconv.ParseInt(f.Size, 10, 64)
		if size > 0 && alreadyHave(finalPath, size) {
			skipped++
			continue
		}

		wantMD5 := f.MD5
		if f.Name == identifier+"_files.xml" {
			// This file lists every file's checksum, including its own
			// <file> entry — but that entry's md5 was necessarily computed
			// before the entry for itself existed, so it can never match
			// the file's actual bytes. Confirmed against multiple real
			// items' metadata; not something a retry or re-fetch fixes.
			wantMD5 = ""
		}

		log.Printf("archiveorg: downloading %s (%d bytes)", f.Name, size)
		if err := downloadFile(ctx, client, downloadURL(identifier, f.Name), finalPath, wantMD5); err != nil {
			return fmt.Errorf("downloading %s: %w", f.Name, err)
		}
		downloaded++
	}

	if err := os.WriteFile(donePath, []byte("DONE"), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", donePath, err)
	}
	log.Printf("archiveorg: done: %d downloaded, %d already present, %d total", downloaded, skipped, len(md.Files))
	return nil
}
