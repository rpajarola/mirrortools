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
	store := newFakeStore("me@example.com", "hunter2", 12345)
	store.deliver("first message\r\n")
	store.deliver("second message\r\n")

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
	store.deliver("third message\r\n")
	if err := Mirror(context.Background(), addr, destDir, "me@example.com", "hunter2", "INBOX"); err != nil {
		t.Fatal(err)
	}
	checkFile(3, "third message\r\n")

	if got := store.uidsFetched(); !equalUints(got, []imapv2.UID{1, 2, 3}) {
		t.Errorf("fetched UIDs across both runs = %v, want [1 2 3]", got)
	}
	if n := store.fetchRequestCount(); n != 2 {
		t.Errorf("expected exactly 2 FETCH round-trips (one per Mirror call), got %d", n)
	}

	// A UIDVALIDITY change must force a full re-sync (not delete anything,
	// just redownload from UID 1 again).
	store.setUIDValidity(99999)
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
	store := newFakeStore("me@example.com", "hunter2", 1)
	addr, cleanup := startFakeIMAPServer(t, store)
	defer cleanup()

	destDir := t.TempDir()
	err := Mirror(context.Background(), addr, destDir, "me@example.com", "wrong-password", "INBOX")
	if err == nil {
		t.Fatal("expected an error for a rejected login")
	}
}

func TestMirrorUnknownMailbox(t *testing.T) {
	store := newFakeStore("me@example.com", "hunter2", 1)
	addr, cleanup := startFakeIMAPServer(t, store)
	defer cleanup()

	destDir := t.TempDir()
	err := Mirror(context.Background(), addr, destDir, "me@example.com", "hunter2", "NoSuchMailbox")
	if err == nil {
		t.Fatal("expected an error for a mailbox the server doesn't have")
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
// fake server hands out. One store == one mailbox ("INBOX"). ----

type fakeMessage struct {
	uid  imapv2.UID
	body []byte
}

type fakeStore struct {
	mu          sync.Mutex
	user, pass  string
	uidValidity uint32
	nextUID     imapv2.UID
	messages    []fakeMessage
	fetched     []imapv2.UID
	fetchCalls  int
}

func newFakeStore(user, pass string, uidValidity uint32) *fakeStore {
	return &fakeStore{user: user, pass: pass, uidValidity: uidValidity, nextUID: 1}
}

func (s *fakeStore) deliver(body string) imapv2.UID {
	s.mu.Lock()
	defer s.mu.Unlock()
	uid := s.nextUID
	s.nextUID++
	s.messages = append(s.messages, fakeMessage{uid: uid, body: []byte(body)})
	return uid
}

func (s *fakeStore) setUIDValidity(v uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uidValidity = v
}

func (s *fakeStore) uidsFetched() []imapv2.UID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]imapv2.UID(nil), s.fetched...)
}

func (s *fakeStore) fetchRequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetchCalls
}

// ---- fakeSession: one client connection's view onto a fakeStore.
// Implements imapserver.Session; only Login/Select/Fetch do anything real,
// everything else the test client never calls. ----

type fakeSession struct {
	store *fakeStore
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

func (s *fakeSession) Select(mailbox string, options *imapv2.SelectOptions) (*imapv2.SelectData, error) {
	if mailbox != "INBOX" {
		return nil, fmt.Errorf("no such mailbox %q", mailbox)
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	return &imapv2.SelectData{
		NumMessages: uint32(len(s.store.messages)),
		UIDValidity: s.store.uidValidity,
		UIDNext:     s.store.nextUID,
	}, nil
}

func (s *fakeSession) Fetch(w *imapserver.FetchWriter, numSet imapv2.NumSet, options *imapv2.FetchOptions) error {
	uidSet, ok := numSet.(imapv2.UIDSet)
	if !ok {
		return fmt.Errorf("fakeSession only supports UID FETCH")
	}

	s.store.mu.Lock()
	msgs := append([]fakeMessage(nil), s.store.messages...)
	s.store.fetchCalls++
	s.store.mu.Unlock()

	for i, m := range msgs {
		if !uidSet.Contains(m.uid) {
			continue
		}
		s.store.mu.Lock()
		s.store.fetched = append(s.store.fetched, m.uid)
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
func (s *fakeSession) List(*imapserver.ListWriter, string, []string, *imapv2.ListOptions) error {
	return errNotImplemented
}
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
