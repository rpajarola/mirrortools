// Command mirror is the general-purpose CLI front-end for mirrortools: pick
// a method (gphotos, and whatever else gets registered), point it at a
// source (a URL, account email, or other unique identifier — whatever that
// method's usage text says it means) and a destination directory, and it
// mirrors.
//
// The method can be left out when the source says what it is (an
// archive.org or groups.io URL, ftp://, any other web URL, ...), and the
// destination directory when the config file names a default one. See
// config.go for the config file.
//
// Usage:
//
//	mirror [<method>] [method flags] <source> [<destdir>]
//	mirror version
//	mirror gphotos -cookies cookies.txt someone@gmail.com ./photos
//	mirror https://archive.org/details/some-item
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/rpajarola/mirrortools/internal/version"
	"github.com/rpajarola/mirrortools/mirror"

	// Blank-import every mirroring backend so its init() registers it.
	// Adding a new backend package here is the only wiring a new method
	// needs.
	_ "github.com/rpajarola/mirrortools/archiveorg"
	_ "github.com/rpajarola/mirrortools/ftpmirror"
	_ "github.com/rpajarola/mirrortools/gopher"
	_ "github.com/rpajarola/mirrortools/gphotos"
	_ "github.com/rpajarola/mirrortools/groupsio"
	_ "github.com/rpajarola/mirrortools/httpmirror"
	_ "github.com/rpajarola/mirrortools/icloud"
	_ "github.com/rpajarola/mirrortools/imap"
	_ "github.com/rpajarola/mirrortools/rsync"
)

func usage(w io.Writer) {
	fmt.Fprintf(w, "usage: %s [<method>] [flags] <source> [<destdir>]\n\n", os.Args[0])
	fmt.Fprintf(w, "methods (* = picked automatically for the sources it recognizes):\n")
	for _, name := range mirror.Names() {
		m, _ := mirror.Get(name)
		auto := " "
		if m.Match != nil {
			auto = "*"
		}
		fmt.Fprintf(w, "%s %-10s %s\n", auto, m.Name, m.Describe)
		fmt.Fprintf(w, "  %-10s source: %s\n", "", m.Source)
	}
	fmt.Fprintf(w, "\nwithout a <destdir>, the destdir from the config file is used: %s\n", configPath())
	fmt.Fprintf(w, "run `%s <method> -h` for method-specific flags, `%s version` for the version\n", os.Args[0], os.Args[0])
}

// errUsage is a command-line error that has already been reported along
// with the usage text.
var errUsage = errors.New("usage error")

// invocation is a resolved command line: what to run, and where.
type invocation struct {
	method *mirror.Method
	flags  *flag.FlagSet
	run    mirror.Func
	source string
	dest   string
	// detected and defaultDest say that the method and dest were chosen
	// by resolve rather than given, so main can say what it picked.
	detected, defaultDest bool
}

// resolve works out the method, flags, source and destination of a
// command line (without the program name), filling in what it leaves out
// from cfg. Usage text and flag errors go to stderr.
func resolve(args []string, cfg *config, stderr io.Writer) (*invocation, error) {
	inv := &invocation{}
	if m, ok := mirror.Get(args[0]); ok {
		inv.method = m
		args = args[1:]
	} else {
		// The source is the last argument, or the one before it if a
		// destdir follows; flags come first, as for a named method.
		for _, i := range []int{len(args) - 1, len(args) - 2} {
			if i < 0 || strings.HasPrefix(args[i], "-") {
				continue
			}
			if m, ok := mirror.Detect(args[i]); ok {
				inv.method, inv.source, inv.detected = m, args[i], true
				break
			}
		}
		if inv.method == nil {
			fmt.Fprintf(stderr, "%s: no method recognizes the source; name one, as in `%s <method> %s`\n\n",
				os.Args[0], os.Args[0], strings.Join(args, " "))
			usage(stderr)
			return nil, errUsage
		}
	}
	m := inv.method

	fs := flag.NewFlagSet(m.Name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: %s %s [flags] <source> [<destdir>]\n\n", os.Args[0], m.Name)
		fmt.Fprintf(stderr, "source: %s\n\n", m.Source)
		fmt.Fprintf(stderr, "flags:\n")
		fs.PrintDefaults()
	}
	inv.flags, inv.run = fs, m.SetupFlags(fs)
	mc := cfg.Methods[m.Name]
	if mc != nil {
		for name, value := range mc.Flags {
			if fs.Lookup(name) == nil {
				return nil, fmt.Errorf("config file: [%s] %s: %s has no flag -%s", m.Name, name, m.Name, name)
			}
			if err := fs.Set(name, value); err != nil {
				return nil, fmt.Errorf("config file: [%s] %s: %v", m.Name, name, err)
			}
		}
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, errUsage
	}

	rest := fs.Args()
	if len(rest) < 1 || len(rest) > 2 || (inv.detected && rest[0] != inv.source) {
		fs.Usage()
		return nil, errUsage
	}
	inv.source = rest[0]
	switch {
	case len(rest) == 2:
		inv.dest = rest[1]
	case mc != nil && mc.DestDir != "":
		inv.dest, inv.defaultDest = mc.DestDir, true
	case cfg.DestDir != "":
		inv.dest, inv.defaultDest = cfg.DestDir, true
		if m.Subdir != nil {
			sub := m.Subdir(inv.source)
			if sub == "" || !filepath.IsLocal(sub) {
				return nil, fmt.Errorf("%s: cannot tell where to mirror %s under %s; give a destdir", m.Name, inv.source, cfg.DestDir)
			}
			inv.dest = filepath.Join(inv.dest, sub)
		}
	default:
		fmt.Fprintf(stderr, "%s: no destdir given, and no default destdir in %s\n\n", os.Args[0], configPath())
		fs.Usage()
		return nil, errUsage
	}
	return inv, nil
}

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "-h", "-help", "--help":
		usage(os.Stdout)
		return
	case "version", "-version", "--version":
		fmt.Printf("mirror %s\n", version.String())
		return
	}

	cfg, err := loadConfig(configPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", os.Args[0], err)
		os.Exit(2)
	}
	inv, err := resolve(os.Args[1:], cfg, os.Stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return
	case errors.Is(err, errUsage):
		os.Exit(2)
	case err != nil:
		fmt.Fprintf(os.Stderr, "%s: %v\n", os.Args[0], err)
		os.Exit(2)
	}
	if inv.detected || inv.defaultDest {
		log.Printf("mirror: %s %s into %s", inv.method.Name, inv.source, inv.dest)
	}

	if err := os.MkdirAll(inv.dest, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "%s: creating %s: %v\n", os.Args[0], inv.dest, err)
		os.Exit(1)
	}

	if err := inv.run(context.Background(), inv.source, inv.dest); err != nil {
		fmt.Fprintf(os.Stderr, "%s %s: %v\n", os.Args[0], inv.method.Name, err)
		os.Exit(1)
	}
}
