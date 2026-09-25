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
// Each folder is stored as a Maildir: <folder-dir> holds the standard tmp/,
// new/ and cur/ subdirectories, and every message — written to tmp/ first,
// then renamed into place only once fully received, the same atomic-write
// discipline Maildir itself is designed around — lands in cur/, never new/
// (new/ exists only because a directory needs it to be a valid Maildir;
// "new" describes mail an MDA has just delivered and no MUA has looked at
// yet, which isn't a meaningful state for a passive mirror). Its raw RFC 822
// form is stored unmodified — nothing is parsed or reformatted — so it's
// exactly what a real mail client would show.
//
// A message's filename is <uid>.mirrortools:2,<flags>, where <flags> is its
// IMAP flags translated to their Maildir letters (Seen->S, Answered->R,
// Flagged->F, Deleted->T, Draft->D, the $Forwarded keyword->P), sorted as
// Maildir requires. Embedding the IMAP UID in the filename is not part of
// the Maildir spec, but is a long-standing convention (used by tools like
// isync/mbsync) for exactly this purpose: recognizing a message that's
// already been mirrored without needing an index file. A mirrored message's
// flags are fixed at whatever they were the moment it was fetched — mirrored
// once, never revisited — so a flag changing later on the server (e.g. read
// after the mirror ran) isn't reflected retroactively; that would need
// re-fetching every message on every run, defeating the incremental UID
// range described below.
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
// a message already in cur/ (found by its UID, regardless of its current
// flags suffix) is still skipped rather than re-fetched.
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
		Describe: "mirror an IMAP account into destDir as a Maildir per folder",
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

	if err := ensureMaildir(dir); err != nil {
		return fmt.Errorf("creating maildir: %w", err)
	}

	log.Printf("imap: mirroring %s (%d messages, UIDVALIDITY %d) into %s, from UID %d",
		mailbox, selectData.NumMessages, selectData.UIDValidity, dir, lastUID+1)

	var uidSet imapv2.UIDSet
	uidSet.AddRange(imapv2.UID(lastUID+1), 0) // 0 means "*", i.e. no upper bound

	fetchOptions := &imapv2.FetchOptions{
		UID:         true,
		Flags:       true,
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

// ensureMaildir creates dir's tmp/, new/ and cur/ subdirectories, so dir is
// a valid (if perpetually new/-empty — see the package doc comment) Maildir.
func ensureMaildir(dir string) error {
	for _, sub := range [...]string{"tmp", "new", "cur"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// maildirBase is the UID-derived part of a mirrored message's filename,
// common to both its temporary (tmp/) and final (cur/) names. Not itself a
// complete Maildir filename — see the package doc comment for the
// ":2,<flags>" suffix a file in cur/ additionally carries.
func maildirBase(uid imapv2.UID) string {
	return fmt.Sprintf("%d.mirrortools", uid)
}

// maildirHasUID reports whether uid has already been mirrored into dir's
// cur/, regardless of its current flags suffix (see the package doc comment
// on why a flag change alone never triggers a re-fetch).
func maildirHasUID(dir string, uid imapv2.UID) bool {
	matches, _ := filepath.Glob(filepath.Join(dir, "cur", maildirBase(uid)+":2,*"))
	return len(matches) > 0
}

// maildirFlagChars, in the alphabetical order Maildir filenames require,
// maps each IMAP flag this cares about to its Maildir letter. Uses the
// widely-implemented (if not core-spec) convention of "P" for the
// $Forwarded keyword, matching e.g. Dovecot and isync/mbsync.
var maildirFlagChars = []struct {
	flag imapv2.Flag
	char byte
}{
	{imapv2.FlagDraft, 'D'},
	{imapv2.FlagFlagged, 'F'},
	{imapv2.FlagForwarded, 'P'},
	{imapv2.FlagAnswered, 'R'},
	{imapv2.FlagSeen, 'S'},
	{imapv2.FlagDeleted, 'T'},
}

func maildirFlagString(flags []imapv2.Flag) string {
	set := make(map[imapv2.Flag]bool, len(flags))
	for _, f := range flags {
		set[f] = true
	}
	buf := make([]byte, 0, len(maildirFlagChars))
	for _, m := range maildirFlagChars {
		if set[m.flag] {
			buf = append(buf, m.char)
		}
	}
	return string(buf)
}

// writeMessage consumes one FETCH response, writing its body section (if
// any) into dir's Maildir — unless UID is already present in cur/, in which
// case its literal is drained (required to keep the connection's wire
// decoder in sync) and discarded rather than written anywhere.
//
// The body is streamed to tmp/ as soon as it arrives, since its
// LiteralReader must be consumed before the connection can move on to
// whatever the server sends next — which, since the IMAP protocol doesn't
// guarantee FETCH data items arrive in the order they were requested, might
// be the message's flags, still to come. Only once every item for this
// message has been seen (so the final flags, if any, are known for certain)
// is the tmp/ file given its real name and moved into cur/.
func writeMessage(msg *imapclient.FetchMessageData, dir string) (uid imapv2.UID, skipped bool, err error) {
	var (
		flags   []imapv2.Flag
		tmpPath string
		gotBody bool
		haveIt  bool
	)
	for {
		item := msg.Next()
		if item == nil {
			break
		}
		switch it := item.(type) {
		case imapclient.FetchItemDataUID:
			uid = it.UID
			haveIt = maildirHasUID(dir, uid)
		case imapclient.FetchItemDataFlags:
			flags = it.Flags
		case imapclient.FetchItemDataBodySection:
			if uid == 0 {
				return 0, false, fmt.Errorf("body section arrived before UID")
			}
			gotBody = true
			if haveIt {
				if it.Literal != nil {
					io.Copy(io.Discard, it.Literal)
				}
				continue
			}
			if it.Literal == nil {
				return uid, false, fmt.Errorf("server sent no body")
			}
			tmpPath = filepath.Join(dir, "tmp", maildirBase(uid))
			if err := writeLiteralToFile(it.Literal, tmpPath); err != nil {
				return uid, false, err
			}
		}
	}

	if haveIt {
		return uid, true, nil
	}
	if !gotBody {
		return uid, false, fmt.Errorf("server never sent a body section")
	}
	finalPath := filepath.Join(dir, "cur", maildirBase(uid)+":2,"+maildirFlagString(flags))
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return uid, false, err
	}
	return uid, false, nil
}

// writeLiteralToFile copies r's full content to path, which must not
// already exist under a different name being relied on for atomicity —
// writeMessage's caller uses this for a fresh file under tmp/, then renames
// it into cur/ once it knows the final name. A copy that fails partway
// leaves nothing at path.
func writeLiteralToFile(r imapv2.LiteralReader, path string) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, r)
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(path)
		return copyErr
	}
	if closeErr != nil {
		os.Remove(path)
		return closeErr
	}
	return nil
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
