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
go run ./cmd/mirror <method> [flags] <source> <destdir>
```

For example, to mirror a Google Photos account:

```
go run ./cmd/mirror gphotos -cookies cookies.txt someone@gmail.com ./photos
```

Or to mirror an Internet Archive item (this lands in `./archive/some-item`,
since archiveorg mirrors each item into its own `<destdir>/<id>`
subdirectory):

```
go run ./cmd/mirror archiveorg https://archive.org/details/some-item ./archive
```

Or to mirror a Gopher menu tree:

```
go run ./cmd/mirror gopher gopher://gopher.floodgap.com/1/gopher/toybox ./mirror
```

Or to mirror everything under a path of a web server:

```
go run ./cmd/mirror http https://ftp.gnu.org/gnu/hello/ ./mirror
```

Or to mirror a directory tree of an FTP server:

```
go run ./cmd/mirror ftp ftp://ftp.gnu.org/gnu/hello/ ./mirror
```

Or to mirror a groups.io group's Files section:

```
go run ./cmd/mirror groupsio -email you@example.com -password-file pw.txt some-group ./mirror
```

Run `go run ./cmd/mirror` for the list of available methods, or
`go run ./cmd/mirror <method> -h` for a method's flags.
