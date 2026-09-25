package imap

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// ---- readUintFile / writeMarkers / loadLastUID ----

func TestReadUintFile(t *testing.T) {
	dir := t.TempDir()

	if got, err := readUintFile(filepath.Join(dir, "missing")); err != nil || got != 0 {
		t.Errorf("missing file: got %d, %v, want 0, nil", got, err)
	}

	path := filepath.Join(dir, "value")
	if err := os.WriteFile(path, []byte("42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := readUintFile(path); err != nil || got != 42 {
		t.Errorf("got %d, %v, want 42, nil", got, err)
	}

	if err := os.WriteFile(path, []byte("not a number"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readUintFile(path); err == nil {
		t.Error("expected an error for a malformed marker file")
	}
}

func TestLoadLastUID(t *testing.T) {
	t.Run("first run has no markers", func(t *testing.T) {
		dir := t.TempDir()
		got, err := loadLastUID(dir, 100)
		if err != nil || got != 0 {
			t.Fatalf("got %d, %v, want 0, nil", got, err)
		}
	})

	t.Run("matching UIDVALIDITY resumes from the stored last UID", func(t *testing.T) {
		dir := t.TempDir()
		if err := writeMarkers(dir, 100, 55); err != nil {
			t.Fatal(err)
		}
		got, err := loadLastUID(dir, 100)
		if err != nil || got != 55 {
			t.Fatalf("got %d, %v, want 55, nil", got, err)
		}
	})

	t.Run("a changed UIDVALIDITY forces a full re-sync from 0", func(t *testing.T) {
		dir := t.TempDir()
		if err := writeMarkers(dir, 100, 55); err != nil {
			t.Fatal(err)
		}
		got, err := loadLastUID(dir, 200)
		if err != nil || got != 0 {
			t.Fatalf("got %d, %v, want 0, nil", got, err)
		}
	})
}

// ---- Mirror: end-to-end against a real go-imap server ----
//
// There's no ubiquitous local IMAP server binary the way there is for rsync,
// so this spins up github.com/emersion/go-imap/v2's own server
// implementation (imapserver) — the real IMAP wire protocol codec, talking
// real IMAP over a real (self-signed) TLS listener to the real imapclient
// Mirror() uses — backed by a trivial in-memory mailbox rather than a full
// mail store.

func TestMirrorEndToEnd(t *testing.T) {
	store := newFakeStore("me@example.com", "hunter2")
	store.addMailbox("INBOX", 12345)
	store.deliver("INBOX", "first message\r\n")
	store.deliver("INBOX", "second message\r\n")

	addr, cleanup := startFakeIMAPServer(t, store)
	defer cleanup()

	destDir := t.TempDir()
	if err := Mirror(context.Background(), addr, destDir, "me@example.com", "hunter2", "INBOX"); err != nil {
		t.Fatal(err)
	}

	checkFile := func(uid int, want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(destDir, fmt.Sprintf("%d.eml", uid)))
		if err != nil {
			t.Fatalf("uid %d: %v", uid, err)
		}
		if string(got) != want {
			t.Errorf("uid %d: got %q, want %q", uid, got, want)
		}
	}
	checkFile(1, "first message\r\n")
	checkFile(2, "second message\r\n")

	gotValidity, err := readUintFile(filepath.Join(destDir, uidValidityFile))
	if err != nil || gotValidity != 12345 {
		t.Errorf(".uidvalidity: got %d, %v, want 12345", gotValidity, err)
	}
	gotLastUID, err := readUintFile(filepath.Join(destDir, lastUIDFile))
	if err != nil || gotLastUID != 2 {
		t.Errorf(".last-uid: got %d, %v, want 2", gotLastUID, err)
	}

	// Incremental update: a message delivered after the first run must show
	// up on a second run, without re-fetching (or re-requesting) the first
	// two.
	store.deliver("INBOX", "third message\r\n")
	if err := Mirror(context.Background(), addr, destDir, "me@example.com", "hunter2", "INBOX"); err != nil {
		t.Fatal(err)
	}
	checkFile(3, "third message\r\n")

	if got := store.uidsFetched("INBOX"); !equalUints(got, []imapv2.UID{1, 2, 3}) {
		t.Errorf("fetched UIDs across both runs = %v, want [1 2 3]", got)
	}
	if n := store.fetchRequestCount("INBOX"); n != 2 {
		t.Errorf("expected exactly 2 FETCH round-trips (one per Mirror call), got %d", n)
	}

	// A UIDVALIDITY change must force a full re-sync (not delete anything,
	// just redownload from UID 1 again).
	store.setUIDValidity("INBOX", 99999)
	if err := Mirror(context.Background(), addr, destDir, "me@example.com", "hunter2", "INBOX"); err != nil {
		t.Fatal(err)
	}
	checkFile(1, "first message\r\n")
	gotValidity, err = readUintFile(filepath.Join(destDir, uidValidityFile))
	if err != nil || gotValidity != 99999 {
		t.Errorf(".uidvalidity after change: got %d, %v, want 99999", gotValidity, err)
	}
}

func TestMirrorLoginFailure(t *testing.T) {
	store := newFakeStore("me@example.com", "hunter2")
	store.addMailbox("INBOX", 1)
	addr, cleanup := startFakeIMAPServer(t, store)
	defer cleanup()

	destDir := t.TempDir()
	err := Mirror(context.Background(), addr, destDir, "me@example.com", "wrong-password", "INBOX")
	if err == nil {
		t.Fatal("expected an error for a rejected login")
	}
}

func TestMirrorUnknownMailbox(t *testing.T) {
	store := newFakeStore("me@example.com", "hunter2")
	store.addMailbox("INBOX", 1)
	addr, cleanup := startFakeIMAPServer(t, store)
	defer cleanup()

	destDir := t.TempDir()
	err := Mirror(context.Background(), addr, destDir, "me@example.com", "hunter2", "NoSuchMailbox")
	if err == nil {
		t.Fatal("expected an error for a mailbox the server doesn't have")
	}
}

// ---- Mirror with no -mailbox: the whole account, folder tree and all ----

func TestMirrorAllFoldersEndToEnd(t *testing.T) {
	store := newFakeStore("me@example.com", "hunter2")
	store.addMailbox("INBOX", 1)
	store.addMailbox("Archive/2024", 2)
	// "Archive" itself is a pure hierarchy node (real servers do this, e.g.
	// Gmail's "[Gmail]"): listed, but \Noselect, so it must never be opened
	// or get its own marker files — only its selectable child should be.
	store.addNoSelectFolder("Archive")

	store.deliver("INBOX", "inbox message\r\n")
	store.deliver("Archive/2024", "archived message\r\n")

	addr, cleanup := startFakeIMAPServer(t, store)
	defer cleanup()

	destDir := t.TempDir()
	if err := Mirror(context.Background(), addr, destDir, "me@example.com", "hunter2", ""); err != nil {
		t.Fatal(err)
	}

	checkFile := func(rel, want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(destDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%s: got %q, want %q", rel, got, want)
		}
	}
	checkFile("INBOX/1.eml", "inbox message\r\n")
	checkFile("Archive/2024/1.eml", "archived message\r\n")

	if _, err := os.Stat(filepath.Join(destDir, "Archive", uidValidityFile)); !os.IsNotExist(err) {
		t.Errorf("the \\Noselect 'Archive' folder must not have been mirrored itself, stat err = %v", err)
	}

	// Incremental: a new message in one folder must show up on a second
	// run, without disturbing the other folder.
	store.deliver("Archive/2024", "second archived message\r\n")
	if err := Mirror(context.Background(), addr, destDir, "me@example.com", "hunter2", ""); err != nil {
		t.Fatal(err)
	}
	checkFile("Archive/2024/2.eml", "second archived message\r\n")
	if n := store.fetchRequestCount("INBOX"); n != 2 {
		t.Errorf("INBOX should have been visited on both runs, got %d fetch round-trips", n)
	}
}

func TestMirrorAllFoldersContinuesPastOneFolderFailure(t *testing.T) {
	store := newFakeStore("me@example.com", "hunter2")
	store.addMailbox("INBOX", 1)
	store.addMailbox("Broken", 1)
	store.deliver("INBOX", "still works\r\n")
	store.breakMailbox("Broken") // Select on this one always errors

	addr, cleanup := startFakeIMAPServer(t, store)
	defer cleanup()

	destDir := t.TempDir()
	if err := Mirror(context.Background(), addr, destDir, "me@example.com", "hunter2", ""); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destDir, "INBOX", "1.eml"))
	if err != nil || string(got) != "still works\r\n" {
		t.Fatalf("INBOX should still have been mirrored despite Broken failing: got %q, %v", got, err)
	}
}

func equalUints(a, b []imapv2.UID) bool {
	if len(a) != len(b) {
		return false
	}
	sort.Slice(a, func(i, j int) bool { return a[i] < a[j] })
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- fakeStore: the shared, in-memory backing store for every session the
// fake server hands out. Models a whole account: any number of mailboxes,
// plus \Noselect pure hierarchy folders that carry no messages of their
// own. ----

type fakeMessage struct {
	uid  imapv2.UID
	body []byte
}

type fakeMailbox struct {
	uidValidity uint32
	nextUID     imapv2.UID
	messages    []fakeMessage
	fetched     []imapv2.UID
	fetchCalls  int
	broken      bool // Select always fails, simulating a server-side error
}

type fakeStore struct {
	mu              sync.Mutex
	user, pass      string
	delim           rune
	mailboxes       map[string]*fakeMailbox
	noSelectFolders []string
}

func newFakeStore(user, pass string) *fakeStore {
	return &fakeStore{user: user, pass: pass, delim: '/', mailboxes: map[string]*fakeMailbox{}}
}

func (s *fakeStore) addMailbox(name string, uidValidity uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mailboxes[name] = &fakeMailbox{uidValidity: uidValidity, nextUID: 1}
}

func (s *fakeStore) addNoSelectFolder(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noSelectFolders = append(s.noSelectFolders, name)
}

func (s *fakeStore) breakMailbox(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mailboxes[name].broken = true
}

func (s *fakeStore) deliver(mailbox, body string) imapv2.UID {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := s.mailboxes[mailbox]
	uid := mb.nextUID
	mb.nextUID++
	mb.messages = append(mb.messages, fakeMessage{uid: uid, body: []byte(body)})
	return uid
}

func (s *fakeStore) setUIDValidity(mailbox string, v uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mailboxes[mailbox].uidValidity = v
}

func (s *fakeStore) uidsFetched(mailbox string) []imapv2.UID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]imapv2.UID(nil), s.mailboxes[mailbox].fetched...)
}

func (s *fakeStore) fetchRequestCount(mailbox string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mailboxes[mailbox].fetchCalls
}

// ---- fakeSession: one client connection's view onto a fakeStore.
// Implements imapserver.Session; only Login/List/Select/Fetch do anything
// real, everything else the test client never calls. ----

type fakeSession struct {
	store    *fakeStore
	selected string // set by Select; Fetch operates on this mailbox
}

func (s *fakeSession) Close() error { return nil }

func (s *fakeSession) Login(username, password string) error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if username != s.store.user || password != s.store.pass {
		return imapserver.ErrAuthFailed
	}
	return nil
}

func (s *fakeSession) List(w *imapserver.ListWriter, ref string, patterns []string, options *imapv2.ListOptions) error {
	s.store.mu.Lock()
	names := make([]string, 0, len(s.store.mailboxes))
	for name := range s.store.mailboxes {
		names = append(names, name)
	}
	noSelect := append([]string(nil), s.store.noSelectFolders...)
	delim := s.store.delim
	s.store.mu.Unlock()
	sort.Strings(names)

	for _, name := range noSelect {
		if err := w.WriteList(&imapv2.ListData{Mailbox: name, Delim: delim, Attrs: []imapv2.MailboxAttr{imapv2.MailboxAttrNoSelect}}); err != nil {
			return err
		}
	}
	for _, name := range names {
		if err := w.WriteList(&imapv2.ListData{Mailbox: name, Delim: delim}); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeSession) Select(mailbox string, options *imapv2.SelectOptions) (*imapv2.SelectData, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	mb, ok := s.store.mailboxes[mailbox]
	if !ok {
		return nil, fmt.Errorf("no such mailbox %q", mailbox)
	}
	if mb.broken {
		return nil, fmt.Errorf("simulated server error selecting %q", mailbox)
	}
	s.selected = mailbox
	return &imapv2.SelectData{
		NumMessages: uint32(len(mb.messages)),
		UIDValidity: mb.uidValidity,
		UIDNext:     mb.nextUID,
	}, nil
}

func (s *fakeSession) Fetch(w *imapserver.FetchWriter, numSet imapv2.NumSet, options *imapv2.FetchOptions) error {
	uidSet, ok := numSet.(imapv2.UIDSet)
	if !ok {
		return fmt.Errorf("fakeSession only supports UID FETCH")
	}

	s.store.mu.Lock()
	mb := s.store.mailboxes[s.selected]
	msgs := append([]fakeMessage(nil), mb.messages...)
	mb.fetchCalls++
	s.store.mu.Unlock()

	for i, m := range msgs {
		if !uidSet.Contains(m.uid) {
			continue
		}
		s.store.mu.Lock()
		mb.fetched = append(mb.fetched, m.uid)
		s.store.mu.Unlock()

		rw := w.CreateMessage(uint32(i + 1))
		if options.UID {
			rw.WriteUID(m.uid)
		}
		for _, sec := range options.BodySection {
			wc := rw.WriteBodySection(sec, int64(len(m.body)))
			if _, err := wc.Write(m.body); err != nil {
				wc.Close()
				rw.Close()
				return err
			}
			wc.Close()
		}
		if err := rw.Close(); err != nil {
			return err
		}
	}
	return nil
}

var errNotImplemented = errors.New("not implemented in fakeSession")

func (s *fakeSession) Create(string, *imapv2.CreateOptions) error { return errNotImplemented }
func (s *fakeSession) Delete(string) error                        { return errNotImplemented }
func (s *fakeSession) Rename(string, string, *imapv2.RenameOptions) error {
	return errNotImplemented
}
func (s *fakeSession) Subscribe(string) error   { return errNotImplemented }
func (s *fakeSession) Unsubscribe(string) error { return errNotImplemented }
func (s *fakeSession) Status(string, *imapv2.StatusOptions) (*imapv2.StatusData, error) {
	return nil, errNotImplemented
}
func (s *fakeSession) Append(string, imapv2.LiteralReader, *imapv2.AppendOptions) (*imapv2.AppendData, error) {
	return nil, errNotImplemented
}
func (s *fakeSession) Poll(*imapserver.UpdateWriter, bool) error { return nil }
func (s *fakeSession) Idle(*imapserver.UpdateWriter, <-chan struct{}) error {
	return errNotImplemented
}
func (s *fakeSession) Unselect() error { return nil }
func (s *fakeSession) Expunge(*imapserver.ExpungeWriter, *imapv2.UIDSet) error {
	return errNotImplemented
}
func (s *fakeSession) Search(imapserver.NumKind, *imapv2.SearchCriteria, *imapv2.SearchOptions) (*imapv2.SearchData, error) {
	return nil, errNotImplemented
}
func (s *fakeSession) Store(*imapserver.FetchWriter, imapv2.NumSet, *imapv2.StoreFlags, *imapv2.StoreOptions) error {
	return errNotImplemented
}
func (s *fakeSession) Copy(imapv2.NumSet, string) (*imapv2.CopyData, error) {
	return nil, errNotImplemented
}

// ---- test server plumbing: a real imapserver.Server over a real
// (self-signed) TLS listener on an ephemeral port. ----

func startFakeIMAPServer(t *testing.T, store *fakeStore) (addr string, cleanup func()) {
	t.Helper()

	cert, pool := generateSelfSignedCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &fakeSession{store: store}, nil, nil
		},
		Caps: imapv2.CapSet{imapv2.CapIMAP4rev1: {}},
	})

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(tlsLn) }()

	// Mirror verifies the server's certificate for real (see imap.go's
	// tlsConfig var) — trust our freshly generated test cert instead of
	// weakening that check.
	origTLSConfig := tlsConfig
	tlsConfig = func(host string) *tls.Config {
		return &tls.Config{ServerName: host, RootCAs: pool}
	}

	cleanup = func() {
		tlsConfig = origTLSConfig
		srv.Close()
		select {
		case <-serveErr:
		case <-time.After(2 * time.Second):
		}
	}
	return ln.Addr().String(), cleanup
}

func generateSelfSignedCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}
