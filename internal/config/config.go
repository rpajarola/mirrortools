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
	Auth        AuthConfig
}

// AuthConfig holds authentication settings for the web dashboard and HTTP API.
type AuthConfig struct {
	Password     string
	PasswordFile string
	OAuth        OAuthConfig
}

// PasswordValue returns the configured password or reads it from PasswordFile.
func (a *AuthConfig) PasswordValue() (string, error) {
	if a.Password != "" {
		return a.Password, nil
	}
	if a.PasswordFile != "" {
		b, err := os.ReadFile(a.PasswordFile)
		if err != nil {
			return "", fmt.Errorf("reading password file %s: %w", a.PasswordFile, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", nil
}

// Enabled reports whether any authentication method is configured.
func (a *AuthConfig) Enabled() bool {
	return a.Password != "" || a.PasswordFile != "" || a.OAuth.Enabled()
}

// OAuthConfig holds OAuth 2.0 / OpenID Connect settings.
type OAuthConfig struct {
	Provider         string // "google", "github", "generic"
	Issuer           string // OpenID Connect discovery issuer URL
	ClientID         string
	ClientSecret     string
	ClientSecretFile string
	AuthURL          string
	TokenURL         string
	UserInfoURL      string
	RedirectURL      string
	Scopes           []string
	AllowedUsers     []string
}

// Enabled reports whether OAuth authentication is configured.
func (o *OAuthConfig) Enabled() bool {
	return o.ClientID != "" && (o.ClientSecret != "" || o.ClientSecretFile != "")
}

// ClientSecretValue returns the configured client secret or reads it from ClientSecretFile.
func (o *OAuthConfig) ClientSecretValue() (string, error) {
	if o.ClientSecret != "" {
		return o.ClientSecret, nil
	}
	if o.ClientSecretFile != "" {
		b, err := os.ReadFile(o.ClientSecretFile)
		if err != nil {
			return "", fmt.Errorf("reading client secret file %s: %w", o.ClientSecretFile, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", nil
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
		if k == "auth" {
			subMap, ok := m[k].(map[string]any)
			if !ok {
				return fmt.Errorf("%s: [daemon.auth]: expected a table", path)
			}
			if err := parseAuthConfig(&d.Auth, subMap, path); err != nil {
				return err
			}
			continue
		}
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

func parseAuthConfig(a *AuthConfig, m map[string]any, path string) error {
	for _, k := range sortedKeys(m) {
		if k == "oauth" {
			subMap, ok := m[k].(map[string]any)
			if !ok {
				return fmt.Errorf("%s: [daemon.auth.oauth]: expected a table", path)
			}
			if err := parseOAuthConfig(&a.OAuth, subMap, path); err != nil {
				return err
			}
			continue
		}
		val, err := scalar(m[k])
		if err != nil {
			return fmt.Errorf("%s: [daemon.auth] %s: %v", path, k, err)
		}
		switch k {
		case "password":
			a.Password = val
		case "password-file", "password_file":
			a.PasswordFile = ExpandHome(val)
		default:
			return fmt.Errorf("%s: [daemon.auth]: unknown setting %q", path, k)
		}
	}
	return nil
}

func parseOAuthConfig(o *OAuthConfig, m map[string]any, path string) error {
	for _, k := range sortedKeys(m) {
		switch k {
		case "scopes":
			o.Scopes = stringList(m[k])
		case "allowed-users", "allowed_users":
			o.AllowedUsers = stringList(m[k])
		default:
			val, err := scalar(m[k])
			if err != nil {
				return fmt.Errorf("%s: [daemon.auth.oauth] %s: %v", path, k, err)
			}
			switch k {
			case "provider":
				o.Provider = val
			case "issuer":
				o.Issuer = val
			case "client-id", "client_id":
				o.ClientID = val
			case "client-secret", "client_secret":
				o.ClientSecret = val
			case "client-secret-file", "client_secret_file":
				o.ClientSecretFile = ExpandHome(val)
			case "auth-url", "auth_url":
				o.AuthURL = val
			case "token-url", "token_url":
				o.TokenURL = val
			case "userinfo-url", "userinfo_url":
				o.UserInfoURL = val
			case "redirect-url", "redirect_url":
				o.RedirectURL = val
			default:
				return fmt.Errorf("%s: [daemon.auth.oauth]: unknown setting %q", path, k)
			}
		}
	}
	return nil
}

func stringList(v any) []string {
	switch val := v.(type) {
	case []any:
		var res []string
		for _, item := range val {
			res = append(res, fmt.Sprint(item))
		}
		return res
	case []string:
		return val
	case string:
		var res []string
		for _, s := range strings.Split(val, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				res = append(res, s)
			}
		}
		return res
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
