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

All of them install the `mirror` command and its manual page,
[mirror(1)](https://rpajarola.github.io/apt/mirrortools/mirror.1.html). [packaging](packaging/README.md)
describes what is installed and how the packages are built.

Or build it with Go: `go install github.com/rpajarola/mirrortools/cmd/mirror@latest`

## Layout

- `mirror/` — the top-level library: the `Method`/`Func` interface every
  backend implements, and the registry the CLI uses to find them.
- `cmd/mirror/` — the `mirror` CLI, a thin driver over the registry.
- `man/` — the `mirror(1)` manual page.
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
- `gopher/` — the Gopher protocol backend (registers itself as `gopher`).
  See [`gopher/README.md`](gopher/README.md).
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
`https://groups.io/g/...` to `groupsio`, `ftp://` and `ftps://` to `ftp`,
`gopher://` to `gopher`, `rsync://` and `host::module` to `rsync`, and any
other `http://` or `https://` URL to `http`. The destination can be left
out when the config file names a default one.

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

Or to mirror a groups.io group's Files section:

```
mirror -email you@example.com -password-file pw.txt https://groups.io/g/some-group ./mirror
```

Run `mirror -h` for the list of available methods, or `mirror <method> -h`
for a method's flags.

### Config file

`~/.config/mirrortools/config.toml` (or `$XDG_CONFIG_HOME/mirrortools/config.toml`,
or whatever `$MIRROR_CONFIG` names) sets a default destination, and
defaults for each method's flags, in [TOML](https://toml.io):

```toml
# Where to mirror when no <destdir> is given.
destdir = "~/mirror"

# A section per method: default values for its flags (without the "-"),
# and optionally a destdir of its own.
[groupsio]
email = "you@example.com"
password-file = "~/.config/mirrortools/groupsio-password"

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
