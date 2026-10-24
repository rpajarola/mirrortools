package main

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

// config is the mirror CLI's config file, TOML:
//
//	# where to mirror when no destdir is given
//	destdir = "~/mirror"
//
//	# per method: its own default destdir, and default values for its flags
//	[groupsio]
//	email = "me@example.com"
//	password-file = "~/.config/mirrortools/groupsio-password"
//
//	[http]
//	destdir = "~/web"
//	wait = "1s"
type config struct {
	DestDir string
	Methods map[string]*methodConfig
}

type methodConfig struct {
	DestDir string
	Flags   map[string]string // flag name (without "-") → value
}

// configPath is $MIRROR_CONFIG if set, else config.toml in mirrortools'
// directory under $XDG_CONFIG_HOME or ~/.config — on macOS as well, as
// for most command-line tools, rather than ~/Library/Application Support.
func configPath() string {
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

// loadConfig reads the config file at path. A missing file is an empty
// config.
func loadConfig(path string) (*config, error) {
	cfg := &config{Methods: map[string]*methodConfig{}}
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
			if _, ok := mirror.Get(key); !ok {
				return nil, fmt.Errorf("%s: [%s]: no such method", path, key)
			}
			mc := &methodConfig{Flags: map[string]string{}}
			for _, k := range sortedKeys(v) {
				val, err := scalar(v[k])
				if err != nil {
					return nil, fmt.Errorf("%s: [%s] %s: %v", path, key, k, err)
				}
				if k == "destdir" {
					mc.DestDir = expandHome(val)
				} else {
					mc.Flags[k] = expandHome(val)
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
			cfg.DestDir = expandHome(s)
		}
	}
	return cfg, nil
}

// scalar formats a TOML value as a flag value: strings as they are,
// booleans and numbers the way the flag package parses them.
func scalar(v any) (string, error) {
	switch v := v.(type) {
	case string:
		return v, nil
	case bool, int64, float64:
		return fmt.Sprint(v), nil
	}
	return "", fmt.Errorf("want a string, boolean or number, not %T", v)
}

// expandHome replaces a leading ~/ with the home directory, which a
// shell would do for the same value on the command line.
func expandHome(s string) string {
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
