# ftpmirror

Recursively mirrors a directory tree (or a single file) of an FTP server to a
local directory, keeping the server's directory structure. Registers itself
as the `ftp` method of the [`mirror` CLI](../README.md). Built on
[`github.com/jlaffaye/ftp`](https://github.com/jlaffaye/ftp).

## Usage

```
go run ./cmd/mirror ftp [flags] <ftp:// or ftps:// URL> <destdir>
```

| Flag | Default | Meaning |
|---|---|---|
| `-user` | from the URL, else `anonymous` | Login user. |
| `-password-file` | from the URL, else `anonymous@` | File holding the login password (one line), so it stays out of the command line and shell history. |
| `-tls` | `false` | Upgrade a plain `ftp://` connection with `AUTH TLS` (explicit FTPS). An `ftps://` URL means implicit TLS on port 990. |
| `-update` | `false` | Re-list every directory, and re-download files whose listed size or mtime changed (or whose local copy is gone). Also retries files that failed permanently before. |

Files land at `<destdir>/<host>/<path>` (`<host>:<port>` for a non-default
port). Only the URL's directory and what's below it is mirrored. A URL naming
a file mirrors just that file. Empty directories are created too.

## Listings, timestamps and symlinks

Directories are listed with `MLSD` where the server supports it, which gives
exact UTC timestamps and includes hidden files. Otherwise `LIST` is used, and
its output is parsed in Unix `ls`, DOS and other common formats. `LIST` often
hides dotfiles and only gives timestamps to the minute or day, so each file's
exact mtime is then fetched with `MDTM` where the server supports it.

A symlink reported by `LIST` is recreated as a local symlink, not followed.
An absolute target is rewritten relative to the link, so it points into the
mirror rather than at the local filesystem's root. `MLSD` doesn't report
symlinks; servers list them as whatever they point to.

## Re-runs and resuming

Crawl state is kept in a SQLite database, `<destdir>/.ftpmirror.db`. Every
listed file and directory is recorded along with whether it's done, so an
interrupted run picks up where it stopped. By default a re-run doesn't touch
anything already mirrored, and doesn't even connect if nothing is left to
do.

Downloads go to a `*.ftpmirror-tmp` file that's renamed into place once
complete. If a download is interrupted, the temp file is kept, and the next
attempt resumes it with `REST`. It starts over if the server doesn't support
`REST`.

An entry that fails transiently (network error, timeout, 4xx reply) is
retried up to 3 times on a fresh connection. If it still fails, it's
recorded as an error, the run carries on, Mirror exits with an error at the
end, and the next run retries it. A 5xx reply (typically `550`, permission
denied) is logged and recorded as failed. It doesn't fail the run and is
only retried with `-update`. A failed connect or login aborts the run.

A server that stops responding is detected. Control-connection exchanges
time out after 2 minutes, and a transfer times out after 5 minutes without
receiving any data. There's no cap on how long a transfer that's still
making progress may take.
