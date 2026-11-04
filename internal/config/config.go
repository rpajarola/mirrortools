package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/rpajarola/mirrortools/mirror"
)

// Config represents mirrortools' configuration file in TOML:
//
//	# default destination directory
//	destdir = "~/mirror"
//
//	# mirrord daemon settings
//	[daemon]
//	socket = "~/.local/state/mirrortools/mirrord.sock"
//	http = "127.0.0.1:8080"
//	concurrency = 3
//	db = "~/.local/state/mirrortools/mirrord.db"
//	max_retries = 3
//
//	# per-method configuration
//	[groupsio]
//	email = "me@example.com"
//	password-file = "~/.config/mirrortools/groupsio-password"
type Config struct {
	DestDir string
	Daemon  DaemonConfig
	Methods map[string]*MethodConfig
}

// DaemonConfig holds settings for the mirrord service.
type DaemonConfig struct {
	Socket      string
	HTTP        string
	Concurrency int
	DB          string
	MaxRetries  int
}

// MethodConfig holds default destination and flags for a specific mirroring method.
type MethodConfig struct {
	DestDir string
	Flags   map[string]string // flag name (without "-") → value
}

// DefaultSocketPath returns the standard Unix domain socket location.
func DefaultSocketPath() string {
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		return filepath.Join(runtimeDir, "mirrortools", "mirrord.sock")
	}
	stateDir := os.Getenv("XDG_STATE_HOME")
	if stateDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			stateDir = filepath.Join(home, ".local", "state")
		}
	}
	if stateDir != "" {
		return filepath.Join(stateDir, "mirrortools", "mirrord.sock")
	}
	return filepath.Join(os.TempDir(), "mirrord.sock")
}

// DefaultDBPath returns the standard SQLite database file location.
func DefaultDBPath() string {
	stateDir := os.Getenv("XDG_STATE_HOME")
	if stateDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			stateDir = filepath.Join(home, ".local", "state")
		}
	}
	if stateDir != "" {
		return filepath.Join(stateDir, "mirrortools", "mirrord.db")
	}
	return filepath.Join(os.TempDir(), "mirrord.db")
}

// Path returns $MIRROR_CONFIG if set, else config.toml in mirrortools'
// directory under $XDG_CONFIG_HOME or ~/.config.
func Path() string {
	if p := os.Getenv("MIRROR_CONFIG"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "mirrortools", "config.toml")
}

// Load reads the config file at path. A missing file returns a default config.
func Load(path string) (*Config, error) {
	cfg := &Config{
		Methods: map[string]*MethodConfig{},
		Daemon: DaemonConfig{
			Socket:      DefaultSocketPath(),
			HTTP:        "", // empty means HTTP disabled by default (unix socket only)
			Concurrency: 2,
			DB:          DefaultDBPath(),
			MaxRetries:  3,
		},
	}
	if path == "" {
		return cfg, nil
	}
	var raw map[string]any
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("%s: %v", path, err)
	}

	for _, key := range sortedKeys(raw) {
		switch v := raw[key].(type) {
		case map[string]any:
			if key == "daemon" {
				if err := parseDaemonConfig(&cfg.Daemon, v, path); err != nil {
					return nil, err
				}
				continue
			}
			if _, ok := mirror.Get(key); !ok {
				return nil, fmt.Errorf("%s: [%s]: no such method", path, key)
			}
			mc := &MethodConfig{Flags: map[string]string{}}
			for _, k := range sortedKeys(v) {
				val, err := scalar(v[k])
				if err != nil {
					return nil, fmt.Errorf("%s: [%s] %s: %v", path, key, k, err)
				}
				if k == "destdir" {
					mc.DestDir = ExpandHome(val)
				} else {
					mc.Flags[k] = ExpandHome(val)
				}
			}
			cfg.Methods[key] = mc
		default:
			if key != "destdir" {
				return nil, fmt.Errorf("%s: unknown setting %q (method settings go in a [method] section)", path, key)
			}
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("%s: destdir: want a string", path)
			}
			cfg.DestDir = ExpandHome(s)
		}
	}
	return cfg, nil
}

func parseDaemonConfig(d *DaemonConfig, m map[string]any, path string) error {
	for _, k := range sortedKeys(m) {
		val, err := scalar(m[k])
		if err != nil {
			return fmt.Errorf("%s: [daemon] %s: %v", path, k, err)
		}
		switch k {
		case "socket":
			d.Socket = ExpandHome(val)
		case "http":
			d.HTTP = val
		case "concurrency":
			var c int
			if _, err := fmt.Sscanf(val, "%d", &c); err != nil || c < 1 {
				return fmt.Errorf("%s: [daemon] concurrency: want a positive number", path)
			}
			d.Concurrency = c
		case "db":
			d.DB = ExpandHome(val)
		case "max_retries":
			var r int
			if _, err := fmt.Sscanf(val, "%d", &r); err != nil || r < 1 {
				return fmt.Errorf("%s: [daemon] max_retries: want a positive number", path)
			}
			d.MaxRetries = r
		default:
			return fmt.Errorf("%s: [daemon]: unknown setting %q", path, k)
		}
	}
	return nil
}

// scalar formats a TOML value as a string.
func scalar(v any) (string, error) {
	switch v := v.(type) {
	case string:
		return v, nil
	case bool, int64, float64:
		return fmt.Sprint(v), nil
	}
	return "", fmt.Errorf("want a string, boolean or number, not %T", v)
}

// ExpandHome replaces a leading ~/ with the user's home directory.
func ExpandHome(s string) string {
	if s != "~" && !strings.HasPrefix(s, "~/") {
		return s
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return s
	}
	return filepath.Join(home, s[1:])
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
