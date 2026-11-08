package main

import (
	"bytes"
	"context"
	"database/sql"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	iconfig "github.com/rpajarola/mirrortools/internal/config"
	"github.com/rpajarola/mirrortools/internal/daemon"
)

func shortSocketPath(t *testing.T) string {
	d, err := os.MkdirTemp("/tmp", "md")
	if err != nil {
		d = t.TempDir()
	} else {
		t.Cleanup(func() { os.RemoveAll(d) })
	}
	return filepath.Join(d, "s")
}

func TestCLIQueueIntegration(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := shortSocketPath(t)
	dbPath := filepath.Join(tempDir, "test-mirrord.db")

	store, err := daemon.OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	hub := daemon.NewLogHub(100)
	executor := &daemon.InProcessExecutor{
		CustomRunners: map[string]func(ctx context.Context, source, destDir string, flags map[string]string, onLog daemon.LogFunc) error{
			"sourceforge": func(ctx context.Context, source, destDir string, flags map[string]string, onLog daemon.LogFunc) error {
				if onLog != nil {
					onLog("stdout", "test runner starting")
					onLog("stdout", "all files downloaded")
				}
				return nil
			},
		},
	}

	q := daemon.NewQueue(store, executor, hub, daemon.QueueConfig{Concurrency: 2})
	ctx := context.Background()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer q.Stop()

	cfg := &config{
		DestDir: filepath.Join(tempDir, "dest"),
		Daemon: iconfig.DaemonConfig{
			Socket: sockPath,
		},
		Methods: map[string]*methodConfig{},
	}

	server := daemon.NewServer(q, cfg, "", nil)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen(unix): %v", err)
	}
	defer listener.Close()

	httpServer := &http.Server{Handler: server}
	go httpServer.Serve(listener)
	defer httpServer.Close()

	client := daemon.NewUnixClient(sockPath)

	// 1. Submit a job via client
	job, err := client.SubmitJob(ctx, &daemon.SubmitRequest{
		Method:  "sourceforge",
		Source:  "https://sourceforge.net/projects/test/",
		DestDir: filepath.Join(tempDir, "dest"),
	})
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	if job.ID == 0 {
		t.Fatalf("expected job.ID > 0")
	}

	// Wait for job execution to complete
	deadline := time.Now().Add(2 * time.Second)
	var completed *daemon.Job
	for time.Now().Before(deadline) {
		j, err := client.GetJob(ctx, job.ID)
		if err == nil && j.State == daemon.StateCompleted {
			completed = j
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if completed == nil {
		t.Fatalf("job did not complete within deadline")
	}

	// Check logs
	logs, err := client.GetLogs(ctx, job.ID, 10, 0)
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("expected 2 log lines, got %d", len(logs))
	}

	// 2. Test queue pause/resume
	if err := client.PauseQueue(ctx); err != nil {
		t.Fatalf("PauseQueue: %v", err)
	}
	status, err := client.GetStatus(ctx)
	if err != nil || !status.Paused {
		t.Fatalf("expected status.Paused=true, got %+v", status)
	}
	if err := client.ResumeQueue(ctx); err != nil {
		t.Fatalf("ResumeQueue: %v", err)
	}
	status, err = client.GetStatus(ctx)
	if err != nil || status.Paused {
		t.Fatalf("expected status.Paused=false, got %+v", status)
	}
}

func TestCLIOfflineErrorMessage(t *testing.T) {
	tempDir := t.TempDir()
	nonExistentSocket := filepath.Join(tempDir, "absent.sock")
	client := daemon.NewUnixClient(nonExistentSocket)

	_, err := client.GetStatus(context.Background())
	if err == nil {
		t.Fatalf("expected error connecting to non-existent socket")
	}
	if !strings.Contains(err.Error(), "no such file or directory") && !strings.Contains(err.Error(), "connect: connection refused") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("got %q, want short", got)
	}
	if got := truncate("this is a very long string", 10); got != "this is..." {
		t.Errorf("got %q, want this is...", got)
	}
}

func TestSubmitMainWithResolve(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := shortSocketPath(t)

	cfg := &config{
		DestDir: filepath.Join(tempDir, "dest"),
		Daemon: iconfig.DaemonConfig{
			Socket: sockPath,
		},
		Methods: map[string]*methodConfig{},
	}

	// Test submit with -h
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	submitMain([]string{"-h"}, cfg)

	w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	buf.ReadFrom(r)
	if !strings.Contains(buf.String(), "usage: mirror queue submit") {
		t.Errorf("expected usage output, got %q", buf.String())
	}
}

func TestCLIQueueTransfersAndIgnores(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := shortSocketPath(t)
	dbPath := filepath.Join(tempDir, "test-mirrord.db")
	siteDestDir := filepath.Join(tempDir, "dest", "example.com")
	if err := os.MkdirAll(siteDestDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Create a dummy .httpmirror.db
	httpDBPath := filepath.Join(siteDestDir, ".httpmirror.db")
	db, err := sql.Open("sqlite", httpDBPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE urls (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			url TEXT NOT NULL UNIQUE,
			requisite INTEGER NOT NULL DEFAULT 0,
			state TEXT NOT NULL DEFAULT 'pending',
			status INTEGER,
			error TEXT,
			etag TEXT,
			last_modified TEXT,
			path TEXT
		);
		CREATE TABLE ignored_patterns (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pattern TEXT NOT NULL UNIQUE,
			created_at DATETIME NOT NULL
		);
		INSERT INTO urls (url, requisite, state, status) VALUES ('https://example.com/index.html', 0, 'done', 200);
		INSERT INTO urls (url, requisite, state, status, error) VALUES ('https://deadhost.com/icon.png', 1, 'error', 0, 'connection refused');
	`)
	db.Close()
	if err != nil {
		t.Fatalf("exec dummy schema: %v", err)
	}

	store, err := daemon.OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	hub := daemon.NewLogHub(100)
	executor := &daemon.InProcessExecutor{}
	q := daemon.NewQueue(store, executor, hub, daemon.QueueConfig{Concurrency: 1})
	ctx := context.Background()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer q.Stop()

	cfg := &config{
		DestDir: filepath.Join(tempDir, "dest"),
		Daemon: iconfig.DaemonConfig{
			Socket: sockPath,
		},
		Methods: map[string]*methodConfig{},
	}

	server := daemon.NewServer(q, cfg, "", nil)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen(unix): %v", err)
	}
	defer listener.Close()

	httpServer := &http.Server{Handler: server}
	go httpServer.Serve(listener)
	defer httpServer.Close()

	client := daemon.NewUnixClient(sockPath)
	_, err = client.SubmitJob(ctx, &daemon.SubmitRequest{
		Method:  "http",
		Source:  "https://example.com/",
		DestDir: siteDestDir,
	})
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}

	runCapture := func(args []string) string {
		oldStdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		queueMain(args, cfg)

		w.Close()
		os.Stdout = oldStdout
		var b bytes.Buffer
		b.ReadFrom(r)
		return b.String()
	}

	// 1. transfers
	out := runCapture([]string{"transfers", "example.com"})
	if !strings.Contains(out, "example.com") || !strings.Contains(out, "https://deadhost.com/icon.png") {
		t.Fatalf("expected transfers listing, got: %s", out)
	}

	// 2. disable URL
	out = runCapture([]string{"disable", "example.com", "https://deadhost.com/icon.png"})
	if !strings.Contains(out, "Disabled transfer") {
		t.Fatalf("expected disabled output, got: %s", out)
	}

	// Verify it shows disabled
	out = runCapture([]string{"transfers", "-category", "disabled", "example.com"})
	if !strings.Contains(out, "disabled") || !strings.Contains(out, "https://deadhost.com/icon.png") {
		t.Fatalf("expected disabled transfers listing, got: %s", out)
	}

	// 3. enable URL
	out = runCapture([]string{"enable", "example.com", "https://deadhost.com/icon.png"})
	if !strings.Contains(out, "Re-enabled transfer") {
		t.Fatalf("expected re-enabled output, got: %s", out)
	}

	// 4. ignore pattern
	out = runCapture([]string{"ignore", "example.com", "deadhost.com"})
	if !strings.Contains(out, "Added ignore pattern") {
		t.Fatalf("expected added ignore pattern, got: %s", out)
	}

	// 5. list ignores
	out = runCapture([]string{"ignores", "example.com"})
	if !strings.Contains(out, "deadhost.com") {
		t.Fatalf("expected ignores listing deadhost.com, got: %s", out)
	}

	// 6. unignore pattern
	out = runCapture([]string{"unignore", "example.com", "deadhost.com"})
	if !strings.Contains(out, "Removed ignore pattern") {
		t.Fatalf("expected removed ignore pattern, got: %s", out)
	}
}
