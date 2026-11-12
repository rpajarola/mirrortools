package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iconfig "github.com/rpajarola/mirrortools/internal/config"
	"github.com/rpajarola/mirrortools/internal/daemon"

	// Register backends for auto-detection in tests
	_ "github.com/rpajarola/mirrortools/sourceforge"
)

func shortSocketPath(t *testing.T) string {
	d, err := os.MkdirTemp("/tmp", "mq")
	if err != nil {
		d = t.TempDir()
	} else {
		t.Cleanup(func() { os.RemoveAll(d) })
	}
	return filepath.Join(d, "s")
}

func startTestDaemon(t *testing.T) (string, string) {
	tempDir := t.TempDir()
	sockPath := shortSocketPath(t)
	dbPath := filepath.Join(tempDir, "test.db")

	store, err := daemon.OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	hub := daemon.NewLogHub(100)
	executor := &daemon.InProcessExecutor{}
	q := daemon.NewQueue(store, executor, hub, daemon.QueueConfig{Concurrency: 1})
	ctx := context.Background()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Queue.Start: %v", err)
	}
	t.Cleanup(func() { q.Stop() })

	cfg := &iconfig.Config{
		DestDir: filepath.Join(tempDir, "dest"),
		Daemon: iconfig.DaemonConfig{
			Socket: sockPath,
		},
	}

	server := daemon.NewServer(q, cfg, "", nil)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen(unix): %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	httpServer := &http.Server{Handler: server}
	go httpServer.Serve(listener)
	t.Cleanup(func() { httpServer.Close() })

	return sockPath, cfg.DestDir
}

func TestMirrorqHelpAndVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer

	// Help flags
	for _, arg := range []string{"-h", "--help", "help"} {
		stdout.Reset()
		stderr.Reset()
		code := run([]string{arg}, nil, &stdout, &stderr, true)
		if code != 0 {
			t.Errorf("run(%s) = %d; want 0", arg, code)
		}
		if !strings.Contains(stdout.String(), "usage: mirrorq") {
			t.Errorf("run(%s) stdout = %q; want usage", arg, stdout.String())
		}
	}

	// Version flags
	for _, arg := range []string{"-v", "--version", "version"} {
		stdout.Reset()
		stderr.Reset()
		code := run([]string{arg}, nil, &stdout, &stderr, true)
		if code != 0 {
			t.Errorf("run(%s) = %d; want 0", arg, code)
		}
		if !strings.Contains(stdout.String(), "mirrorq") {
			t.Errorf("run(%s) stdout = %q; want version", arg, stdout.String())
		}
	}
}

func TestMirrorqNoArgsTerminal(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{}, nil, &stdout, &stderr, true)
	if code != 2 {
		t.Fatalf("expected exit code 2 on empty terminal invocation, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: mirrorq") {
		t.Fatalf("expected usage on stderr, got %q", stderr.String())
	}
}

func TestMirrorqOffline(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-socket", "/tmp/nonexistent-mirrord-socket.sock", "https://sourceforge.net/projects/foo/"}, nil, &stdout, &stderr, true)
	if code != 1 {
		t.Fatalf("expected exit code 1 when daemon offline, got %d", code)
	}
	if !strings.Contains(stderr.String(), "cannot connect to mirrord") {
		t.Fatalf("expected connect error on stderr, got %q", stderr.String())
	}
}

func TestMirrorqEnqueueArgs(t *testing.T) {
	sockPath, _ := startTestDaemon(t)

	var stdout, stderr bytes.Buffer
	urls := []string{
		"https://sourceforge.net/projects/proj1/",
		"https://sourceforge.net/projects/proj2/",
	}

	args := append([]string{"-socket", sockPath}, urls...)
	code := run(args, nil, &stdout, &stderr, true)
	if code != 0 {
		t.Fatalf("run failed with code %d: stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "Enqueued job #1") || !strings.Contains(out, "Enqueued job #2") {
		t.Fatalf("expected both jobs enqueued in stdout, got:\n%s", out)
	}
	if !strings.Contains(out, "sourceforge") {
		t.Fatalf("expected auto-detected method sourceforge, got:\n%s", out)
	}
}

func TestMirrorqEnqueueStdinNonTerminal(t *testing.T) {
	sockPath, _ := startTestDaemon(t)

	stdin := strings.NewReader(`
# This is a comment
https://sourceforge.net/projects/stdin1/

  # Another comment with spaces
https://sourceforge.net/projects/stdin2/
`)

	var stdout, stderr bytes.Buffer
	args := []string{"-socket", sockPath}
	code := run(args, stdin, &stdout, &stderr, false)
	if code != 0 {
		t.Fatalf("run failed with code %d: stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "stdin1") || !strings.Contains(out, "stdin2") {
		t.Fatalf("expected stdin URLs enqueued, got:\n%s", out)
	}
}

func TestMirrorqCombinedArgsAndStdin(t *testing.T) {
	sockPath, _ := startTestDaemon(t)

	stdin := strings.NewReader("https://sourceforge.net/projects/from-stdin/\n")
	var stdout, stderr bytes.Buffer
	args := []string{"-socket", sockPath, "https://sourceforge.net/projects/from-args/"}
	code := run(args, stdin, &stdout, &stderr, false)
	if code != 0 {
		t.Fatalf("run failed with code %d: stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "from-args") || !strings.Contains(out, "from-stdin") {
		t.Fatalf("expected both arg and stdin URLs enqueued, got:\n%s", out)
	}
}

func TestMirrorqQuietAndCustomDest(t *testing.T) {
	sockPath, _ := startTestDaemon(t)

	var stdout, stderr bytes.Buffer
	customDest := "/tmp/custom-dest"
	args := []string{"-socket", sockPath, "-dest", customDest, "-quiet", "https://sourceforge.net/projects/quiet-job/"}
	code := run(args, nil, &stdout, &stderr, true)
	if code != 0 {
		t.Fatalf("run failed with code %d: stderr=%s", code, stderr.String())
	}

	if stdout.Len() > 0 {
		t.Fatalf("expected empty stdout with -quiet, got %q", stdout.String())
	}

	// Verify job was stored with custom destination
	client := daemon.NewUnixClient(sockPath)
	jobs, err := client.ListJobs(context.Background(), daemon.JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].DestDir != customDest {
		t.Fatalf("expected DestDir=%q, got %+v", customDest, jobs)
	}
}

func TestMirrorqWithFullConfigFile(t *testing.T) {
	sockPath, _ := startTestDaemon(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cfgContent := `
destdir = "/tmp/mirror"

[daemon]
socket = "` + sockPath + `"

[archiveorg]
wait = "1s"

[git]
destdir = "/tmp/git"

[http]
wait = "2s"

[sourceforge]
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	args := []string{"-config", cfgPath, "https://sourceforge.net/projects/cfg-test/"}
	code := run(args, nil, &stdout, &stderr, true)
	if code != 0 {
		t.Fatalf("run failed with code %d: stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Enqueued job") {
		t.Fatalf("expected job to be enqueued, got: %s", stdout.String())
	}
}

func TestMirrorqDeduplicationAndForce(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := shortSocketPath(t)
	dbPath := filepath.Join(tempDir, "test.db")

	store, err := daemon.OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	hub := daemon.NewLogHub(100)
	executor := &daemon.InProcessExecutor{
		CustomRunners: map[string]func(ctx context.Context, source, destDir string, flags map[string]string, onLog daemon.LogFunc) error{
			"sourceforge": func(ctx context.Context, source, destDir string, flags map[string]string, onLog daemon.LogFunc) error {
				return nil
			},
		},
	}
	q := daemon.NewQueue(store, executor, hub, daemon.QueueConfig{Concurrency: 1})
	ctx := context.Background()
	if err := q.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Queue.Start: %v", err)
	}
	t.Cleanup(func() { q.Stop() })

	cfg := &iconfig.Config{
		DestDir: filepath.Join(tempDir, "dest"),
		Daemon:  iconfig.DaemonConfig{Socket: sockPath},
	}
	server := daemon.NewServer(q, cfg, "", nil)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen(unix): %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	httpServer := &http.Server{Handler: server}
	go httpServer.Serve(listener)
	t.Cleanup(func() { httpServer.Close() })

	targetURL := "https://sourceforge.net/projects/same-proj/"

	// 1. Passing the same URL twice in one invocation creates job #1 once and skips the second
	var stdout, stderr bytes.Buffer
	code := run([]string{"-socket", sockPath, targetURL, targetURL}, nil, &stdout, &stderr, true)
	if code != 0 {
		t.Fatalf("run failed (%d): %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Enqueued job #1") {
		t.Fatalf("expected Enqueued job #1, got:\n%s", out)
	}
	if !strings.Contains(out, "Job #1 already exists [queued]") {
		t.Fatalf("expected Job #1 already exists [queued], got:\n%s", out)
	}
	if strings.Contains(out, "#2") {
		t.Fatalf("unexpected job #2 created:\n%s", out)
	}

	// 2. Mark job #1 completed; re-submitting without -f keeps job #1 completed and does not create #2
	if err := store.FinishJob(ctx, 1, daemon.StateCompleted, ""); err != nil {
		t.Fatalf("FinishJob: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"-socket", sockPath, targetURL}, nil, &stdout, &stderr, true)
	if code != 0 {
		t.Fatalf("run completed dup failed (%d): %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Job #1 already exists [completed]") {
		t.Fatalf("expected Job #1 already exists [completed], got:\n%s", stdout.String())
	}

	// 3. Re-submitting with -f re-queues job #1 under its existing ID
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"-socket", sockPath, "-f", targetURL}, nil, &stdout, &stderr, true)
	if code != 0 {
		t.Fatalf("run -f failed (%d): %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Re-queued job #1") {
		t.Fatalf("expected Re-queued job #1, got:\n%s", stdout.String())
	}

	jobs, err := store.ListJobs(ctx, daemon.JobFilter{})
	if err != nil || len(jobs) != 1 || jobs[0].ID != 1 || jobs[0].State != daemon.StateQueued {
		t.Fatalf("expected single queued job #1, got %+v (err=%v)", jobs, err)
	}
}
