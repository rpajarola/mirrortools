// Package ftpmirror recursively mirrors a directory tree of an FTP server
// to a local directory, keeping the server's directory structure.
//
// It registers itself as the "ftp" method with mirrortools' mirror
// package, so it's normally driven through the mirror CLI:
//
//	mirror ftp ftp://ftp.example.com/pub/ ./mirror
//
// Listings are read with MLSD where the server supports it (precise UTC
// timestamps, hidden files included) and LIST otherwise. Symbolic links
// that LIST reports are recreated as local symlinks rather than followed.
// Interrupted downloads resume from where they stopped (REST).
//
// Crawl state lives in a SQLite database in destDir, so an interrupted run
// resumes where it left off, and a re-run doesn't touch what's already been
// mirrored unless -update is given.
package ftpmirror

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"
	_ "modernc.org/sqlite"

	"github.com/rpajarola/mirrortools/mirror"
)

func init() {
	mirror.Register(&mirror.Method{
		Name:     "ftp",
		Source:   "an ftp:// (or implicit-TLS ftps://) URL of a directory or file, optionally with user[:password]@",
		Describe: "recursively mirror a directory tree of an FTP server",
		SetupFlags: func(fs *flag.FlagSet) mirror.Func {
			user := fs.String("user", "", "login user (default: from the URL, else anonymous)")
			passwordFile := fs.String("password-file", "", "path to a file containing the login password, one line")
			explicitTLS := fs.Bool("tls", false, "upgrade the connection with AUTH TLS (explicit FTPS)")
			update := fs.Bool("update", false, "re-list every directory and re-download files whose size or mtime changed")
			return func(ctx context.Context, source, destDir string) error {
				opts := Options{User: *user, ExplicitTLS: *explicitTLS, Update: *update}
				if *passwordFile != "" {
					pw, err := os.ReadFile(*passwordFile)
					if err != nil {
						return fmt.Errorf("reading -password-file: %w", err)
					}
					opts.Password = strings.TrimSpace(string(pw))
				}
				return Mirror(ctx, source, destDir, opts)
			}
		},
		Match: func(source string) bool {
			return strings.HasPrefix(source, "ftp://") || strings.HasPrefix(source, "ftps://")
		},
	})
}

// Options are Mirror's tunables, one per CLI flag.
type Options struct {
	// User and Password override any credentials in the URL. With
	// neither, the login is anonymous.
	User, Password string
	// ExplicitTLS upgrades a plain ftp:// connection with AUTH TLS. An
	// ftps:// URL means implicit TLS (TLS from the first byte, port 990).
	ExplicitTLS bool
	// Update re-lists every directory already handled by an earlier run,
	// queuing new files and files whose listed size or mtime changed (or
	// whose local copy is gone). Without it, a re-run only works on what
	// earlier runs didn't finish.
	Update bool
}

// dbFileName is the crawl state database, kept in destDir itself — above
// the per-host directories, so it can't collide with a mirrored file.
const dbFileName = ".ftpmirror.db"

// tmpSuffix marks a download in progress, the same write-then-rename
// convention used elsewhere in mirrortools. Unlike elsewhere, a leftover
// temp file is kept on failure: the next attempt resumes it with REST.
const tmpSuffix = ".ftpmirror-tmp"

const maxAttempts = 3

var (
	// initialBackoff is the delay before the first in-run retry of a
	// transiently failing entry, doubling after each attempt.
	initialBackoff = 5 * time.Second
	// cmdTimeout caps how long one control-connection exchange (login,
	// CWD, starting a transfer, ...) may take before the connection is
	// considered dead.
	cmdTimeout = 2 * time.Minute
	// idleTimeout caps how long a data transfer may go without receiving
	// anything; there's no cap on a transfer's total duration.
	idleTimeout = 5 * time.Minute
)

// Entry states in the database.
const (
	statePending = "pending" // not handled yet
	stateDone    = "done"    // directory listed, or file downloaded
	stateError   = "error"   // transient failure; retried on the next run
	stateFailed  = "failed"  // permanent failure (5xx reply); retried only with -update
)

const schema = `
CREATE TABLE IF NOT EXISTS entries (
	id     INTEGER PRIMARY KEY,
	host   TEXT NOT NULL,             -- host[:port], as in the local directory name
	path   TEXT NOT NULL,             -- absolute remote path
	is_dir INTEGER NOT NULL,
	size   INTEGER NOT NULL DEFAULT -1, -- as listed; -1 if unknown
	mtime  INTEGER NOT NULL DEFAULT 0,  -- as listed, Unix seconds; 0 if unknown
	state  TEXT NOT NULL DEFAULT 'pending',
	error  TEXT,
	UNIQUE(host, path)
);
CREATE INDEX IF NOT EXISTS entries_state ON entries(host, state, id);
`

// crawler holds the state of one Mirror run.
type crawler struct {
	db      *sql.DB
	opts    Options
	addr    string // host:port to dial
	host    string // host[:port] for the local directory and db rows
	tlsConf *tls.Config
	implTLS bool
	user    string
	pass    string
	hostDir string // destDir/host
	root    string // remote root path, cleaned
	conn    *ftp.ServerConn
	errors  int
	fetched int
}

// permanent reports whether err is an FTP reply with a 5xx code — a
// permanent failure that retrying won't fix.
func permanent(err error) bool {
	var te *textproto.Error
	return errors.As(err, &te) && te.Code >= 500
}

// fatalError aborts the whole run (bad credentials, unreachable server on
// the first connect).
type fatalError struct{ err error }

func (e *fatalError) Error() string { return e.err.Error() }
func (e *fatalError) Unwrap() error { return e.err }

// localError is a failure on our side (disk I/O, path collisions) that an
// immediate retry won't fix.
type localError struct{ err error }

func (e *localError) Error() string { return e.err.Error() }
func (e *localError) Unwrap() error { return e.err }

// Mirror recursively downloads the directory (or single file) source names
// into destDir/<host>/<path>. destDir must exist.
//
// It returns an error if any entry failed transiently (network errors, 4xx
// replies); those are retried on the next run. Entries that failed with a
// 5xx reply (typically 550, permission denied) are logged and recorded but
// don't fail the run.
func Mirror(ctx context.Context, source, destDir string, opts Options) error {
	u, err := url.Parse(source)
	if err != nil {
		return fmt.Errorf("parsing %q: %w", source, err)
	}
	c := &crawler{opts: opts}
	switch u.Scheme {
	case "ftp":
		if opts.ExplicitTLS {
			c.tlsConf = &tls.Config{ServerName: u.Hostname()}
		}
	case "ftps":
		c.tlsConf = &tls.Config{ServerName: u.Hostname()}
		c.implTLS = true
	default:
		return fmt.Errorf("not an ftp:// or ftps:// URL: %q", source)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("no host in %q", source)
	}
	defPort := "21"
	if c.implTLS {
		defPort = "990"
	}
	port := u.Port()
	if port == "" {
		port = defPort
	}
	c.addr = net.JoinHostPort(u.Hostname(), port)
	c.host = strings.ToLower(u.Hostname())
	if port != defPort {
		c.host += ":" + port
	}
	if c.host == "." || c.host == ".." || strings.ContainsAny(c.host, `/\`) {
		return fmt.Errorf("unusable host %q", u.Hostname())
	}
	c.hostDir = filepath.Join(destDir, c.host)
	c.root = path.Clean("/" + u.Path)

	c.user, c.pass = "anonymous", "anonymous@"
	if u.User != nil {
		c.user = u.User.Username()
		c.pass, _ = u.User.Password()
	}
	if opts.User != "" {
		c.user = opts.User
	}
	if opts.Password != "" {
		c.pass = opts.Password
	}

	c.db, err = openDB(filepath.Join(destDir, dbFileName))
	if err != nil {
		return err
	}
	defer c.db.Close()
	defer c.disconnect()

	if err := c.addRoot(ctx); err != nil {
		return err
	}
	log.Printf("ftp: mirroring ftp://%s%s into %s", c.host, c.root, destDir)

	reset := `UPDATE entries SET state = 'pending' WHERE host = ? AND state = 'error'`
	if opts.Update {
		reset = `UPDATE entries SET state = 'pending' WHERE host = ? AND (is_dir OR state != 'done')`
	}
	if _, err := c.db.ExecContext(ctx, reset, c.host); err != nil {
		return err
	}

	if err := c.run(ctx); err != nil {
		return err
	}
	log.Printf("ftp: %d files downloaded this run, %d errors", c.fetched, c.errors)
	if c.errors > 0 {
		return fmt.Errorf("%d entries failed, re-run to retry them", c.errors)
	}
	return nil
}

func openDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initializing %s: %w", dbPath, err)
	}
	return db, nil
}

// addRoot records the start path, finding out whether it's a directory or
// a file the first time it's seen. A re-run with nothing left to do never
// connects at all.
func (c *crawler) addRoot(ctx context.Context) error {
	var n int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entries WHERE host = ? AND path = ?`, c.host, c.root).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if err := c.connect(ctx); err != nil {
		return err
	}
	isDir := true
	size, mtime := int64(-1), time.Time{}
	err := c.guard(ctx, func() error { return c.conn.ChangeDir(c.root) })
	if err != nil {
		if !permanent(err) {
			return fmt.Errorf("ftp://%s%s: %w", c.host, c.root, err)
		}
		// Not a directory (or not one we may enter): try it as a file.
		isDir = false
		err = c.guard(ctx, func() error {
			var err error
			size, err = c.conn.FileSize(c.root)
			if err == nil && c.conn.IsGetTimeSupported() {
				mtime, _ = c.conn.GetTime(c.root)
			}
			return err
		})
		if err != nil {
			return fmt.Errorf("ftp://%s%s is neither a directory nor a file: %w", c.host, c.root, err)
		}
	}
	return c.upsert(ctx, c.root, isDir, size, mtime)
}

// connect (re)establishes the control connection if there isn't one.
// Failing to connect or log in is fatal to the run.
func (c *crawler) connect(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	opts := []ftp.DialOption{ftp.DialWithContext(ctx), ftp.DialWithTimeout(cmdTimeout)}
	if c.tlsConf != nil {
		if c.implTLS {
			opts = append(opts, ftp.DialWithTLS(c.tlsConf))
		} else {
			opts = append(opts, ftp.DialWithExplicitTLS(c.tlsConf))
		}
	}
	conn, err := ftp.Dial(c.addr, opts...)
	if err != nil {
		return &fatalError{fmt.Errorf("connecting to %s: %w", c.addr, err)}
	}
	c.conn = conn
	if err := c.guard(ctx, func() error { return conn.Login(c.user, c.pass) }); err != nil {
		c.disconnect()
		return &fatalError{fmt.Errorf("logging in to %s as %s: %w", c.addr, c.user, err)}
	}
	return nil
}

func (c *crawler) disconnect() {
	if c.conn != nil {
		c.conn.Quit()
		c.conn = nil
	}
}

// guard runs f, one or more control-connection exchanges, tearing the
// connection down if it takes longer than cmdTimeout or ctx is canceled —
// the FTP library has no deadlines of its own on the control connection,
// so a server that stops responding would otherwise hang the run forever.
func (c *crawler) guard(ctx context.Context, f func() error) error {
	conn := c.conn
	timer := time.AfterFunc(cmdTimeout, func() { conn.Quit() })
	stop := context.AfterFunc(ctx, func() { conn.Quit() })
	err := f()
	timer.Stop()
	stop()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// upsert records a listed entry. A new one is queued. A known one is only
// touched with -update, and then requeued if it's a file whose size or
// mtime changed or whose local copy is gone.
func (c *crawler) upsert(ctx context.Context, p string, isDir bool, size int64, mtime time.Time) error {
	var mt int64
	if !mtime.IsZero() {
		mt = mtime.Unix()
	}
	var (
		id             int64
		oldSize, oldMt int64
		oldIsDir       bool
		state          string
	)
	err := c.db.QueryRowContext(ctx, `SELECT id, is_dir, size, mtime, state FROM entries WHERE host = ? AND path = ?`,
		c.host, p).Scan(&id, &oldIsDir, &oldSize, &oldMt, &state)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = c.db.ExecContext(ctx, `INSERT INTO entries(host, path, is_dir, size, mtime) VALUES (?, ?, ?, ?, ?)`,
			c.host, p, isDir, size, mt)
		return err
	}
	if err != nil || !c.opts.Update {
		return err
	}
	changed := isDir != oldIsDir || size != oldSize || mt != oldMt
	if !isDir && !changed {
		if fi, err := os.Lstat(c.localPath(p)); err != nil || (size >= 0 && fi.Size() != size) {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	_, err = c.db.ExecContext(ctx, `UPDATE entries SET is_dir = ?, size = ?, mtime = ?, state = 'pending' WHERE id = ?`,
		isDir, size, mt, id)
	return err
}

// inScope reports whether remote path p is the root or below it.
func (c *crawler) inScope(p string) bool {
	return p == c.root || c.root == "/" || strings.HasPrefix(p, c.root+"/")
}

// localPath maps an absolute remote path to destDir/<host>/<path>.
// Cleaning the rooted path drops any ".." that would climb above it.
func (c *crawler) localPath(p string) string {
	return filepath.Join(c.hostDir, filepath.FromSlash(path.Clean("/"+p)))
}

// run works through pending entries in the order they were discovered
// until none are left; entries discovered meanwhile get higher ids, so the
// same loop picks them up. Pending entries outside the current root (left
// over from a run with a different start path into the same destDir) are
// left alone.
func (c *crawler) run(ctx context.Context) error {
	var lastID int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var (
			id          int64
			p           string
			isDir       bool
			size, mtime int64
		)
		err := c.db.QueryRowContext(ctx,
			`SELECT id, path, is_dir, size, mtime FROM entries WHERE host = ? AND state = 'pending' AND id > ? ORDER BY id LIMIT 1`,
			c.host, lastID).Scan(&id, &p, &isDir, &size, &mtime)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		lastID = id
		if !c.inScope(p) {
			continue
		}
		if err := c.process(ctx, id, p, isDir, size, mtime); err != nil {
			return err
		}
	}
}

// process handles one entry, retrying transient failures a bounded number
// of times (on a fresh connection each time) before recording it as an
// error and moving on. Only a canceled context, a fatal (login) error or a
// database failure is returned.
func (c *crawler) process(ctx context.Context, id int64, p string, isDir bool, size, mtime int64) error {
	backoff := initialBackoff
	for attempt := 1; ; attempt++ {
		err := c.connect(ctx)
		if err == nil {
			if isDir {
				err = c.listDir(ctx, p)
			} else {
				err = c.download(ctx, p, size, mtime)
			}
		}
		if err == nil {
			_, err := c.db.ExecContext(ctx, `UPDATE entries SET state = 'done', error = NULL WHERE id = ?`, id)
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var fe *fatalError
		if errors.As(err, &fe) {
			return err
		}
		if permanent(err) {
			log.Printf("ftp: %s: %v", p, err)
			return c.setState(ctx, id, stateFailed, err)
		}
		// Whatever went wrong, the control connection may be out of
		// step with the server now; start over on a fresh one.
		c.disconnect()
		var le *localError
		if errors.As(err, &le) || attempt == maxAttempts {
			log.Printf("ftp: giving up on %s: %v", p, err)
			c.errors++
			return c.setState(ctx, id, stateError, err)
		}
		log.Printf("ftp: %s: %v, retrying in %s", p, err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff *= 2
	}
}

func (c *crawler) setState(ctx context.Context, id int64, state string, err error) error {
	_, dbErr := c.db.ExecContext(ctx, `UPDATE entries SET state = ?, error = ? WHERE id = ?`, state, err.Error(), id)
	return dbErr
}

// listDir lists remote directory dir, recording its files and
// subdirectories and recreating its symlinks locally.
func (c *crawler) listDir(ctx context.Context, dir string) error {
	log.Printf("ftp: list %s", dir)
	var entries []*ftp.Entry
	// CWD, then list the current directory: more robust than passing the
	// path to MLSD/LIST, which some servers mangle when it contains
	// spaces or glob characters.
	err := c.guard(ctx, func() error {
		if err := c.conn.ChangeDir(dir); err != nil {
			return err
		}
		var err error
		entries, err = c.conn.List("")
		return err
	})
	if err != nil {
		return err
	}

	local := c.localPath(dir)
	if err := os.MkdirAll(local, 0o755); err != nil {
		return &localError{err}
	}
	for _, e := range entries {
		// MLSD's cdir/pdir entries come back as folders named ".", ".."
		// or the directory's full path.
		if e.Name == "" || e.Name == "." || e.Name == ".." || strings.Contains(e.Name, "/") {
			continue
		}
		child := path.Join(dir, e.Name)
		switch e.Type {
		case ftp.EntryTypeFolder:
			if err := c.upsert(ctx, child, true, -1, time.Time{}); err != nil {
				return err
			}
		case ftp.EntryTypeFile:
			if err := c.upsert(ctx, child, false, int64(e.Size), e.Time); err != nil {
				return err
			}
		case ftp.EntryTypeLink:
			c.symlink(child, e.Target)
		}
	}
	return nil
}

// symlink recreates remote symlink p -> target locally. An absolute target
// is rewritten relative to the link so it points into the mirror instead
// of at the local filesystem's root. Failures are only logged: a symlink
// isn't content, and the mirror is still useful without it.
func (c *crawler) symlink(p, target string) {
	if target == "" {
		return
	}
	local := c.localPath(p)
	if _, err := os.Lstat(local); err == nil {
		return
	}
	t := filepath.FromSlash(target)
	if path.IsAbs(target) {
		rel, err := filepath.Rel(filepath.Dir(local), c.localPath(target))
		if err != nil {
			log.Printf("ftp: %s: can't relativize symlink target %q: %v", p, target, err)
			return
		}
		t = rel
	}
	if err := os.Symlink(t, local); err != nil {
		log.Printf("ftp: %s: creating symlink: %v", p, err)
	}
}

// download fetches remote file p into its local path, resuming a temp file
// left behind by an earlier attempt.
func (c *crawler) download(ctx context.Context, p string, size, mtime int64) error {
	local := c.localPath(p)
	if strings.HasSuffix(local, tmpSuffix) {
		return &localError{fmt.Errorf("file name collides with temp file naming")}
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return &localError{err}
	}
	tmp := local + tmpSuffix

	var offset int64
	if fi, err := os.Stat(tmp); err == nil && (size < 0 || fi.Size() < size) {
		offset = fi.Size()
	}
	if offset > 0 {
		log.Printf("ftp: get %s (resuming at %d of %d bytes)", p, offset, size)
	} else {
		log.Printf("ftp: get %s (%d bytes)", p, size)
	}

	var resp *ftp.Response
	err := c.guard(ctx, func() error {
		var err error
		resp, err = c.conn.RetrFrom(p, uint64(offset))
		if err != nil && offset > 0 && permanent(err) {
			// Most likely REST isn't supported; start over.
			offset = 0
			resp, err = c.conn.RetrFrom(p, 0)
		}
		return err
	})
	if err != nil {
		return err
	}

	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if offset > 0 {
		flags = os.O_WRONLY | os.O_APPEND
	}
	out, err := os.OpenFile(tmp, flags, 0o644)
	if err != nil {
		resp.Close()
		return &localError{err}
	}
	stop := context.AfterFunc(ctx, func() { resp.SetDeadline(time.Unix(1, 0)) })
	n, copyErr := io.Copy(out, &idleReader{ctx, resp})
	stop()
	closeErr := out.Close()
	// Close reads the server's end-of-transfer reply on the control
	// connection.
	respErr := c.guard(ctx, resp.Close)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case copyErr != nil:
		return copyErr
	case closeErr != nil:
		return &localError{closeErr}
	case respErr != nil:
		return respErr
	}
	if got := offset + n; size >= 0 && got < size {
		return fmt.Errorf("short transfer: got %d of %d bytes", got, size)
	}

	if err := os.Rename(tmp, local); err != nil {
		return &localError{err}
	}
	// MLSD timestamps are exact; LIST ones are often only to the minute
	// or day, so ask MDTM for the precise one where it's available.
	var t time.Time
	if mtime != 0 {
		t = time.Unix(mtime, 0)
	}
	if !c.conn.IsTimePreciseInList() && c.conn.IsGetTimeSupported() {
		c.guard(ctx, func() error {
			if mt, err := c.conn.GetTime(p); err == nil {
				t = mt
			}
			return nil
		})
	}
	if !t.IsZero() {
		os.Chtimes(local, t, t)
	}
	c.fetched++
	return nil
}

// idleReader fails a data transfer that receives nothing for idleTimeout.
type idleReader struct {
	ctx context.Context
	r   *ftp.Response
}

func (ir *idleReader) Read(b []byte) (int, error) {
	if err := ir.ctx.Err(); err != nil {
		return 0, err
	}
	ir.r.SetDeadline(time.Now().Add(idleTimeout))
	return ir.r.Read(b)
}
