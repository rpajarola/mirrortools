# mirrortools
Tools for mirroring internet accounts/services to local disk.

## Install

Packages are attached to each [release](https://github.com/rpajarola/mirrortools/releases),
and available from:

- **Debian and Ubuntu** (amd64, arm64): the [apt repository](https://rpajarola.github.io/apt/),
  then `sudo apt install mirrortools`
- **macOS**: the [Homebrew tap](https://github.com/rpajarola/homebrew-tap),
  `brew install rpajarola/tap/mirrortools`, or the installer package from a
  release, which needs no Homebrew

All of them install the `mirror` command, the `mirrord` daemon service,
the `mirrorq` enqueue utility, and their manual pages,
[mirror(1)](https://rpajarola.github.io/apt/mirrortools/mirror.1.html),
[mirrord(1)](https://rpajarola.github.io/apt/mirrortools/mirrord.1.html), and
[mirrorq(1)](https://rpajarola.github.io/apt/mirrortools/mirrorq.1.html).
[packaging](packaging/README.md)
describes what is installed and how the packages are built.

Or build it with Go: `go install github.com/rpajarola/mirrortools/cmd/mirror@latest`

## Layout

- `mirror/` — the top-level library: the `Method`/`Func` interface every
  backend implements, and the registry the CLI uses to find them.
- `cmd/mirror/` — the `mirror` CLI, a thin driver over the registry and daemon queue client.
- `cmd/mirrord/` — the `mirrord` daemon service (queue, workers, Web UI).
- `cmd/mirrorq/` — the `mirrorq` utility for batch enqueuing URLs via arguments or stdin.
- `man/` — the `mirror(1)`, `mirrord(1)` and `mirrorq(1)` manual pages.
- `packaging/` — `.deb`, macOS `.pkg` and Homebrew packaging, built by
  the release workflow. See [`packaging/README.md`](packaging/README.md).
- `gphotos/` — the Google Photos backend (registers itself as `gphotos`).
  See [`gphotos/README.md`](gphotos/README.md) for how to get cookies for
  it, including the Chrome DBSC quirk that can make them stop working
  within minutes.
- `archiveorg/` — the Internet Archive backend (registers itself as
  `archiveorg`). See [`archiveorg/README.md`](archiveorg/README.md).
- `ftpmirror/` — the FTP backend (registers itself as `ftp`). See
  [`ftpmirror/README.md`](ftpmirror/README.md).
- `git/` — the Git repository backend (registers itself as `git`). See
  [`git/README.md`](git/README.md).
- `gopher/` — the Gopher protocol backend (registers itself as `gopher`).
  See [`gopher/README.md`](gopher/README.md).
- `sourceforge/` — the SourceForge backend (registers itself as
  `sourceforge`). See [`sourceforge/README.md`](sourceforge/README.md).
- `httpmirror/` — the generic HTTP(S) backend (registers itself as `http`).
  See [`httpmirror/README.md`](httpmirror/README.md).
- `groupsio/` — the groups.io backend (registers itself as `groupsio`). See
  [`groupsio/README.md`](groupsio/README.md).
- `icloud/` — the iCloud Photos backend (registers itself as `icloud`).
  See [`icloud/README.md`](icloud/README.md) for the sign-in/2FA flow.
  **Not yet tested against a real account** — see that README's first
  section before relying on it.

Each backend mirrors a `source` (a URL, account email, or other identifier —
whatever uniquely names the thing for that method) into a destination
directory, plus whatever method-specific flags it needs (e.g. `-cookies`).

Adding a new backend means writing a package that registers a `mirror.Method`
in its `init()`, then blank-importing it from `cmd/mirror/main.go`.

## Usage

```
mirror [<method>] [flags] <source> [<destdir>]
```

(or `go run ./cmd/mirror ...` from a checkout).

The method can be left out when the source says what it is:
`https://archive.org/details/...` goes to `archiveorg`,
`https://groups.io/g/...` to `groupsio`,
`https://sourceforge.net/projects/...` to `sourceforge`, `ftp://` and `ftps://` to `ftp`,
`gopher://` to `gopher`, `rsync://` and `host::module` to `rsync`, Git
repository URLs (`git://`, `ssh://`, `git@host:path`, `*.git`, and
repositories served over `http://` or `https://`) to `git`, and any other
`http://` or `https://` URL to `http`. The destination can be left out when
the config file names a default one.

For example, to mirror a Google Photos account:

```
mirror gphotos -cookies cookies.txt someone@gmail.com ./photos
```

Or to mirror an Internet Archive item (this lands in `./archive/some-item`,
since archiveorg mirrors each item into its own `<destdir>/<id>`
subdirectory):

```
mirror https://archive.org/details/some-item ./archive
```

Or to mirror a Gopher menu tree:

```
mirror gopher://gopher.floodgap.com/1/gopher/toybox ./mirror
```

Or to mirror everything under a path of a web server:

```
mirror https://ftp.gnu.org/gnu/hello/ ./mirror
```

Or to mirror a directory tree of an FTP server:

```
mirror ftp://ftp.gnu.org/gnu/hello/ ./mirror
```

Or to mirror every file in a SourceForge project's files area (this lands
in `./mirror/fsd-amoeba`):

```
mirror https://sourceforge.net/projects/fsd-amoeba/ ./mirror
```

Or to mirror a groups.io group's Files section:

```
mirror -email you@example.com -password-file pw.txt https://groups.io/g/some-group ./mirror
```

Run `mirror -h` for the list of available methods, or `mirror <method> -h`
for a method's flags.

## Daemon & Queue Service (`mirrord`)

`mirrord` runs as a persistent service managing a job queue in SQLite,
executing mirror tasks with worker concurrency, isolating failing jobs in
quarantine, and serving a local Unix domain socket and Web UI.

Start the daemon:

```bash
mirrord -http 127.0.0.1:8080 -password "your-admin-password"
# or via systemd: systemctl --user start mirrord
```

Open `http://127.0.0.1:8080` in your browser for the real-time Web dashboard,
site/subtree hierarchy view, queue controls, live log streaming, and in-browser config editing.
The dashboard supports toggling between an interactive **Site & Subtree** tree view and a **Flat List** view,
with one-click pause, resume, quarantine, unquarantine, and cancellation of individual URLs, entire sites, or subtrees.
When password or OAuth authentication is configured, Web UI sessions use HTTP-only
cookies and remote REST API clients can pass `Authorization: Bearer <password>`.
Local CLI commands via Unix socket (`mirror queue`) bypass HTTP authentication automatically.

### Fair-Share Dispatch & Scheduling

When multiple sites are queued simultaneously:
- **Per-host fair share**: Concurrent worker slots prioritize idle sites so that different domains execute in parallel rather than jumping back-and-forth or starving other hosts.
- **Hierarchical tree order**: Within each site, queued URLs execute systematically from the root and parent directories downward into subdirectories.
- **Prefix operations**: Pause, resume, quarantine, unquarantine, or cancel can target single jobs by ID or entire subtrees by URL/site prefix (e.g. `example.com` or `https://example.com/docs/`).

### Queue CLI

Manage the daemon's queue from the command line using `mirror queue` (or the
shorthand `mirror submit`):

```bash
# Submit jobs to the background queue (method and destination auto-detected)
mirror queue submit https://sourceforge.net/projects/fsd-amoeba/
mirror submit -wait 1s https://archive.org/details/nasa-audio

# Inspect the queue (filterable by state, site, or prefix)
mirror queue list
mirror queue list -site example.com
mirror queue list -prefix https://example.com/docs/
mirror queue status 1
mirror queue logs -f 1           # stream live output

# Queue dispatcher control
mirror queue pause               # pause queue dispatcher
mirror queue resume              # resume queue processing
mirror queue concurrency 4       # adjust worker limit

# Job, site, and subtree control
mirror queue pause 1             # pause individual job #1
mirror queue resume 1            # resume individual job #1
mirror queue pause example.com/docs/   # pause all queued jobs under subtree
mirror queue resume example.com/docs/  # resume all paused jobs under subtree
mirror queue quarantine 1        # quarantine individual job #1
mirror queue quarantine example.com    # quarantine all jobs for entire site
mirror queue unquarantine example.com  # return site to queue
mirror queue cancel 1            # cancel job #1
mirror queue cancel example.com/docs/  # cancel all jobs under subtree
mirror queue retry 1             # retry a failed/quarantined job

# Inspect per-site transfers (retrieved, failed, future, disabled)
mirror queue transfers example.com
mirror queue transfers -category failed example.com
mirror queue transfers -search icon example.com

# Permanently disable URLs or defunct third-party hosts so site reaches done
mirror queue disable example.com https://deadhost.com/icon.png
mirror queue disable example.com deadhost.com    # adds ignore pattern for deadhost
mirror queue ignores example.com                 # list active ignore patterns
mirror queue ignore example.com "*.deadhost.com" # add pattern
mirror queue unignore example.com deadhost.com   # remove pattern
mirror queue enable example.com https://deadhost.com/icon.png
```

### Batch Enqueuing (`mirrorq`)

`mirrorq` enqueues URLs directly into the background queue, accepting URLs either as arguments or line-by-line from standard input:

```bash
# Pass URLs as command-line arguments
mirrorq https://sourceforge.net/projects/fsd-amoeba/ https://archive.org/details/nasa-audio

# Pipe a list of URLs (one per line, comments starting with # and blank lines ignored)
cat urls.txt | mirrorq

# Redirect directly
mirrorq < urls.txt

# Override destination directory or suppress confirmation messages
mirrorq -d ~/downloads/archive -q < urls.txt
```

### Config file

`~/.config/mirrortools/config.toml` (or `$XDG_CONFIG_HOME/mirrortools/config.toml`,
or whatever `$MIRROR_CONFIG` names) sets a default destination, and
defaults for each method's flags, in [TOML](https://toml.io):

```toml
# Where to mirror when no <destdir> is given.
destdir = "~/mirror"

# Daemon service settings
[daemon]
http = "127.0.0.1:8080"
concurrency = 3

# Optional Web UI & HTTP API authentication
[daemon.auth]
password-file = "~/.config/mirrortools/daemon-password"

# Optional OAuth 2.0 / OpenID Connect SSO
[daemon.auth.oauth]
provider = "google"   # or "github", "generic"
client-id = "your-client-id.apps.googleusercontent.com"
client-secret-file = "~/.config/mirrortools/oauth-secret"
allowed-users = ["you@example.com"]

# A section per method: default values for its flags (without the "-"),
# and optionally a destdir of its own.
[groupsio]
email = "you@example.com"
password-file = "~/.config/mirrortools/groupsio-password"

[git]
destdir = "~/git"

[http]
wait = "1s"

[gphotos]
destdir = "~/Pictures/Google Photos"
cookies = "~/.config/mirrortools/gphotos-cookies.txt"
```

Flags on the command line override the config file. A leading `~/` in a
value is the home directory.

Most methods already put each source in a subdirectory of its own, such as
`<destdir>/<host>/<path>` or `<destdir>/<item>`, so they can all share the
default destination. The others get one when they use it: `gphotos/<account>`,
`icloud/<apple id>`, `imap/<host>`, and `<host>/<path>` for `rsync`. A
method's own `destdir`, or one given on the command line, is used as it is.
