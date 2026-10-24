package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDetect(t *testing.T) {
	for _, tc := range []struct{ args, method string }{
		{"https://archive.org/details/some-item", "archiveorg"},
		{"https://www.archive.org/download/some-item/file.zip", "archiveorg"},
		{"https://groups.io/g/somegroup", "groupsio"},
		{"https://groups.io/g/somegroup/files/sub", "groupsio"},
		{"https://example.com/pub/", "http"},
		{"http://archive.org/", "http"},
		{"https://groups.io/", "http"},
		{"ftp://ftp.example.com/pub/", "ftp"},
		{"ftps://ftp.example.com/pub/", "ftp"},
		{"gopher://gopher.example.com/1/", "gopher"},
		{"rsync://rsync.example.com/module/", "rsync"},
		{"rsync.example.com::module/dir", "rsync"},
	} {
		inv, err := resolve([]string{tc.args, "/dest"}, &config{}, io.Discard)
		if err != nil {
			t.Errorf("%s: %v", tc.args, err)
			continue
		}
		if inv.method.Name != tc.method || !inv.detected {
			t.Errorf("%s: got method %s (detected %v), want %s", tc.args, inv.method.Name, inv.detected, tc.method)
		}
	}
	for _, src := range []string{"someone@example.com", "imap.example.com:993", "host:/path", "./dir"} {
		if _, err := resolve([]string{src, "/dest"}, &config{}, io.Discard); !errors.Is(err, errUsage) {
			t.Errorf("%s: got %v, want a usage error", src, err)
		}
	}
}

func TestResolveArgs(t *testing.T) {
	cfg := &config{
		DestDir: "/m",
		Methods: map[string]*methodConfig{
			"http":     {Flags: map[string]string{"wait": "2s"}},
			"groupsio": {DestDir: "/groups", Flags: map[string]string{"email": "me@example.com"}},
		},
	}
	for _, tc := range []struct {
		args                []string
		method, src, dest   string
		detected, defaulted bool
	}{
		{[]string{"https://example.com/a/"}, "http", "https://example.com/a/", "/m", true, true},
		{[]string{"-update", "https://example.com/a/", "/d"}, "http", "https://example.com/a/", "/d", true, false},
		{[]string{"-wait", "1s", "https://example.com/a/"}, "http", "https://example.com/a/", "/m", true, true},
		{[]string{"http", "https://archive.org/details/x"}, "http", "https://archive.org/details/x", "/m", false, true},
		{[]string{"https://groups.io/g/xyz"}, "groupsio", "https://groups.io/g/xyz", "/groups", true, true},
		{[]string{"gphotos", "me@gmail.com"}, "gphotos", "me@gmail.com", "/m/gphotos/me@gmail.com", false, true},
		{[]string{"gphotos", "me@gmail.com", "/p"}, "gphotos", "me@gmail.com", "/p", false, false},
		{[]string{"imap", "-user", "u", "mail.example.com:993"}, "imap", "mail.example.com:993", "/m/imap/mail.example.com", false, true},
		{[]string{"rsync://h/mod/dir"}, "rsync", "rsync://h/mod/dir", "/m/h/mod", true, true},
		{[]string{"rsync://h/mod/dir/"}, "rsync", "rsync://h/mod/dir/", "/m/h/mod/dir", true, true},
		{[]string{"h::mod"}, "rsync", "h::mod", "/m/h", true, true},
		{[]string{"rsync", "u@h:/srv/../data/"}, "rsync", "u@h:/srv/../data/", "/m/h/data", false, true},
	} {
		inv, err := resolve(tc.args, cfg, io.Discard)
		if err != nil {
			t.Errorf("%q: %v", tc.args, err)
			continue
		}
		dest := filepath.FromSlash(tc.dest)
		if inv.method.Name != tc.method || inv.source != tc.src || inv.dest != dest ||
			inv.detected != tc.detected || inv.defaultDest != tc.defaulted {
			t.Errorf("%q: got %s %s %s (detected %v, default dest %v), want %s %s %s (%v, %v)", tc.args,
				inv.method.Name, inv.source, inv.dest, inv.detected, inv.defaultDest,
				tc.method, tc.src, dest, tc.detected, tc.defaulted)
		}
	}
}

func TestResolveErrors(t *testing.T) {
	noDest := &config{}
	if _, err := resolve([]string{"https://example.com/"}, noDest, io.Discard); !errors.Is(err, errUsage) {
		t.Errorf("no destdir: got %v, want a usage error", err)
	}
	if _, err := resolve([]string{"http", "-h"}, noDest, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: got %v, want flag.ErrHelp", err)
	}
	if _, err := resolve([]string{"https://example.com/", "/a", "/b"}, noDest, io.Discard); !errors.Is(err, errUsage) {
		t.Errorf("extra argument: got %v, want a usage error", err)
	}
	badFlag := &config{Methods: map[string]*methodConfig{"http": {Flags: map[string]string{"nope": "1"}}}}
	if _, err := resolve([]string{"https://example.com/", "/d"}, badFlag, io.Discard); err == nil || errors.Is(err, errUsage) {
		t.Errorf("unknown config flag: got %v, want an error naming it", err)
	}
	badValue := &config{Methods: map[string]*methodConfig{"http": {Flags: map[string]string{"wait": "soon"}}}}
	if _, err := resolve([]string{"https://example.com/", "/d"}, badValue, io.Discard); err == nil {
		t.Error("bad config flag value: got no error")
	}
}

func TestFlagsOverrideConfig(t *testing.T) {
	cfg := &config{Methods: map[string]*methodConfig{"ftp": {Flags: map[string]string{"user": "fromconfig"}}}}
	inv, err := resolve([]string{"-user", "fromflag", "ftp://h/", "/d"}, cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := inv.flags.Lookup("user").Value.String(); got != "fromflag" {
		t.Errorf("-user: got %q, want the command line to win", got)
	}
	inv, err = resolve([]string{"ftp://h/", "/d"}, cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := inv.flags.Lookup("user").Value.String(); got != "fromconfig" {
		t.Errorf("-user: got %q, want the config file's", got)
	}
}

func TestLoadConfig(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	os.WriteFile(p, []byte(`
destdir = "~/mirror"

[groupsio]
email = "me@example.com"
password-file = "~/.groupsio-pw"

[http]
destdir = "/web"
wait = "1s"
update = true
`), 0o644)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "mirror"); cfg.DestDir != want {
		t.Errorf("destdir: got %q, want %q", cfg.DestDir, want)
	}
	g := cfg.Methods["groupsio"]
	if g == nil || g.Flags["email"] != "me@example.com" || g.Flags["password-file"] != filepath.Join(home, ".groupsio-pw") {
		t.Errorf("groupsio: got %+v", g)
	}
	h := cfg.Methods["http"]
	if h == nil || h.DestDir != "/web" || h.Flags["wait"] != "1s" || h.Flags["update"] != "true" {
		t.Errorf("http: got %+v", h)
	}

	if cfg, err := loadConfig(filepath.Join(dir, "missing.toml")); err != nil || cfg.DestDir != "" {
		t.Errorf("missing file: got %+v, %v; want an empty config", cfg, err)
	}

	os.WriteFile(p, []byte(`dest = "/x"`), 0o644)
	if _, err := loadConfig(p); err == nil {
		t.Error("unknown top-level setting: got no error")
	}
	os.WriteFile(p, []byte("[htp]\nwait = \"1s\"\n"), 0o644)
	if _, err := loadConfig(p); err == nil {
		t.Error("unknown method: got no error")
	}
	os.WriteFile(p, []byte("[http]\nwait = [1, 2]\n"), 0o644)
	if _, err := loadConfig(p); err == nil {
		t.Error("array value: got no error")
	}
}
