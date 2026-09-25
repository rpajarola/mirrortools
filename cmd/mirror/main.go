// Command mirror is the general-purpose CLI front-end for mirrortools: pick
// a method (gphotos, and whatever else gets registered), point it at a
// source (a URL, account email, or other unique identifier — whatever that
// method's usage text says it means) and a destination directory, and it
// mirrors.
//
// Usage:
//
//	mirror <method> [method flags] <source> <destdir>
//	mirror gphotos -cookies cookies.txt someone@gmail.com ./photos
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/rpajarola/mirrortools/mirror"

	// Blank-import every mirroring backend so its init() registers it.
	// Adding a new backend package here is the only wiring a new method
	// needs.
	_ "github.com/rpajarola/mirrortools/archiveorg"
	_ "github.com/rpajarola/mirrortools/gopher"
	_ "github.com/rpajarola/mirrortools/gphotos"
	_ "github.com/rpajarola/mirrortools/groupsio"
	_ "github.com/rpajarola/mirrortools/icloud"
	_ "github.com/rpajarola/mirrortools/imap"
	_ "github.com/rpajarola/mirrortools/rsync"
)

func usage() {
	fmt.Fprintf(os.Stderr, "usage: %s <method> [flags] <source> <destdir>\n\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "methods:\n")
	for _, name := range mirror.Names() {
		m, _ := mirror.Get(name)
		fmt.Fprintf(os.Stderr, "  %-10s %s\n", m.Name, m.Describe)
		fmt.Fprintf(os.Stderr, "  %-10s source: %s\n", "", m.Source)
	}
	fmt.Fprintf(os.Stderr, "\nrun `%s <method> -h` for method-specific flags\n", os.Args[0])
}

func main() {
	flag.Usage = usage
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	name := os.Args[1]
	if name == "-h" || name == "-help" || name == "--help" {
		usage()
		return
	}

	m, ok := mirror.Get(name)
	if !ok {
		fmt.Fprintf(os.Stderr, "%s: unknown method %q\n\n", os.Args[0], name)
		usage()
		os.Exit(2)
	}

	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s %s [flags] <source> <destdir>\n\n", os.Args[0], name)
		fmt.Fprintf(os.Stderr, "source: %s\n\n", m.Source)
		fmt.Fprintf(os.Stderr, "flags:\n")
		fs.PrintDefaults()
	}
	mirrorFn := m.SetupFlags(fs)
	fs.Parse(os.Args[2:])

	args := fs.Args()
	if len(args) != 2 {
		fs.Usage()
		os.Exit(2)
	}
	source, destDir := args[0], args[1]

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "%s: creating %s: %v\n", os.Args[0], destDir, err)
		os.Exit(1)
	}

	if err := mirrorFn(context.Background(), source, destDir); err != nil {
		fmt.Fprintf(os.Stderr, "%s %s: %v\n", os.Args[0], name, err)
		os.Exit(1)
	}
}
