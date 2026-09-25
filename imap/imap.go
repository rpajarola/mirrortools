// Package imap mirrors an IMAP mailbox, or an entire account's folder tree,
// to a local directory using github.com/emersion/go-imap/v2, a pure-Go IMAP
// client — no external `imapsync`/`fetchmail`-style tool required.
//
// It registers itself as the "imap" method with mirrortools' mirror
// package, so it's normally driven through the mirror CLI:
//
//	mirror imap -user me@example.com -password-file pw.txt imap.example.com ./mail
//	mirror imap -user me@example.com -password-file pw.txt -mailbox Archive imap.example.com ./mail
//
// With no -mailbox flag, the whole account is mirrored: every folder the
// server lists, laid out under destDir in the same hierarchy the server
// reports (using its own delimiter, e.g. "/" or "."), each folder's messages
// landing in its own subdirectory — so destDir/INBOX holds the inbox,
// destDir/Archive/2024 holds a folder nested under "Archive", and so on. A
// folder the server marks \Noselect (a pure hierarchy node with no messages
// of its own, e.g. a "[Gmail]" grouping folder) is skipped; its selectable
// children are still mirrored into their own subdirectories. A single
// folder's failure — a permissions error, a transient server error — is
// logged and skipped rather than aborting the rest of the account, unless
// every folder fails. Passing -mailbox restricts this to exactly one folder,
// mirrored directly into destDir (no subdirectory), same as earlier versions
// of this tool always did.
//
// Each message is saved as <folder-dir>/<uid>.eml, its raw RFC 822 form —
// nothing is parsed or reformatted, so the file is exactly what a real mail
// client would show, and can be fed to any tool that reads .eml files.
//
// Every mailbox is opened read-only (EXAMINE, not SELECT) and every fetch
// uses IMAP's PEEK option, so mirroring never marks messages as read or
// otherwise changes anything on the server — a mirror is a pure read
// operation.
//
// Incremental updates are UID-based, not a full re-listing, and tracked per
// folder: <folder-dir>/.last-uid records the highest UID mirrored so far in
// that folder, and each run asks the server only for UIDs above that (a
// single UID FETCH <last+1>:* — cheap even against a huge mailbox, since it
// costs nothing proportional to how many messages were already mirrored).
// <folder-dir>/.uidvalidity records the folder's UIDVALIDITY; if the server
// reports a different one on a later run — it's entitled to, e.g. after
// certain kinds of mailbox reorganization — UIDs from before aren't
// guaranteed to mean the same messages any more, so that folder's mirroring
// starts over from UID 1. This never deletes anything already on disk, so a
// UIDVALIDITY change costs a redundant re-download rather than losing data;
// already-present files are still skipped via the usual exists-on-disk
// check.
package imap

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/rpajarola/mirrortools/mirror"
)

func init() {
	mirror.Register(&mirror.Method{
		Name:     "imap",
		Source:   "an IMAP server: host or host:port (always implicit TLS; port defaults to 993)",
		Describe: "mirror an IMAP account into destDir as one .eml file per message, one subdirectory per folder",
		SetupFlags: func(fs *flag.FlagSet) mirror.Func {
			user := fs.String("user", "", "IMAP account username (required)")
			passwordFile := fs.String("password-file", "", "path to a file containing the account's password, "+
				"one line — kept out of the command line and shell history (required)")
			mailbox := fs.String("mailbox", "", "mailbox to mirror; if unset, every folder in the account is mirrored")
			return func(ctx context.Context, source, destDir string) error {
				if *user == "" {
					return fmt.Errorf("-user is required")
				}
				if *passwordFile == "" {
					return fmt.Errorf("-password-file is required")
				}
				pw, err := os.ReadFile(*passwordFile)
				if err != nil {
					return fmt.Errorf("reading -password-file: %w", err)
				}
				password := strings.TrimSpace(string(pw))
				return Mirror(ctx, source, destDir, *user, password, *mailbox)
			}
		},
	})
}

const defaultPort = "993"

const dialTimeout = 30 * time.Second

// tlsConfig builds the TLS configuration used to connect to host. A var, not
// a plain function call, solely so tests can substitute a config that trusts
// a locally-generated test certificate instead of the system root CAs;
// production behavior — verify against the system roots, like any other TLS
// client — is unaffected.
var tlsConfig = func(host string) *tls.Config {
	return &tls.Config{ServerName: host}
}

// tmpSuffix marks a file as a download in progress, the same
// write-then-rename convention used elsewhere in mirrortools: a download cut
// short by a network failure or the process being killed leaves only a
// "*.imap-tmp" behind, never a truncated file at the real name that a later
// run could mistake for a complete one.
const tmpSuffix = ".imap-tmp"

const (
	uidValidityFile = ".uidvalidity"
	lastUIDFile     = ".last-uid"
)

// Mirror connects to the IMAP server named by source and mirrors either one
// mailbox (if mailbox is non-empty), directly into destDir, or, if mailbox
// is empty, every folder the account has, each into its own subdirectory of
// destDir. destDir is guaranteed to already exist by the caller. See the
// package doc comment for the on-disk layout and the incremental-update
// strategy.
func Mirror(ctx context.Context, source, destDir, user, password, mailbox string) error {
	addr := source
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, defaultPort)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("imap: %w", err)
	}

	log.Printf("imap: connecting to %s", addr)
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("imap: dialing %s: %w", addr, err)
	}
	tlsConn := tls.Client(conn, tlsConfig(host))

	client := imapclient.New(tlsConn, nil)
	defer client.Close()

	// Unblocks whatever command is in flight if ctx is canceled — imapclient
	// has no per-command context, so the only way to abort a blocking
	// command is to close the underlying connection out from under it.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			client.Close()
		case <-done:
		}
	}()

	if err := client.Login(user, password).Wait(); err != nil {
		return fmt.Errorf("imap: login: %w", err)
	}
	// A plain `defer client.Logout().Wait()` would call Logout() (which
	// sends the LOGOUT command) immediately, deferring only Wait() — the
	// classic `defer f().Method()` gotcha. That sent LOGOUT before the
	// SELECT below, corrupting the session. Deferring the whole call inside
	// a closure defers Logout() itself too.
	defer func() { client.Logout().Wait() }()

	if mailbox != "" {
		if err := mirrorMailbox(client, mailbox, destDir); err != nil {
			return fmt.Errorf("imap: %w", err)
		}
		return nil
	}
	return mirrorAllMailboxes(client, destDir)
}

// mirrorAllMailboxes lists every folder the account has and mirrors each
// selectable one into its own subdirectory of destDir, named after the
// folder's own hierarchy (see the package doc comment). A single folder's
// error is logged and skipped, same as one item failing elsewhere in
// mirrortools (e.g. groupsio's directory walk) — one bad folder shouldn't
// cost everything else already mirrored — except that if every folder
// fails, that's surfaced as an error rather than silently doing nothing.
func mirrorAllMailboxes(client *imapclient.Client, destDir string) error {
	mailboxes, err := client.List("", "*", nil).Collect()
	if err != nil {
		return fmt.Errorf("imap: listing folders: %w", err)
	}

	log.Printf("imap: mirroring %d folder(s) into %s", len(mailboxes), destDir)

	var mirrored, failed int
	for _, mb := range mailboxes {
		if hasAttr(mb.Attrs, imapv2.MailboxAttrNoSelect) {
			continue
		}
		dir, err := mailboxDir(destDir, mb.Mailbox, mb.Delim)
		if err != nil {
			log.Printf("imap: %s: %v", mb.Mailbox, err)
			failed++
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("imap: %s: creating %s: %v", mb.Mailbox, dir, err)
			failed++
			continue
		}
		if err := mirrorMailbox(client, mb.Mailbox, dir); err != nil {
			log.Printf("imap: %s: %v", mb.Mailbox, err)
			failed++
			continue
		}
		mirrored++
	}

	log.Printf("imap: done: %d folder(s) mirrored, %d failed", mirrored, failed)
	if mirrored == 0 && failed > 0 {
		return fmt.Errorf("imap: all %d folder(s) failed", failed)
	}
	return nil
}

// hasAttr reports whether attrs contains want.
func hasAttr(attrs []imapv2.MailboxAttr, want imapv2.MailboxAttr) bool {
	for _, a := range attrs {
		if a == want {
			return true
		}
	}
	return false
}

// mailboxDir resolves an IMAP mailbox's full name to the local directory it
// mirrors into, splitting on the server's own hierarchy delimiter (0 if the
// server doesn't use one, in which case the whole name is a single
// segment). Rejects a name that would escape destDir once joined onto it —
// defensive only, real IMAP folder names aren't expected to ever trip this
// — the same posture as safeRelPath in mirrortools' other backends.
func mailboxDir(destDir, mailboxName string, delim rune) (string, error) {
	segs := []string{mailboxName}
	if delim != 0 {
		segs = strings.Split(mailboxName, string(delim))
	}
	dir := destDir
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("unsafe folder name %q", mailboxName)
		}
		dir = filepath.Join(dir, s)
	}
	return dir, nil
}

// mirrorMailbox downloads every message of mailbox, from the one after the
// last UID a previous run mirrored (or from the beginning, on a first run or
// after a UIDVALIDITY change) into dir, which is created if needed.
func mirrorMailbox(client *imapclient.Client, mailbox, dir string) error {
	selectData, err := client.Select(mailbox, &imapv2.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return fmt.Errorf("opening mailbox %q: %w", mailbox, err)
	}

	lastUID, err := loadLastUID(dir, selectData.UIDValidity)
	if err != nil {
		return err
	}

	log.Printf("imap: mirroring %s (%d messages, UIDVALIDITY %d) into %s, from UID %d",
		mailbox, selectData.NumMessages, selectData.UIDValidity, dir, lastUID+1)

	var uidSet imapv2.UIDSet
	uidSet.AddRange(imapv2.UID(lastUID+1), 0) // 0 means "*", i.e. no upper bound

	fetchOptions := &imapv2.FetchOptions{
		UID:         true,
		BodySection: []*imapv2.FetchItemBodySection{{Peek: true}},
	}
	cmd := client.Fetch(uidSet, fetchOptions)

	maxUID := lastUID
	downloaded, skipped := 0, 0
	for {
		msg := cmd.Next()
		if msg == nil {
			break
		}
		uid, wasSkipped, err := writeMessage(msg, dir)
		if err != nil {
			cmd.Close()
			return fmt.Errorf("writing message UID %d: %w", uid, err)
		}
		if uint32(uid) > maxUID {
			maxUID = uint32(uid)
		}
		if wasSkipped {
			skipped++
		} else {
			downloaded++
		}
	}
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	if err := writeMarkers(dir, selectData.UIDValidity, maxUID); err != nil {
		return err
	}

	log.Printf("imap: %s: done: %d downloaded, %d already present, highest UID %d", mailbox, downloaded, skipped, maxUID)
	return nil
}

// writeMessage consumes one FETCH response, writing its body section (if
// any) to destDir/<uid>.eml — unless that file already exists, in which case
// its literal is drained (required to keep the connection's wire decoder in
// sync) and discarded. UID is guaranteed to arrive before the body section
// in a message's data items (imapclient always requests it first for a UID
// FETCH), so the destination filename is always known before there's
// anything to write.
func writeMessage(msg *imapclient.FetchMessageData, destDir string) (uid imapv2.UID, skipped bool, err error) {
	for {
		item := msg.Next()
		if item == nil {
			return uid, skipped, nil
		}
		switch it := item.(type) {
		case imapclient.FetchItemDataUID:
			uid = it.UID
		case imapclient.FetchItemDataBodySection:
			if uid == 0 {
				return 0, false, fmt.Errorf("body section arrived before UID")
			}
			wroteAny, werr := streamToFile(it.Literal, filepath.Join(destDir, fmt.Sprintf("%d.eml", uid)))
			if werr != nil {
				return uid, false, werr
			}
			skipped = !wroteAny
		}
	}
}

// streamToFile copies r's content to finalPath via a temp file, renamed into
// place only once the copy finishes — the same atomic write convention used
// elsewhere in mirrortools. If finalPath already exists, r is still drained
// (an IMAP literal must be fully consumed before the next response can be
// read) but not written anywhere. Reports whether it actually wrote the
// file, for the caller's downloaded/skipped counters.
func streamToFile(r imapv2.LiteralReader, finalPath string) (wrote bool, err error) {
	if _, err := os.Stat(finalPath); err == nil {
		if r != nil {
			io.Copy(io.Discard, r)
		}
		return false, nil
	}
	if r == nil {
		return false, fmt.Errorf("server sent no body for %s", filepath.Base(finalPath))
	}

	tmpPath := finalPath + tmpSuffix
	out, err := os.Create(tmpPath)
	if err != nil {
		return false, err
	}
	_, copyErr := io.Copy(out, r)
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(tmpPath)
		return false, copyErr
	}
	if closeErr != nil {
		os.Remove(tmpPath)
		return false, closeErr
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return false, err
	}
	return true, nil
}

// loadLastUID returns the UID to resume mirroring after: the value in
// destDir/.last-uid, unless destDir/.uidvalidity disagrees with the
// mailbox's current UIDVALIDITY (or is absent, i.e. a first run), in which
// case it returns 0 to mirror from the beginning.
func loadLastUID(destDir string, uidValidity uint32) (uint32, error) {
	storedValidity, err := readUintFile(filepath.Join(destDir, uidValidityFile))
	if err != nil {
		return 0, err
	}
	if storedValidity == 0 {
		return 0, nil // first run
	}
	if storedValidity != uidValidity {
		log.Printf("imap: UIDVALIDITY changed (%d -> %d); the server has renumbered this mailbox, doing a full re-sync",
			storedValidity, uidValidity)
		return 0, nil
	}
	return readUintFile(filepath.Join(destDir, lastUIDFile))
}

// writeMarkers records the mailbox's current UIDVALIDITY and the highest UID
// mirrored so far, for loadLastUID to pick up on the next run.
func writeMarkers(destDir string, uidValidity, lastUID uint32) error {
	if err := os.WriteFile(filepath.Join(destDir, uidValidityFile), []byte(strconv.FormatUint(uint64(uidValidity), 10)), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(destDir, lastUIDFile), []byte(strconv.FormatUint(uint64(lastUID), 10)), 0o644)
}

// readUintFile returns the uint32 stored in path, or 0 if path doesn't
// exist.
func readUintFile(path string) (uint32, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", path, err)
	}
	return uint32(v), nil
}
