package rsync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gokrazy/rsync/rsyncclient"
)

// ---- parseSource ----

func TestParseSource(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		want    rsyncSource
		wantErr bool
	}{
		{
			name: "rsync URL with module and subpath",
			src:  "rsync://host/module/sub/dir",
			want: rsyncSource{daemon: true, host: "host", port: "873", path: "module/sub/dir"},
		},
		{
			name: "rsync URL with explicit port",
			src:  "rsync://host:1234/module",
			want: rsyncSource{daemon: true, host: "host", port: "1234", path: "module"},
		},
		{
			name: "rsync URL with user",
			src:  "rsync://someone@host/module",
			want: rsyncSource{daemon: true, user: "someone", host: "host", port: "873", path: "module"},
		},
		{
			name: "rsync URL with no path at all",
			src:  "rsync://host",
			want: rsyncSource{daemon: true, host: "host", port: "873", path: ""},
		},
		{
			name: "a trailing slash on the path is preserved (content-vs-directory semantics)",
			src:  "rsync://host/module/sub/",
			want: rsyncSource{daemon: true, host: "host", port: "873", path: "module/sub/"},
		},
		{
			name: "host::module shorthand",
			src:  "host::module/sub",
			want: rsyncSource{daemon: true, host: "host", port: "873", path: "module/sub"},
		},
		{
			name: "user@host::module shorthand",
			src:  "someone@host::module",
			want: rsyncSource{daemon: true, user: "someone", host: "host", port: "873", path: "module"},
		},
		{
			name: "ssh-style host:path",
			src:  "host:/remote/path",
			want: rsyncSource{host: "host", path: "/remote/path"},
		},
		{
			name: "ssh-style user@host:path with a relative path",
			src:  "someone@host:relative/path",
			want: rsyncSource{user: "someone", host: "host", path: "relative/path"},
		},
		{
			name: "ssh-style path with a trailing slash preserved",
			src:  "host:/remote/path/",
			want: rsyncSource{host: "host", path: "/remote/path/"},
		},
		{
			name:    "no colon at all is not a valid source",
			src:     "just-a-bare-identifier",
			wantErr: true,
		},
		{
			name:    "empty string",
			src:     "",
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseSource(c.src)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if *got != c.want {
				t.Errorf("parseSource(%q) = %+v, want %+v", c.src, *got, c.want)
			}
		})
	}
}

// ---- rshCommand ----

func TestRSHCommand(t *testing.T) {
	t.Run("defaults to ssh when RSYNC_RSH is unset", func(t *testing.T) {
		t.Setenv("RSYNC_RSH", "")
		if got := rshCommand(); got != "ssh" {
			t.Errorf("got %q, want ssh", got)
		}
	})
	t.Run("honors RSYNC_RSH override", func(t *testing.T) {
		t.Setenv("RSYNC_RSH", "custom-rsh")
		if got := rshCommand(); got != "custom-rsh" {
			t.Errorf("got %q, want custom-rsh", got)
		}
	})
}

// ---- statsString ----

func TestStatsString(t *testing.T) {
	if got := statsString(nil); got != "no stats reported" {
		t.Errorf("nil result: got %q", got)
	}
	if got := statsString(&rsyncclient.Result{}); got != "no stats reported" {
		t.Errorf("nil Stats: got %q", got)
	}
}

// ---- dialShell: verifies the spawned command line and the plumbing of its
// stdin/stdout into the returned io.ReadWriteCloser, without needing a real
// ssh binary or a real rsync server on the other end. ----

func TestDialShellSpawnsExpectedCommandAndWiresPipes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test spawns a POSIX /bin/sh script")
	}

	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	script := filepath.Join(dir, "fake-rsh.sh")
	// Records the args it was called with, then echoes stdin back to
	// stdout until EOF — standing in for the remote rsync --server process
	// well enough to exercise shellConn's Read/Write/Close plumbing.
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGS_FILE\"\ncat\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RSYNC_RSH", script)
	t.Setenv("ARGS_FILE", argsFile)

	src := &rsyncSource{host: "myhost", user: "myuser"}
	serverArgs := []string{"--server", "--sender", "-a", ".", "/remote/path"}

	conn, err := dialShell(context.Background(), src, serverArgs)
	if err != nil {
		t.Fatal(err)
	}

	want := []byte("round-trip test\n")
	if _, err := conn.Write(want); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("round-trip: got %q, want %q", got, want)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	gotArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := "-l\nmyuser\nmyhost\nrsync\n--server\n--sender\n-a\n.\n/remote/path\n"
	if string(gotArgs) != wantArgs {
		t.Errorf("args = %q, want %q", gotArgs, wantArgs)
	}
}

func TestDialShellOmitsDashLWhenNoUser(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test spawns a POSIX /bin/sh script")
	}

	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	script := filepath.Join(dir, "fake-rsh.sh")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGS_FILE\"\ncat >/dev/null\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RSYNC_RSH", script)
	t.Setenv("ARGS_FILE", argsFile)

	src := &rsyncSource{host: "myhost"} // no user
	conn, err := dialShell(context.Background(), src, []string{"--server"})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	gotArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := "myhost\nrsync\n--server\n"; string(gotArgs) != want {
		t.Errorf("args = %q, want %q", gotArgs, want)
	}
}

// ---- Mirror: end-to-end against a real rsync daemon ----
//
// Spins up the actual `rsync` binary (tridge rsync or openrsync, whichever
// is on PATH) in --daemon mode against a throwaway module, then exercises
// Mirror's daemon path (parseSource, dialDaemon, rsyncclient.RunDaemon)
// exactly as a real user invocation would. Skipped if no rsync binary is
// available.
func TestMirrorEndToEndAgainstRealRsyncDaemon(t *testing.T) {
	rsyncBin, err := exec.LookPath("rsync")
	if err != nil {
		t.Skip("no rsync binary on PATH, skipping real-daemon integration test")
	}

	srcDir := t.TempDir()
	writeFile := func(rel, content string) {
		t.Helper()
		full := filepath.Join(srcDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("a.txt", "hello world")
	writeFile("sub/b.txt", "nested content")

	confPath := filepath.Join(t.TempDir(), "rsyncd.conf")
	conf := fmt.Sprintf("[testmod]\n   path = %s\n   read only = yes\n   use chroot = no\n", srcDir)
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}

	// Grab an ephemeral port by briefly listening on one, then hand it to
	// `rsync --daemon` via --port. Small TOCTOU race between the Close and
	// the daemon binding it, but negligible in practice for a test.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())

	var daemonOutput bytes.Buffer
	cmd := exec.CommandContext(ctx, rsyncBin, "--daemon", "--no-detach",
		"--config="+confPath, "--port="+port, "--address=127.0.0.1")
	cmd.Stdout = &daemonOutput
	cmd.Stderr = &daemonOutput
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s --daemon: %v", rsyncBin, err)
	}
	// The daemon never exits on its own (it's a persistent listener), so
	// cancel must run before Wait — a separate, later-registered t.Cleanup
	// for Wait would run first (t.Cleanup is LIFO) and block forever.
	t.Cleanup(func() {
		cancel()
		cmd.Wait()
	})

	addr := net.JoinHostPort("127.0.0.1", port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rsync daemon never started listening on %s; output so far:\n%s", addr, daemonOutput.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	destDir := t.TempDir()
	source := fmt.Sprintf("rsync://127.0.0.1:%s/testmod/", port)

	checkFile := func(rel, want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(destDir, rel))
		if err != nil {
			t.Fatalf("%s: %v; daemon output:\n%s", rel, err, daemonOutput.String())
		}
		if string(got) != want {
			t.Errorf("%s: got %q, want %q", rel, got, want)
		}
	}

	if err := Mirror(context.Background(), source, destDir, false); err != nil {
		t.Fatalf("Mirror: %v; daemon output:\n%s", err, daemonOutput.String())
	}
	checkFile("a.txt", "hello world")
	checkFile(filepath.Join("sub", "b.txt"), "nested content")

	// Incremental update: a modified file and a brand new one must both
	// show up on a second run against the same destDir.
	writeFile("a.txt", "updated content")
	writeFile("c.txt", "brand new file")
	if err := Mirror(context.Background(), source, destDir, false); err != nil {
		t.Fatalf("Mirror (incremental): %v; daemon output:\n%s", err, daemonOutput.String())
	}
	checkFile("a.txt", "updated content")
	checkFile("c.txt", "brand new file")

	// -delete: a file removed on the remote side must disappear locally.
	if err := os.Remove(filepath.Join(srcDir, "c.txt")); err != nil {
		t.Fatal(err)
	}
	if err := Mirror(context.Background(), source, destDir, true); err != nil {
		t.Fatalf("Mirror (-delete): %v; daemon output:\n%s", err, daemonOutput.String())
	}
	if _, err := os.Stat(filepath.Join(destDir, "c.txt")); !os.IsNotExist(err) {
		t.Errorf("c.txt should have been deleted locally after -delete, stat err = %v", err)
	}
}

func TestMirrorDaemonConnectionRefusedReturnsErrorPromptly(t *testing.T) {
	// An unused port on localhost: connection should be refused immediately,
	// not hang — Mirror must surface that as an error rather than block.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listening there now

	_, port, _ := net.SplitHostPort(addr)
	destDir := t.TempDir()
	source := fmt.Sprintf("rsync://127.0.0.1:%s/nomodule", port)

	done := make(chan error, 1)
	go func() { done <- Mirror(context.Background(), source, destDir, false) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error dialing a closed port")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Mirror did not return promptly against a refused connection")
	}
}
