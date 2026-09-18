# mirrortools
Tools for mirroring internet accounts/services to local disk.

## Layout

- `mirror/` — the top-level library: the `Method`/`Func` interface every
  backend implements, and the registry the CLI uses to find them.
- `cmd/mirror/` — the `mirror` CLI, a thin driver over the registry.
- `gphotos/` — the Google Photos backend (registers itself as `gphotos`).
  See [`gphotos/README.md`](gphotos/README.md) for how to get cookies for
  it, including the Chrome DBSC quirk that can make them stop working
  within minutes.
- `archiveorg/` — the Internet Archive backend (registers itself as
  `archiveorg`). See [`archiveorg/README.md`](archiveorg/README.md).
- `gopher/` — the Gopher protocol backend (registers itself as `gopher`).
  See [`gopher/README.md`](gopher/README.md).
- `groupsio/` — the groups.io backend (registers itself as `groupsio`). See
  [`groupsio/README.md`](groupsio/README.md).

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

Or to mirror a groups.io group's Files section:

```
go run ./cmd/mirror groupsio -email you@example.com -password-file pw.txt some-group ./mirror
```

Run `go run ./cmd/mirror` for the list of available methods, or
`go run ./cmd/mirror <method> -h` for a method's flags.
