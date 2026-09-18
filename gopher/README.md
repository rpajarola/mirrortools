# gopher

Recursively mirrors a [Gopher](https://en.wikipedia.org/wiki/Gopher_(protocol))
menu tree to a local directory by speaking the raw Gopher protocol (RFC
1436) directly over TCP: connect, send the selector, read until the peer
closes the connection. Registers itself as the `gopher` method of the
[`mirror` CLI](../README.md).

No third-party library is needed — the wire protocol is a handful of lines
of code — so this is a from-scratch Go implementation, not a wrapper around
anything.

## Usage

```
go run ./cmd/mirror gopher <gopher:// URL> <destdir>
```

For example:

```
go run ./cmd/mirror gopher gopher://gopher.floodgap.com/1/gopher/toybox ./mirror
```

Files land at `<destdir>/<host>/<selector>`, mirroring the selector path
structure the server exposes — the example above downloads into
`./mirror/gopher.floodgap.com/gopher/toybox/...`. A menu's `i` (info) lines
are collected into a `00README` file alongside its other entries.

Recursion stays within the subtree rooted at the URL's own path — a menu
entry pointing outside it is skipped and logged, not followed — and never
follows a link to a different host or port than the one in the URL, even if
the remote menu lists one; only item types that are actual downloadable
content (text, menu, binhex, DOS binary, uuencoded, generic binary, GIF,
image) are followed at all. Search (`7`), CSO phone book (`2`), Telnet
(`T`/`8`), and HTTP (`h`) entries are logged and skipped, since none of them
are Gopher content this tool can fetch on its own.

A selector whose local file already exists is skipped without re-fetching.
A selector that resolves to an existing local directory is still re-fetched
and re-parsed as a menu on every run, so re-running the command against the
same destination picks up anything new a Gopher menu has added since the
last mirror, without re-downloading files it already has.
