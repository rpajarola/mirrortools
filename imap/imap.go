// Package imap mirrors an IMAP mailbox (an inbox, by default) to a local
// directory using github.com/emersion/go-imap/v2, a pure-Go IMAP client —
// no external `imapsync`/`fetchmail`-style tool required.
//
// It registers itself as the "imap" method with mirrortools' mirror
// package, so it's normally driven through the mirror CLI:
//
//	mirror imap -user me@example.com -password-file pw.txt imap.example.com ./mail
//
// Each message is saved as destDir/<uid>.eml, its raw RFC 822 form — nothing
// is parsed or reformatted, so the file is exactly what a real mail client
// would show, and can be fed to any tool that reads .eml files.
//
// The mailbox is opened read-only (EXAMINE, not SELECT) and every fetch uses
// IMAP's PEEK option, so mirroring a mailbox never marks its messages as
// read or otherwise changes anything on the server — a mirror is a pure
// read operation.
//
// Incremental updates are UID-based, not a full re-listing: destDir/.last-uid
// records the highest UID mirrored so far, and each run asks the server only
// for UIDs above that (a single UID FETCH <last+1>:* — cheap even against a
// huge mailbox, since it costs nothing proportional to how many messages
// were already mirrored). destDir/.uidvalidity records the mailbox's
// UIDVALIDITY; if the server reports a different one on a later run — it's
// entitled to, e.g. after certain kinds of mailbox reorganization — UIDs
// from before aren't guaranteed to mean the same messages any more, so
// mirroring starts over from UID 1. This never deletes anything already on
// disk, so a UIDVALIDITY change costs a redundant re-download rather than
// losing data; already-present files are still skipped via the usual
// exists-on-disk check.
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
		Describe: "mirror an IMAP mailbox (INBOX by default) into destDir as one .eml file per message",
		SetupFlags: func(fs *flag.FlagSet) mirror.Func {
			user := fs.String("user", "", "IMAP account username (required)")
			passwordFile := fs.String("password-file", "", "path to a file containing the account's password, "+
				"one line — kept out of the command line and shell history (required)")
			mailbox := fs.String("mailbox", "INBOX", "mailbox to mirror")
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

// Mirror downloads every message of mailbox on the IMAP server named by
// source, from the one after the last UID a previous run mirrored (or from
// the beginning, on a first run or after a UIDVALIDITY change) into destDir,
// which the caller guarantees exists. See the package doc comment for the
// on-disk layout and the incremental-update strategy.
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

	selectData, err := client.Select(mailbox, &imapv2.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return fmt.Errorf("imap: opening mailbox %q: %w", mailbox, err)
	}

	lastUID, err := loadLastUID(destDir, selectData.UIDValidity)
	if err != nil {
		return fmt.Errorf("imap: %w", err)
	}

	log.Printf("imap: mirroring %s (%d messages, UIDVALIDITY %d) into %s, from UID %d",
		mailbox, selectData.NumMessages, selectData.UIDValidity, destDir, lastUID+1)

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
		uid, wasSkipped, err := writeMessage(msg, destDir)
		if err != nil {
			cmd.Close()
			return fmt.Errorf("imap: writing message UID %d: %w", uid, err)
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
		return fmt.Errorf("imap: fetch: %w", err)
	}

	if err := writeMarkers(destDir, selectData.UIDValidity, maxUID); err != nil {
		return fmt.Errorf("imap: %w", err)
	}

	log.Printf("imap: done: %d downloaded, %d already present, highest UID %d", downloaded, skipped, maxUID)
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
