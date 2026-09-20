// Package rsync mirrors a remote rsync source into a local directory using
// gokrazy/rsync's pure-Go rsync client (github.com/gokrazy/rsync/rsyncclient)
// — no dependency on an external `rsync` binary for the transfer itself.
// Every run is effectively `rsync -a`: recursive, preserving symlinks,
// permissions, times, and (where possible) ownership — archive mode is
// always on, and the rsync protocol's own delta-transfer already makes a
// second run against the same destDir an incremental update, so there's
// nothing extra to configure for that. The one optional flag, -delete,
// mirrors rsync's --delete for removing local files that no longer exist on
// the remote side.
//
// It registers itself as the "rsync" method with mirrortools' mirror
// package, so it's normally driven through the mirror CLI:
//
//	mirror rsync rsync://host/module/path ./mirror
//	mirror rsync host::module/path ./mirror
//	mirror rsync -delete user@host:/remote/path/ ./mirror
//
// Three source forms are accepted, exactly as real rsync accepts them:
//
//   - rsync://[user@]host[:port]/module[/path] and [user@]host::module[/path]
//     both address an rsync daemon module. The connection is a plain TCP
//     dial (default port 873) with no external process involved. The
//     daemon protocol's own authentication isn't implemented (the
//     underlying library doesn't support it yet), so this only works
//     against anonymous/public modules.
//   - [user@]host:path addresses a plain remote path fetched over a remote
//     shell, almost always ssh. As real rsync does, the external `ssh`
//     binary (or $RSYNC_RSH, if set) is spawned to run `rsync --server
//     --sender ...` on the remote host, and its stdin/stdout become the
//     rsync protocol connection; ssh authentication (keys, agent, known
//     hosts) is whatever that binary and the local ssh config already do.
//
// As with real rsync, a trailing slash on the source path is significant:
// it copies the directory's contents into destDir, while omitting it copies
// the directory itself into destDir. Unlike mirrortools' other backends,
// Mirror does not create its own named subdirectory under destDir — destDir
// is the rsync destination directly, matching ordinary rsync behavior and
// letting the trailing-slash convention above do its usual job.
package rsync

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gokrazy/rsync/rsyncclient"
	"github.com/rpajarola/mirrortools/mirror"
)

func init() {
	mirror.Register(&mirror.Method{
		Name: "rsync",
		Source: "an rsync source: rsync://[user@]host[:port]/module[/path], " +
			"[user@]host::module[/path] (rsync daemon), or [user@]host:path (over ssh)",
		Describe: "mirror a remote rsync source into destDir (archive mode, i.e. `rsync -a`)",
		SetupFlags: func(fs *flag.FlagSet) mirror.Func {
			delete := fs.Bool("delete", false, "delete local files that no longer exist on the remote side")
			return func(ctx context.Context, source, destDir string) error {
				return Mirror(ctx, source, destDir, *delete)
			}
		},
	})
}

const dialTimeout = 30 * time.Second

// defaultDaemonPort is the standard rsync daemon TCP port, used whenever a
// source doesn't specify one explicitly.
const defaultDaemonPort = "873"

// rsyncSource is one parsed rsync source, in whichever of the three forms
// described in the package doc comment it came in as.
type rsyncSource struct {
	daemon bool // rsync daemon (dialed directly), vs. a remote shell
	user   string
	host   string
	port   string // daemon only
	path   string // daemon: "module" or "module/sub/dir"; shell: the remote path
}

// parseSource accepts the same three source forms real rsync does — see the
// package doc comment. It does not itself validate host/path contents
// beyond basic shape: anything rsync (or ssh) would reject, this rejects
// too, just later and with a less specific error.
func parseSource(src string) (*rsyncSource, error) {
	if rest, ok := strings.CutPrefix(src, "rsync://"); ok {
		hostport, path, _ := strings.Cut(rest, "/")
		user, hostport := splitUser(hostport)
		host, port := splitHostPort(hostport)
		if port == "" {
			port = defaultDaemonPort
		}
		return &rsyncSource{daemon: true, user: user, host: host, port: port, path: path}, nil
	}

	idx := strings.IndexByte(src, ':')
	if idx < 0 {
		return nil, fmt.Errorf("not an rsync source (want rsync://host/module/path, "+
			"host::module/path, or [user@]host:path): %q", src)
	}
	user, host := splitUser(src[:idx])
	rest := src[idx+1:]
	if after, ok := strings.CutPrefix(rest, ":"); ok {
		return &rsyncSource{daemon: true, user: user, host: host, port: defaultDaemonPort, path: after}, nil
	}
	return &rsyncSource{user: user, host: host, path: rest}, nil
}

func splitUser(s string) (user, host string) {
	if at := strings.LastIndexByte(s, '@'); at >= 0 {
		return s[:at], s[at+1:]
	}
	return "", s
}

func splitHostPort(s string) (host, port string) {
	if h, p, ok := strings.Cut(s, ":"); ok {
		return h, p
	}
	return s, ""
}

// Mirror downloads every file of an rsync source into destDir (which the
// caller guarantees exists) in archive mode, optionally deleting local
// files absent from the remote side. See the package doc comment for the
// accepted source forms and how a trailing slash on the path affects the
// result.
func Mirror(ctx context.Context, source, destDir string, delete bool) error {
	src, err := parseSource(source)
	if err != nil {
		return err
	}

	args := []string{"-a"}
	if delete {
		args = append(args, "--delete")
	}
	client, err := rsyncclient.New(args)
	if err != nil {
		return fmt.Errorf("rsync: %w", err)
	}

	log.Printf("rsync: mirroring %s into %s", source, destDir)

	var stats string
	if src.daemon {
		conn, err := dialDaemon(ctx, src)
		if err != nil {
			return err
		}
		defer conn.Close()
		result, err := client.RunDaemon(ctx, conn, src.path, []string{destDir})
		if err != nil {
			return fmt.Errorf("rsync: %w", err)
		}
		stats = statsString(result)
	} else {
		conn, err := dialShell(ctx, src, client.ServerCommandOptions(src.path))
		if err != nil {
			return err
		}
		defer conn.Close()
		result, err := client.Run(ctx, conn, []string{destDir})
		if err != nil {
			return fmt.Errorf("rsync: %w", err)
		}
		stats = statsString(result)
	}

	log.Printf("rsync: done: %s", stats)
	return nil
}

func statsString(result *rsyncclient.Result) string {
	if result == nil || result.Stats == nil {
		return "no stats reported"
	}
	return fmt.Sprintf("%d bytes of files, %d read / %d written on the wire",
		result.Stats.Size, result.Stats.Read, result.Stats.Written)
}

// dialDaemon connects directly to an rsync daemon over TCP — no external
// process, unlike dialShell.
func dialDaemon(ctx context.Context, src *rsyncSource) (io.ReadWriteCloser, error) {
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(src.host, src.port))
	if err != nil {
		return nil, fmt.Errorf("rsync: dialing daemon at %s: %w", src.host, err)
	}
	return conn, nil
}

// shellConn adapts a spawned remote-shell process's stdin/stdout pipes into
// a single io.ReadWriteCloser, the same connection abstraction dialDaemon
// returns for the daemon case. Closing it closes both pipes and waits for
// the process to exit, surfacing a non-zero exit (e.g. ssh itself failing)
// as an error.
type shellConn struct {
	stdout io.ReadCloser
	stdin  io.WriteCloser
	cmd    *exec.Cmd
}

func (c *shellConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *shellConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

func (c *shellConn) Close() error {
	c.stdin.Close()
	c.stdout.Close()
	return c.cmd.Wait()
}

// dialShell spawns a remote shell (ssh by default, or $RSYNC_RSH — the same
// override real rsync honors) to run `rsync --server --sender <serverArgs>`
// on src.host, the same way real rsync's own ssh transport works. serverArgs
// is exactly what a real rsync client would pass, from
// Client.ServerCommandOptions.
func dialShell(ctx context.Context, src *rsyncSource, serverArgs []string) (io.ReadWriteCloser, error) {
	shellArgs := strings.Fields(rshCommand())
	if len(shellArgs) == 0 {
		return nil, fmt.Errorf("rsync: empty $RSYNC_RSH")
	}

	args := shellArgs[1:]
	if src.user != "" {
		args = append(args, "-l", src.user)
	}
	args = append(args, src.host, "rsync")
	args = append(args, serverArgs...)

	cmd := exec.CommandContext(ctx, shellArgs[0], args...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("rsync: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("rsync: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("rsync: starting %s: %w", shellArgs[0], err)
	}

	return &shellConn{stdout: stdout, stdin: stdin, cmd: cmd}, nil
}

// rshCommand returns the remote shell command to spawn for an ssh-style
// source: $RSYNC_RSH if set — the same override real rsync honors — else
// "ssh".
func rshCommand() string {
	if e := os.Getenv("RSYNC_RSH"); e != "" {
		return e
	}
	return "ssh"
}
