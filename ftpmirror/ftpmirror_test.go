package ftpmirror

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func init() { initialBackoff = time.Millisecond }

// ---- fake FTP server ----

type fakeEntry struct {
	dir     bool
	content string
	mtime   time.Time
	link    string // symlink target (LIST mode only)
}

// fakeServer is a minimal FTP server covering what jlaffaye/ftp and
// Mirror use: USER/PASS, FEAT, TYPE, OPTS, EPSV, CWD, PWD, MLSD/LIST, REST,
// RETR, SIZE, MDTM, QUIT.
type fakeServer struct {
	t        *testing.T
	ln       net.Listener
	noMLST   bool   // only offer LIST
	password string // required password, "" for any

	mu       sync.Mutex
	files    map[string]*fakeEntry // absolute path -> entry
	conns    int
	retrs    []string       // "path@offset"
	failRetr map[string]int // path -> reply code to fail RETR with
}

func newFakeServer(t *testing.T) *fakeServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{t: t, ln: ln, files: map[string]*fakeEntry{"/": {dir: true}}, failRetr: map[string]int{}}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns++
			s.mu.Unlock()
			go s.serve(c)
		}
	}()
	return s
}

func (s *fakeServer) url(p string) string { return "ftp://" + s.ln.Addr().String() + p }

var testTime = time.Date(2020, 5, 17, 12, 0, 0, 0, time.UTC)

func (s *fakeServer) file(p, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mkdirs(path.Dir(p))
	s.files[p] = &fakeEntry{content: content, mtime: testTime}
}

func (s *fakeServer) symlink(p, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mkdirs(path.Dir(p))
	s.files[p] = &fakeEntry{link: target, mtime: testTime}
}

func (s *fakeServer) mkdirs(p string) {
	for ; p != "/"; p = path.Dir(p) {
		if s.files[p] == nil {
			s.files[p] = &fakeEntry{dir: true, mtime: testTime}
		}
	}
}

func (s *fakeServer) connCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

func (s *fakeServer) retrCount(p string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.retrs {
		if strings.HasPrefix(r, p+"@") {
			n++
		}
	}
	return n
}

func (s *fakeServer) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	reply := func(format string, args ...any) { fmt.Fprintf(c, format+"\r\n", args...) }
	reply("220 fake ftpd")
	cwd := "/"
	var data net.Listener
	var offset int64
	resolve := func(arg string) string {
		if arg == "" {
			return cwd
		}
		if !strings.HasPrefix(arg, "/") {
			arg = path.Join(cwd, arg)
		}
		return path.Clean(arg)
	}
	openData := func() net.Conn {
		if data == nil {
			return nil
		}
		data.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
		dc, err := data.Accept()
		data.Close()
		data = nil
		if err != nil {
			return nil
		}
		return dc
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(cmd) {
		case "USER":
			reply("331 password please")
		case "PASS":
			if s.password != "" && arg != s.password {
				reply("530 login incorrect")
			} else {
				reply("230 logged in")
			}
		case "FEAT":
			if s.noMLST {
				reply("211-Features:\r\n MDTM\r\n SIZE\r\n REST STREAM\r\n211 End")
			} else {
				reply("211-Features:\r\n MLST type*;size*;modify*;\r\n MDTM\r\n SIZE\r\n REST STREAM\r\n211 End")
			}
		case "TYPE", "OPTS":
			reply("200 ok")
		case "EPSV":
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				reply("425 no")
				continue
			}
			data = ln
			reply("229 Entering Extended Passive Mode (|||%d|)", ln.Addr().(*net.TCPAddr).Port)
		case "CWD":
			p := resolve(arg)
			s.mu.Lock()
			e := s.files[p]
			s.mu.Unlock()
			if e == nil || !e.dir {
				reply("550 no such directory")
			} else {
				cwd = p
				reply("250 ok")
			}
		case "PWD":
			reply(`257 "%s"`, cwd)
		case "REST":
			offset, _ = strconv.ParseInt(arg, 10, 64)
			reply("350 restarting")
		case "MLSD", "LIST":
			dir := resolve(arg)
			var lines []string
			s.mu.Lock()
			for p, e := range s.files {
				if p == "/" || path.Dir(p) != dir {
					continue
				}
				name := path.Base(p)
				if cmd == "MLSD" {
					typ := "file"
					if e.dir {
						typ = "dir"
					}
					lines = append(lines, fmt.Sprintf("type=%s;size=%d;modify=%s; %s", typ, len(e.content), e.mtime.Format("20060102150405"), name))
				} else {
					date := e.mtime.Format("Jan _2  2006")
					switch {
					case e.dir:
						lines = append(lines, fmt.Sprintf("drwxr-xr-x 2 ftp ftp 4096 %s %s", date, name))
					case e.link != "":
						lines = append(lines, fmt.Sprintf("lrwxrwxrwx 1 ftp ftp %d %s %s -> %s", len(e.link), date, name, e.link))
					default:
						lines = append(lines, fmt.Sprintf("-rw-r--r-- 1 ftp ftp %d %s %s", len(e.content), date, name))
					}
				}
			}
			s.mu.Unlock()
			if cmd == "MLSD" {
				lines = append(lines, "type=cdir;modify=20200101000000; "+dir)
			}
			sort.Strings(lines)
			reply("150 here it comes")
			dc := openData()
			if dc == nil {
				reply("425 no data connection")
				continue
			}
			for _, l := range lines {
				fmt.Fprintf(dc, "%s\r\n", l)
			}
			dc.Close()
			reply("226 done")
		case "RETR":
			p := resolve(arg)
			s.mu.Lock()
			e := s.files[p]
			fail := s.failRetr[p]
			s.retrs = append(s.retrs, fmt.Sprintf("%s@%d", p, offset))
			s.mu.Unlock()
			off := offset
			offset = 0
			if fail != 0 {
				if data != nil {
					data.Close()
					data = nil
				}
				reply("%d failing on purpose", fail)
				continue
			}
			if e == nil || e.dir || e.link != "" {
				reply("550 no such file")
				continue
			}
			reply("150 here it comes")
			dc := openData()
			if dc == nil {
				reply("425 no data connection")
				continue
			}
			if off < int64(len(e.content)) {
				dc.Write([]byte(e.content[off:]))
			}
			dc.Close()
			reply("226 done")
		case "SIZE", "MDTM":
			s.mu.Lock()
			e := s.files[resolve(arg)]
			s.mu.Unlock()
			if e == nil || e.dir {
				reply("550 no such file")
			} else if cmd == "SIZE" {
				reply("213 %d", len(e.content))
			} else {
				reply("213 %s", e.mtime.Format("20060102150405"))
			}
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 not implemented")
		}
	}
}

// ---- helpers ----

func readLocal(t *testing.T, destDir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(destDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Errorf("%s: %v", rel, err)
		return ""
	}
	return string(b)
}

func exists(destDir, rel string) bool {
	_, err := os.Lstat(filepath.Join(destDir, filepath.FromSlash(rel)))
	return err == nil
}

// ---- tests ----

func TestMirrorEndToEnd(t *testing.T) {
	s := newFakeServer(t)
	s.file("/pub/a.txt", "hello")
	s.file("/pub/.hidden", "h")
	s.file("/pub/sub dir/b.bin", "\x00\x01\x02")
	s.file("/pub/sub dir/deeper/c.txt", "c")
	s.file("/other/x.txt", "out of scope")
	s.mkdirs("/pub/empty")

	destDir := t.TempDir()
	if err := Mirror(context.Background(), s.url("/pub"), destDir, Options{}); err != nil {
		t.Fatal(err)
	}
	host := s.ln.Addr().String()
	for rel, want := range map[string]string{
		"pub/a.txt":                "hello",
		"pub/.hidden":              "h",
		"pub/sub dir/b.bin":        "\x00\x01\x02",
		"pub/sub dir/deeper/c.txt": "c",
	} {
		if got := readLocal(t, destDir, host+"/"+rel); got != want {
			t.Errorf("%s: got %q, want %q", rel, got, want)
		}
	}
	if fi, err := os.Stat(filepath.Join(destDir, host, "pub", "empty")); err != nil || !fi.IsDir() {
		t.Errorf("empty directory should be created: %v", err)
	}
	if exists(destDir, host+"/other") {
		t.Error("/other is out of scope")
	}
	if fi, err := os.Stat(filepath.Join(destDir, host, "pub", "a.txt")); err != nil || !fi.ModTime().Equal(testTime) {
		t.Errorf("mtime should come from the listing: %v, %v", fi.ModTime(), err)
	}
	if matches, _ := filepath.Glob(filepath.Join(destDir, host, "pub", "*"+tmpSuffix)); len(matches) != 0 {
		t.Errorf("leftover temp files: %v", matches)
	}

	// A re-run with nothing left to do doesn't even connect.
	before := s.connCount()
	if err := Mirror(context.Background(), s.url("/pub"), destDir, Options{}); err != nil {
		t.Fatal(err)
	}
	if n := s.connCount() - before; n != 0 {
		t.Errorf("re-run connected %d times, want 0", n)
	}

	// -update picks up new and changed files, leaves unchanged ones.
	s.file("/pub/a.txt", "hello, changed")
	s.file("/pub/new.txt", "new")
	if err := Mirror(context.Background(), s.url("/pub"), destDir, Options{Update: true}); err != nil {
		t.Fatal(err)
	}
	if got := readLocal(t, destDir, host+"/pub/a.txt"); got != "hello, changed" {
		t.Errorf("a.txt: got %q", got)
	}
	if got := readLocal(t, destDir, host+"/pub/new.txt"); got != "new" {
		t.Errorf("new.txt: got %q", got)
	}
	if n := s.retrCount("/pub/sub dir/b.bin"); n != 1 {
		t.Errorf("unchanged b.bin downloaded %d times, want 1", n)
	}
}

func TestMirrorLISTModeAndSymlinks(t *testing.T) {
	s := newFakeServer(t)
	s.noMLST = true
	s.file("/pub/a.txt", "hello")
	s.file("/pub/sub/b.txt", "b")
	s.symlink("/pub/rel", "a.txt")
	s.symlink("/pub/sub/abs", "/pub/a.txt")

	destDir := t.TempDir()
	if err := Mirror(context.Background(), s.url("/pub/"), destDir, Options{}); err != nil {
		t.Fatal(err)
	}
	host := s.ln.Addr().String()
	if got := readLocal(t, destDir, host+"/pub/sub/b.txt"); got != "b" {
		t.Errorf("b.txt: got %q", got)
	}
	for rel, want := range map[string]string{"pub/rel": "a.txt", "pub/sub/abs": filepath.Join("..", "a.txt")} {
		got, err := os.Readlink(filepath.Join(destDir, host, filepath.FromSlash(rel)))
		if err != nil || got != want {
			t.Errorf("%s: symlink -> %q, %v; want -> %q", rel, got, err, want)
		}
	}
	if got := readLocal(t, destDir, host+"/pub/sub/abs"); got != "hello" {
		t.Errorf("absolute symlink should resolve inside the mirror, got %q", got)
	}
	// LIST timestamps are imprecise, so the mtime comes from MDTM.
	if fi, err := os.Stat(filepath.Join(destDir, host, "pub", "a.txt")); err != nil || !fi.ModTime().Equal(testTime) {
		t.Errorf("mtime: %v, %v", fi.ModTime(), err)
	}
}

func TestMirrorResumesPartialDownload(t *testing.T) {
	s := newFakeServer(t)
	s.file("/pub/big.bin", "0123456789")

	destDir := t.TempDir()
	host := s.ln.Addr().String()
	dir := filepath.Join(destDir, host, "pub")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// What an earlier, interrupted attempt leaves behind.
	if err := os.WriteFile(filepath.Join(dir, "big.bin"+tmpSuffix), []byte("0123"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Mirror(context.Background(), s.url("/pub"), destDir, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := readLocal(t, destDir, host+"/pub/big.bin"); got != "0123456789" {
		t.Errorf("got %q", got)
	}
	s.mu.Lock()
	retrs := append([]string(nil), s.retrs...)
	s.mu.Unlock()
	if len(retrs) != 1 || retrs[0] != "/pub/big.bin@4" {
		t.Errorf("RETRs = %v, want one resuming at offset 4", retrs)
	}
}

func TestMirrorErrorsAndRetries(t *testing.T) {
	s := newFakeServer(t)
	s.file("/pub/flaky.txt", "flaky")
	s.file("/pub/denied.txt", "denied")
	s.file("/pub/ok.txt", "ok")
	s.mu.Lock()
	s.failRetr["/pub/flaky.txt"] = 451
	s.failRetr["/pub/denied.txt"] = 550
	s.mu.Unlock()

	destDir := t.TempDir()
	if err := Mirror(context.Background(), s.url("/pub"), destDir, Options{}); err == nil {
		t.Fatal("expected an error while flaky.txt keeps failing")
	}
	if n := s.retrCount("/pub/flaky.txt"); n != maxAttempts {
		t.Errorf("flaky.txt tried %d times, want %d", n, maxAttempts)
	}
	if n := s.retrCount("/pub/denied.txt"); n != 1 {
		t.Errorf("a 550 is permanent and shouldn't be retried, got %d attempts", n)
	}
	host := s.ln.Addr().String()
	if got := readLocal(t, destDir, host+"/pub/ok.txt"); got != "ok" {
		t.Errorf("ok.txt: got %q", got)
	}

	s.mu.Lock()
	delete(s.failRetr, "/pub/flaky.txt")
	delete(s.failRetr, "/pub/denied.txt")
	s.mu.Unlock()
	if err := Mirror(context.Background(), s.url("/pub"), destDir, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := readLocal(t, destDir, host+"/pub/flaky.txt"); got != "flaky" {
		t.Errorf("flaky.txt: got %q", got)
	}
	if n := s.retrCount("/pub/ok.txt"); n != 1 {
		t.Errorf("ok.txt re-downloaded on resume: %d", n)
	}
	if exists(destDir, host+"/pub/denied.txt") {
		t.Error("a permanently failed file is only retried with -update")
	}
	if err := Mirror(context.Background(), s.url("/pub"), destDir, Options{Update: true}); err != nil {
		t.Fatal(err)
	}
	if got := readLocal(t, destDir, host+"/pub/denied.txt"); got != "denied" {
		t.Errorf("denied.txt after -update: got %q", got)
	}
}

func TestMirrorSingleFile(t *testing.T) {
	s := newFakeServer(t)
	s.file("/pub/a.txt", "hello")
	s.file("/pub/b.txt", "not wanted")

	destDir := t.TempDir()
	if err := Mirror(context.Background(), s.url("/pub/a.txt"), destDir, Options{}); err != nil {
		t.Fatal(err)
	}
	host := s.ln.Addr().String()
	if got := readLocal(t, destDir, host+"/pub/a.txt"); got != "hello" {
		t.Errorf("got %q", got)
	}
	if exists(destDir, host+"/pub/b.txt") {
		t.Error("only the named file should be mirrored")
	}
}

func TestMirrorLoginFailure(t *testing.T) {
	s := newFakeServer(t)
	s.password = "secret"
	s.file("/pub/a.txt", "hello")

	err := Mirror(context.Background(), s.url("/pub"), t.TempDir(), Options{User: "me", Password: "wrong"})
	if err == nil || !strings.Contains(err.Error(), "logging in") {
		t.Fatalf("err = %v, want a login error", err)
	}
	if err := Mirror(context.Background(), s.url("/pub"), t.TempDir(), Options{User: "me", Password: "secret"}); err != nil {
		t.Fatal(err)
	}
}

func TestMirrorRejectsBadURLs(t *testing.T) {
	for _, src := range []string{"http://example.com/", "ftp:///pub", "ftp://../x"} {
		if err := Mirror(context.Background(), src, t.TempDir(), Options{}); err == nil {
			t.Errorf("%s: expected an error", src)
		}
	}
}

func TestInScopeAndLocalPath(t *testing.T) {
	c := &crawler{root: "/pub", hostDir: "/dest/host"}
	for p, want := range map[string]bool{"/pub": true, "/pub/a": true, "/public": false, "/": false, "/other/pub": false} {
		if got := c.inScope(p); got != want {
			t.Errorf("inScope(%q) = %v, want %v", p, got, want)
		}
	}
	if !(&crawler{root: "/"}).inScope("/anything") {
		t.Error("root / covers everything")
	}
	for p, want := range map[string]string{"/pub/a": "/dest/host/pub/a", "/../../etc/x": "/dest/host/etc/x"} {
		if got := c.localPath(p); got != filepath.FromSlash(want) {
			t.Errorf("localPath(%q) = %q, want %q", p, got, want)
		}
	}
}
