// Command mirrorq enqueues URLs in the mirrord background daemon queue.
//
// Usage:
//
//	mirrorq [flags] [url ...]
//	mirrorq https://example.com/item1 https://example.com/item2
//	cat urls.txt | mirrorq
//	mirrorq < urls.txt
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	iconfig "github.com/rpajarola/mirrortools/internal/config"
	"github.com/rpajarola/mirrortools/internal/daemon"
	"github.com/rpajarola/mirrortools/internal/version"
)

func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, "usage: mirrorq [flags] [url ...]\n\n")
	fmt.Fprintf(w, "Enqueue URLs to be mirrored by the mirrord background daemon.\n")
	fmt.Fprintf(w, "URLs can be given as arguments, read from stdin (one per line) when\n")
	fmt.Fprintf(w, "redirected or piped, or both.\n\n")
	fmt.Fprintf(w, "flags:\n")
	fmt.Fprintf(w, "  -c, -config <path>   path to config.toml\n")
	fmt.Fprintf(w, "  -s, -socket <path>   path to mirrord unix domain socket\n")
	fmt.Fprintf(w, "  -d, -dest <dir>      override destination directory\n")
	fmt.Fprintf(w, "  -q, -quiet           suppress job enqueued confirmations\n")
	fmt.Fprintf(w, "  -v, -version         print mirrorq version and exit\n")
}

func isConnectRefused(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connect: connection refused") ||
		strings.Contains(msg, "no such file or directory")
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, isStdinTerminal bool) int {
	fs := flag.NewFlagSet("mirrorq", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // handled below
	fs.Usage = func() {}

	var configPath string
	fs.StringVar(&configPath, "config", "", "path to config.toml")
	fs.StringVar(&configPath, "c", "", "path to config.toml (shorthand)")

	var socketPath string
	fs.StringVar(&socketPath, "socket", "", "path to mirrord unix domain socket")
	fs.StringVar(&socketPath, "s", "", "path to mirrord unix domain socket (shorthand)")

	var destDir string
	fs.StringVar(&destDir, "dest", "", "override destination directory")
	fs.StringVar(&destDir, "d", "", "override destination directory (shorthand)")

	var quiet bool
	fs.BoolVar(&quiet, "quiet", false, "suppress job enqueued confirmations")
	fs.BoolVar(&quiet, "q", false, "suppress job enqueued confirmations (shorthand)")

	var showVersion bool
	fs.BoolVar(&showVersion, "version", false, "print mirrorq version and exit")
	fs.BoolVar(&showVersion, "v", false, "print mirrorq version and exit (shorthand)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stdout)
			return 0
		}
		fmt.Fprintf(stderr, "mirrorq: %v\n\n", err)
		printUsage(stderr)
		return 2
	}

	if showVersion {
		fmt.Fprintf(stdout, "mirrorq %s\n", version.String())
		return 0
	}

	rest := fs.Args()
	if len(rest) == 1 && (rest[0] == "version") {
		fmt.Fprintf(stdout, "mirrorq %s\n", version.String())
		return 0
	}
	if len(rest) == 1 && (rest[0] == "help") {
		printUsage(stdout)
		return 0
	}

	var urls []string
	readStdin := false

	for _, arg := range rest {
		if arg == "-" {
			readStdin = true
			continue
		}
		arg = strings.TrimSpace(arg)
		if arg != "" {
			urls = append(urls, arg)
		}
	}

	if (readStdin || !isStdinTerminal) && stdin != nil {
		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			urls = append(urls, line)
		}
		if err := scanner.Err(); err != nil {
			fmt.Fprintf(stderr, "mirrorq: reading stdin: %v\n", err)
			return 1
		}
	}

	if len(urls) == 0 {
		printUsage(stderr)
		return 2
	}

	cfgFile := configPath
	if cfgFile == "" {
		cfgFile = iconfig.Path()
	}
	cfg, err := iconfig.Load(cfgFile)
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "mirrorq: config error: %v\n", err)
		return 2
	}
	if cfg == nil {
		cfg = &iconfig.Config{}
	}

	sock := socketPath
	if sock == "" {
		if s := os.Getenv("MIRRORD_SOCKET"); s != "" {
			sock = s
		} else if cfg.Daemon.Socket != "" {
			sock = cfg.Daemon.Socket
		} else {
			sock = iconfig.DefaultSocketPath()
		}
	}

	client := daemon.NewUnixClient(sock)
	ctx := context.Background()

	hasError := false
	for _, u := range urls {
		sub := &daemon.SubmitRequest{
			Source:  u,
			DestDir: destDir,
		}
		job, err := client.SubmitJob(ctx, sub)
		if err != nil {
			if isConnectRefused(err) {
				fmt.Fprintf(stderr, "mirrorq: cannot connect to mirrord at %s\n", sock)
				fmt.Fprintf(stderr, "is the daemon running? Start it with `mirrord` or `systemctl --user start mirrord`\n")
				return 1
			}
			fmt.Fprintf(stderr, "mirrorq: %s: %v\n", u, err)
			hasError = true
			continue
		}
		if !quiet {
			fmt.Fprintf(stdout, "Enqueued job #%d (%s: %s into %s)\n", job.ID, job.Method, job.Source, job.DestDir)
		}
	}

	if hasError {
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, isTerminal(os.Stdin)))
}
