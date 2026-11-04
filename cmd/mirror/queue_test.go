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
	"time"

	iconfig "github.com/rpajarola/mirrortools/internal/config"
	"github.com/rpajarola/mirrortools/internal/daemon"
)

func TestCLIQueueIntegration(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := filepath.Join(tempDir, "test-mirrord.sock")
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
	sockPath := filepath.Join(tempDir, "mock.sock")

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
