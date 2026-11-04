// Command mirrord is the background mirroring daemon for mirrortools.
// It manages a persistent job queue, worker concurrency, failure quarantine,
// and serves both a local Unix domain socket (for CLI control) and an HTTP
// interface (for REST API and Web UI).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rpajarola/mirrortools/internal/config"
	"github.com/rpajarola/mirrortools/internal/daemon"
	"github.com/rpajarola/mirrortools/internal/version"
	"github.com/rpajarola/mirrortools/internal/web"

	// Register all mirroring backends
	_ "github.com/rpajarola/mirrortools/archiveorg"
	_ "github.com/rpajarola/mirrortools/ftpmirror"
	_ "github.com/rpajarola/mirrortools/git"
	_ "github.com/rpajarola/mirrortools/gopher"
	_ "github.com/rpajarola/mirrortools/gphotos"
	_ "github.com/rpajarola/mirrortools/groupsio"
	_ "github.com/rpajarola/mirrortools/httpmirror"
	_ "github.com/rpajarola/mirrortools/icloud"
	_ "github.com/rpajarola/mirrortools/imap"
	_ "github.com/rpajarola/mirrortools/rsync"
	_ "github.com/rpajarola/mirrortools/sourceforge"
)

func findMirrorBinary() string {
	// 1. Check if "mirror" is in the same directory as this executable
	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), "mirror")
		if fi, err := os.Stat(sibling); err == nil && !fi.IsDir() {
			return sibling
		}
	}
	// 2. Look up on PATH
	if p, err := exec.LookPath("mirror"); err == nil {
		return p
	}
	return ""
}

func main() {
	configFlag := flag.String("config", "", "path to config.toml")
	socketFlag := flag.String("socket", "", "path to unix domain socket")
	httpFlag := flag.String("http", "", "HTTP address to listen on for Web UI (e.g. 127.0.0.1:8080)")
	dbFlag := flag.String("db", "", "path to SQLite database")
	concFlag := flag.Int("concurrency", 0, "maximum concurrent mirror workers")
	verFlag := flag.Bool("version", false, "print mirrord version and exit")
	flag.Parse()

	if *verFlag {
		fmt.Printf("mirrord %s\n", version.String())
		return
	}

	confPath := *configFlag
	if confPath == "" {
		confPath = config.Path()
	}

	cfg, err := config.Load(confPath)
	if err != nil {
		log.Fatalf("mirrord: loading config %s: %v", confPath, err)
	}

	// Apply flag overrides
	socketPath := cfg.Daemon.Socket
	if *socketFlag != "" {
		socketPath = config.ExpandHome(*socketFlag)
	}
	httpAddr := cfg.Daemon.HTTP
	if *httpFlag != "" {
		httpAddr = *httpFlag
	}
	dbPath := cfg.Daemon.DB
	if *dbFlag != "" {
		dbPath = config.ExpandHome(*dbFlag)
	}
	concurrency := cfg.Daemon.Concurrency
	if *concFlag > 0 {
		concurrency = *concFlag
	}

	log.Printf("mirrord: starting mirrortools daemon %s", version.String())

	// Initialize Store
	store, err := daemon.OpenStore(dbPath)
	if err != nil {
		log.Fatalf("mirrord: opening database at %s: %v", dbPath, err)
	}
	defer store.Close()
	log.Printf("mirrord: database open at %s", dbPath)

	// Determine Executor
	var executor daemon.Executor
	mirrorBin := findMirrorBinary()
	if mirrorBin != "" {
		log.Printf("mirrord: using subprocess executor with %s", mirrorBin)
		executor = &daemon.CommandExecutor{BinaryPath: mirrorBin}
	} else {
		log.Printf("mirrord: 'mirror' binary not found on PATH; falling back to in-process executor")
		executor = &daemon.InProcessExecutor{}
	}

	// Initialize Queue & LogHub
	hub := daemon.NewLogHub(1000)
	q := daemon.NewQueue(store, executor, hub, daemon.QueueConfig{
		Concurrency: concurrency,
		MaxRetries:  cfg.Daemon.MaxRetries,
	})

	if err := q.Start(context.Background()); err != nil {
		log.Fatalf("mirrord: starting queue: %v", err)
	}
	defer q.Stop()

	// Initialize API & Web Server with embedded Web UI
	server := daemon.NewServer(q, cfg, confPath, web.FS())

	// Prepare Unix socket listener
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		log.Fatalf("mirrord: creating socket directory %s: %v", filepath.Dir(socketPath), err)
	}

	// Remove stale socket file if not accepting connections
	if conn, err := net.Dial("unix", socketPath); err == nil {
		conn.Close()
		log.Fatalf("mirrord: another instance is already listening on %s", socketPath)
	} else {
		_ = os.Remove(socketPath)
	}

	unixListener, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Fatalf("mirrord: listening on unix socket %s: %v", socketPath, err)
	}
	defer os.Remove(socketPath)
	_ = os.Chmod(socketPath, 0o660)

	log.Printf("mirrord: listening on unix socket %s", socketPath)

	httpServer := &http.Server{
		Handler:      server,
		ReadTimeout:  15 * time.Minute,
		WriteTimeout: 0, // 0 for streaming SSE
	}

	// Start Unix socket server
	go func() {
		if err := httpServer.Serve(unixListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("mirrord: unix server error: %v", err)
		}
	}()

	// Optionally start TCP HTTP server for Web UI
	if httpAddr != "" {
		tcpListener, err := net.Listen("tcp", httpAddr)
		if err != nil {
			log.Fatalf("mirrord: listening on http %s: %v", httpAddr, err)
		}
		log.Printf("mirrord: Web UI and HTTP API listening on http://%s", httpAddr)

		tcpServer := &http.Server{
			Handler:      server,
			ReadTimeout:  15 * time.Minute,
			WriteTimeout: 0,
		}
		go func() {
			if err := tcpServer.Serve(tcpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("mirrord: http server error: %v", err)
			}
		}()
		defer tcpServer.Close()
	}

	// Wait for OS shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("mirrord: received signal %s, shutting down gracefully...", sig)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = httpServer.Shutdown(shutdownCtx)
	log.Printf("mirrord: shutdown complete")
}
