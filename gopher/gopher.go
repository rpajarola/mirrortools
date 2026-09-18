// Package gopher recursively mirrors a Gopher menu tree over the raw Gopher
// protocol (RFC 1436): connect, send the selector, read until the peer
// closes the connection. No third-party dependency is needed — the wire
// protocol is trivial — so this is a from-scratch port of a Python script
// rather than a wrapper around an existing library.
//
// It registers itself as the "gopher" method with mirrortools' mirror
// package, so it's normally driven through the mirror CLI:
//
//	mirror gopher gopher://host/1/some/menu ./mirror
//
// This fixes several bugs found in the Python script it replaces, rather
// than reproducing them:
//
//   - There, an item's (host, selector) pair was marked "seen" on the first
//     attempt regardless of outcome, so the retry loop's second attempt
//     always silently no-op'd — retries never actually happened. Here, an
//     item is marked seen once, up front, and the retry loop that follows
//     genuinely re-attempts the fetch.
//   - There, for a "/1/"-prefixed URL, the "stay within the requested
//     subtree" check compared against a path that still had the literal
//     "/1" segment, which the actual fetches never produce — so the very
//     first fetch of any "/1/..." URL failed that check and the whole run
//     aborted immediately. Fixed by deriving the scope boundary from the
//     same stripped selector the fetches actually use.
//   - There, collected README text (built as bytes) was written via
//     `.encode()`, a str-only method — any menu with an info line would
//     crash. Fixed by treating menu text as raw bytes throughout.
//   - There, only a socket timeout was treated as retryable; a connection
//     refused, a DNS failure, or anything else propagated all the way up
//     and killed the whole run over one bad link. Here, any network error
//     on one item is retried a bounded number of times and then that one
//     item is skipped, logged, and the crawl continues.
//   - There, files were written straight to their final path, so a process
//     killed mid-download left a truncated file that a later run's
//     "already downloaded" check would mistake for a complete one. Fixed
//     with the same write-to-temp-then-rename convention used elsewhere in
//     mirrortools.
//
// One deliberate behavior change: a menu entry naming a different host, or
// the same host on a different port, is not followed — only recursing
// within the same host:port keeps a mirror run scoped to one server, and
// avoids ambiguity about which host's directory tree a cross-host entry's
// content should be filed under. The Python script recursed into these
// (connecting to whatever host the entry named) while still filing the
// result under the original host's directory tree — a divergence that
// looks unintentional rather than a feature worth preserving.
package gopher

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rpajarola/mirrortools/mirror"
)

func init() {
	mirror.Register(&mirror.Method{
		Name:     "gopher",
		Source:   "a gopher:// URL, e.g. gopher://host/1/some/menu or gopher://host/0/some/file.txt",
		Describe: "recursively mirror a Gopher menu tree over the raw Gopher protocol",
		SetupFlags: func(fs *flag.FlagSet) mirror.Func {
			return func(ctx context.Context, source, destDir string) error {
				return Mirror(ctx, source, destDir)
			}
		},
	})
}

// Gopher item type characters this cares about. See RFC 1436 §3.8 for the
// full list; unlisted types are neither followed nor specially handled,
// just logged and skipped.
const (
	typeText  = '0'
	typeMenu  = '1'
	typeError = '3'
	typeInfo  = 'i'
)

// unknownType marks an item whose type isn't known yet — only ever true for
// the single top-level item a Mirror call starts from when its URL's path
// didn't carry an explicit "/1/" menu marker (see Mirror). Every other item
// gets its type from the menu entry that named it.
const unknownType = 0

// followTypes are the item types worth recursing into (or, for a plain
// file, worth ever fetching): text, menu, binhex, dos binary, uuencoded,
// binary, gif, image. Deliberately excludes '2' (CSO phone book) and '7'
// (search) — both need interactive input this tool can't supply — and 'h'
// (HTTP link) and 'T' (telnet) — neither is Gopher content to mirror.
const followTypes = "014569gI"

// direntRe matches one Gopher menu line: type char, display string, tab,
// selector, tab, host, tab, port, optionally followed by more tab-separated
// fields this doesn't care about — a Gopher+ server (e.g. gopher.quux.org)
// appends a "+" field after the port on every line to advertise Gopher+
// support, and the trailing "(?:\t.*)?" keeps that from making an otherwise
// well-formed line fail to match entirely. Anchored so a partial/garbled
// line (e.g. the tail end of a read that got cut off mid-entry) doesn't
// accidentally partially match.
var direntRe = regexp.MustCompile(`^([0-9+TgIih])([^\t]*)\t([^\t]*)\t([^\t]*)\t([0-9]*)(?:\t.*)?$`)

// crlf is the line terminator Gopher uses throughout: between menu entries,
// and after the selector line a client sends.
var crlf = []byte("\r\n")

// tmpSuffix marks a file as a download in progress, the same
// write-then-rename convention used elsewhere in mirrortools: a download
// cut short by a network failure or the process being killed leaves only a
// "*.gopher-tmp" behind, never a truncated file at the real name that a
// later run could mistake for a complete one.
const tmpSuffix = ".gopher-tmp"

const (
	dialTimeout    = 30 * time.Second
	fetchTimeout   = 5 * time.Minute // whole-exchange cap: write selector, read entire response
	maxAttempts    = 5
	initialBackoff = 5 * time.Second
)

// crawl holds the state shared across every item fetched during one Mirror
// run: the fixed host/port everything is fetched from (see the package doc
// comment on why cross-host links aren't followed), the directory
// boundaries, and which selectors have already been dealt with.
type crawl struct {
	hostname string
	port     string
	basedir  string // destDir/hostname — selectors are absolute paths under this
	rootDir  string // basedir/<initial selector> — recursion may not escape this
	seen     map[string]bool
}

// Mirror recursively downloads the Gopher menu tree rooted at source (a
// gopher:// URL) into destDir, which the caller guarantees exists.
//
// Files land at destDir/<host>/<selector>, mirroring the selector path
// structure the server exposes — e.g. gopher://host/1/foo/bar downloads
// into destDir/host/foo/bar/... A menu's "i" (info) lines are collected
// into a 00README file alongside its other entries. Recursion never leaves
// the subtree rooted at the URL's own path, and never follows a link to a
// different host or port (see the package doc comment).
//
// A selector whose local file already exists is skipped without
// re-fetching (a plain resume/skip check, not a checksum — Gopher's
// directory-listing protocol has no per-item size or hash to compare
// against). A selector that resolves to an existing local directory is
// still re-fetched and re-parsed as a menu, so a later run can pick up
// entries a Gopher menu has added since the last mirror.
func Mirror(ctx context.Context, source, destDir string) error {
	u, err := url.Parse(source)
	if err != nil {
		return fmt.Errorf("parsing %q: %w", source, err)
	}
	if u.Scheme != "" && u.Scheme != "gopher" {
		return fmt.Errorf("not a gopher:// URL: %q", source)
	}
	hostname := u.Hostname()
	if hostname == "" {
		return fmt.Errorf("no host in %q", source)
	}
	port := u.Port()
	if port == "" {
		port = "70"
	}

	// A "/1/"-prefixed path names the root as an explicit menu and the "1"
	// isn't part of the selector itself — strip it before it's ever used,
	// both for the scope boundary below and for the actual selector sent
	// to the server, so the two stay consistent (see the package doc
	// comment for what goes wrong when they aren't).
	selector := u.Path
	itemType := byte(unknownType)
	if strings.HasPrefix(selector, "/1/") {
		itemType = typeMenu
		selector = selector[2:]
	}

	c := &crawl{
		hostname: hostname,
		port:     port,
		basedir:  filepath.Join(destDir, hostname),
		seen:     map[string]bool{},
	}
	c.rootDir = filepath.Join(c.basedir, filepath.FromSlash(path.Clean("/"+strings.TrimPrefix(selector, "/"))))

	if _, err := os.Stat(c.rootDir); err == nil {
		log.Printf("gopher: updating %s", source)
	} else {
		log.Printf("gopher: cloning %s", source)
	}

	return c.get(ctx, selector, itemType)
}

// localPath resolves selector to its local filesystem path, rejecting one
// that would land outside rootDir — the boundary of the subtree this
// Mirror call was asked to fetch. Gopher selectors are server-controlled
// strings that end up straight in a menu entry; nothing about the protocol
// guarantees one won't contain ".." or point at an entirely different part
// of the server's tree, so this is checked structurally (via the resolved
// path's relationship to rootDir) rather than trusted.
func (c *crawl) localPath(selector string) (string, error) {
	full := filepath.Join(c.basedir, filepath.FromSlash(path.Clean("/"+strings.TrimPrefix(selector, "/"))))
	rel, err := filepath.Rel(c.rootDir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("selector %q resolves outside the mirrored subtree, skipping", selector)
	}
	return full, nil
}

// get fetches one item (selector, with itemType from the menu entry that
// named it, or unknownType for the one top-level item Mirror starts from)
// if it isn't already seen or already on disk, retrying a bounded number of
// times on any network error before giving up on just this one item and
// letting the rest of the crawl continue.
func (c *crawl) get(ctx context.Context, selector string, itemType byte) error {
	if c.seen[selector] {
		return nil
	}
	c.seen[selector] = true

	localPath, err := c.localPath(selector)
	if err != nil {
		log.Printf("gopher: %v", err)
		return nil
	}

	if fi, err := os.Stat(localPath); err == nil {
		if fi.IsDir() {
			itemType = typeMenu // revisit: the menu may have grown new entries since last time
		} else {
			return nil // plain file already downloaded
		}
	}

	log.Printf("gopher: get %s%s", c.hostname, selector)

	backoff := initialBackoff
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = c.fetch(ctx, selector, itemType, localPath)
		if lastErr == nil {
			return nil
		}
		if attempt == maxAttempts {
			break
		}
		log.Printf("gopher: %s%s: %v, retrying in %s", c.hostname, selector, lastErr, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff *= 2
	}
	log.Printf("gopher: giving up on %s%s: %v", c.hostname, selector, lastErr)
	return nil
}

// fetch performs one attempt at retrieving selector: dial, send the
// selector line, read the first chunk of the response, and either parse it
// as a menu (recursing into its entries) or stream it to disk as a plain
// file — see readMenu and writeFile.
//
// itemType decides which of those two a definite (non-unknownType) type
// commits to without even looking at the response. Only for unknownType —
// the single top-level item a run can start from without an explicit "/1/"
// in its URL — is the first chunk of the actual response sniffed to guess
// which one it is (see sniffIsMenu).
func (c *crawl) fetch(ctx context.Context, selector string, itemType byte, localPath string) error {
	conn, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", net.JoinHostPort(c.hostname, c.port))
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(fetchTimeout))

	// Unblocks the read/write below if ctx is canceled mid-transfer —
	// net.Conn has no context-aware API of its own.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	if _, err := conn.Write(append([]byte(selector), crlf...)); err != nil {
		return err
	}

	first := make([]byte, 8192)
	n, err := conn.Read(first)
	if err != nil && err != io.EOF {
		return err
	}
	first = first[:n]
	if n == 0 {
		log.Printf("gopher: %s%s: empty response", c.hostname, selector)
		return nil
	}

	if itemType == typeMenu || (itemType == unknownType && sniffIsMenu(first)) {
		return c.readMenu(ctx, conn, first, localPath)
	}
	return writeFile(conn, first, localPath)
}

// sniffIsMenu guesses whether chunk — the first bytes of a response whose
// type wasn't already known from a menu entry — looks like a Gopher menu
// rather than arbitrary file content: every complete line in it parses as a
// menu entry, and at least one does. The last line is exempt from that
// check since chunk may have been cut off mid-entry by the read size limit;
// there's no way to tell a genuinely malformed line from a truncated one
// without reading further, so it's simply not held against the guess
// either way.
func sniffIsMenu(chunk []byte) bool {
	lines := bytes.Split(chunk, crlf)
	matched := 0
	for i, line := range lines {
		if len(line) == 0 || string(line) == "." {
			continue
		}
		if direntRe.Match(line) {
			matched++
			continue
		}
		if i == len(lines)-1 {
			continue
		}
		return false
	}
	return matched > 0
}

// writeFile streams a plain (non-menu) item to localPath: first (already
// read before the type was decided) followed by the rest of conn until the
// peer closes it — Gopher has no explicit end-of-file marker for anything
// but a menu, so EOF is the only signal a plain item is complete. Written
// to a temp file and renamed into place only once the copy finishes, so an
// interrupted download never leaves a truncated file at the real name.
func writeFile(conn net.Conn, first []byte, localPath string) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	tmpPath := localPath + tmpSuffix
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	_, writeErr := out.Write(first)
	var copyErr error
	if writeErr == nil {
		_, copyErr = io.Copy(out, conn)
	}
	closeErr := out.Close()
	if writeErr != nil || copyErr != nil || closeErr != nil {
		os.Remove(tmpPath)
		if writeErr != nil {
			return writeErr
		}
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	return os.Rename(tmpPath, localPath)
}

// readMenu drains the rest of a menu response (first, followed by conn
// until EOF), then parses it a line at a time: "i" lines are collected into
// a 00README, entries of a followTypes type on this same host:port are
// recursively fetched via get, and everything else (errors, entries on a
// different host/port, search/telnet/HTTP entries, unparseable lines) is
// logged and skipped rather than treated as fatal to the whole menu.
func (c *crawl) readMenu(ctx context.Context, conn net.Conn, first []byte, localDir string) error {
	buf := append([]byte(nil), first...)
	chunk := make([]byte, 8192)
	for {
		n, err := conn.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if err != nil {
			break // EOF (or a timeout) ends the menu; there's no other terminator to wait for
		}
	}

	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return err
	}

	var readme []byte
	for _, line := range bytes.Split(buf, crlf) {
		if len(line) == 0 || string(line) == "." {
			continue
		}
		m := direntRe.FindSubmatch(line)
		if m == nil {
			log.Printf("gopher: %s: skipping unparseable menu line %q", c.hostname, line)
			continue
		}
		typ := m[1][0]
		display, selector, host, port := m[2], string(m[3]), string(m[4]), string(m[5])

		switch typ {
		case typeInfo:
			readme = append(readme, display...)
			readme = append(readme, '\n')
		case typeError:
			log.Printf("gopher: %s: error entry: %s", c.hostname, display)
		default:
			if strings.IndexByte(followTypes, typ) < 0 {
				log.Printf("gopher: skipping menu entry of type %q: %s", string(typ), selector)
				continue
			}
			if !strings.EqualFold(host, c.hostname) || port != c.port {
				log.Printf("gopher: skipping cross-host entry %s:%s%s", host, port, selector)
				continue
			}
			if err := c.get(ctx, selector, typ); err != nil {
				return err
			}
		}
	}

	if len(readme) > 0 {
		if err := os.WriteFile(filepath.Join(localDir, "00README"), readme, 0o644); err != nil {
			return err
		}
	}
	return nil
}
