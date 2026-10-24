// Package mirror defines the common interface that mirrortools' various
// mirroring backends (gphotos, and whatever gets added after it) implement,
// plus the registry that lets the mirror CLI (cmd/mirror) discover them.
//
// A backend identifies what to mirror with a single "source" string — a URL,
// an account email, or whatever else uniquely identifies the thing for that
// method — and a destination directory to mirror it into. Everything else
// (auth, filters, pacing, ...) is the backend's own flags, registered via
// Method.SetupFlags.
package mirror

import (
	"context"
	"flag"
	"path/filepath"
	"sort"
	"strings"
)

// Func mirrors everything reachable from source into destDir, which the
// caller guarantees exists. source is a URL, account identifier, or other
// unique handle — whatever the owning Method's Source field says it means.
type Func func(ctx context.Context, source, destDir string) error

// Method is one pluggable mirroring backend, e.g. "gphotos".
type Method struct {
	// Name is the CLI subcommand name, e.g. "gphotos". Must be unique across
	// all registered methods.
	Name string
	// Source describes what the "source" argument means for this method,
	// e.g. "Google account email" — shown in the CLI's usage text.
	Source string
	// Describe is a one-line summary of the method, shown in top-level usage.
	Describe string
	// SetupFlags registers this method's own flags (e.g. -cookies) on fs and
	// returns the Func that performs the mirror using their parsed values.
	// It's called before fs.Parse, so the returned Func must read flag
	// values through the pointers fs.String/fs.Bool/etc. returned, not by
	// closing over them directly.
	SetupFlags func(fs *flag.FlagSet) Func

	// Match reports whether source is something this method mirrors, so
	// the CLI can pick the method itself when the user doesn't name one.
	// Nil means the method is only used when named, e.g. because its
	// sources (an email address) look the same as another method's.
	Match func(source string) bool
	// Fallback marks a Match as generic: the method is picked only when no
	// other method's Match accepts the source, as http is for URLs that
	// a more specific method, such as archiveorg, doesn't claim.
	Fallback bool
	// Subdir, if set, names the subdirectory a source is mirrored into
	// when the destination is the CLI's shared default directory rather
	// than one the user gave. Methods that already mirror into a
	// subdirectory of their own, such as <destDir>/<host>/..., leave it
	// nil; methods that fill destDir directly set it, so that different
	// sources don't end up mixed together.
	Subdir func(source string) string
}

var registry = map[string]*Method{}

// Register adds a Method to the registry. Backends call this from their own
// init() so that blank-importing the package (e.g. `_
// "github.com/rpajarola/mirrortools/gphotos"`) is enough to make them
// available to the CLI. It panics on an empty or duplicate name, since both
// are programming errors rather than runtime conditions.
func Register(m *Method) {
	if m.Name == "" {
		panic("mirror: Register: empty Name")
	}
	if _, dup := registry[m.Name]; dup {
		panic("mirror: Register: duplicate method " + m.Name)
	}
	registry[m.Name] = m
}

// Get looks up a registered Method by name.
func Get(name string) (*Method, bool) {
	m, ok := registry[name]
	return m, ok
}

// Detect picks the method for source among those with a Match: a
// specific method if one matches, else a Fallback one. If several
// match at the same level, the first by name wins.
func Detect(source string) (*Method, bool) {
	var fallback *Method
	for _, name := range Names() {
		m := registry[name]
		if m.Match == nil || !m.Match(source) {
			continue
		}
		if !m.Fallback {
			return m, true
		}
		if fallback == nil {
			fallback = m
		}
	}
	return fallback, fallback != nil
}

// Names returns all registered method names, sorted alphabetically.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// SafeName joins method and an identifier from a source, such as an
// account or host name, into a relative path for a Subdir, keeping the
// identifier a single path element.
func SafeName(method, id string) string {
	id = strings.NewReplacer("/", "_", "\\", "_").Replace(id)
	if id == "" || id == "." || id == ".." {
		id = "_"
	}
	return filepath.Join(method, id)
}
