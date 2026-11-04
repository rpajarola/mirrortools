package config

import (
	"os"
	"path/filepath"
	"testing"

	_ "github.com/rpajarola/mirrortools/httpmirror"
)

func TestDaemonConfigParsing(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "config.toml")
	content := `
destdir = "~/all_mirrors"

[daemon]
socket = "/run/user/1000/mirrord.sock"
http = "127.0.0.1:9090"
concurrency = 4
db = "/var/lib/mirrord.db"
max_retries = 5

[http]
wait = "2s"
`
	if err := os.WriteFile(confPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(confPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Daemon.Socket != "/run/user/1000/mirrord.sock" {
		t.Errorf("got socket %q", cfg.Daemon.Socket)
	}
	if cfg.Daemon.HTTP != "127.0.0.1:9090" {
		t.Errorf("got http %q", cfg.Daemon.HTTP)
	}
	if cfg.Daemon.Concurrency != 4 {
		t.Errorf("got concurrency %d", cfg.Daemon.Concurrency)
	}
	if cfg.Daemon.DB != "/var/lib/mirrord.db" {
		t.Errorf("got db %q", cfg.Daemon.DB)
	}
	if cfg.Daemon.MaxRetries != 5 {
		t.Errorf("got max_retries %d", cfg.Daemon.MaxRetries)
	}
	if cfg.Methods["http"].Flags["wait"] != "2s" {
		t.Errorf("got http.wait %q", cfg.Methods["http"].Flags["wait"])
	}
}
