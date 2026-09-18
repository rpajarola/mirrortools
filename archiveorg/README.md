# archiveorg

Mirrors an [Internet Archive](https://archive.org) item to a local directory
by reading the item's metadata file listing and downloading each file over
plain HTTPS. Registers itself as the `archiveorg` method of the [`mirror`
CLI](../README.md).

No archive.org account or API key is needed, and there's no dependency on
the official Python [`internetarchive`](https://github.com/jjjake/internetarchive)
library — this is a from-scratch port of a script that used it, talking to
archive.org's public `metadata` and `download` endpoints directly instead.

## Usage

```
go run ./cmd/mirror archiveorg [-f] <archive.org item> <destdir>
```

`<archive.org item>` can be a full URL — `https://archive.org/details/<id>`
or `https://archive.org/download/<id>` — or a bare identifier.

| Flag | Default | Meaning |
|---|---|---|
| `-f` | `false` | Re-download even if a previous run's `.done` marker is present in `<destdir>/<id>`. |

Each item is mirrored into its own `<destdir>/<id>` subdirectory (created if
needed), so one `<destdir>` can hold several mirrored items side by side.
Every file listed in the item's metadata is downloaded there (subdirectory
structure in a file's own name, if any, is preserved). Once every file has
been fetched — or already matched what's on disk — and, wherever the
metadata supplied one, its md5 checksum verified, a `<destdir>/<id>/.done`
marker is written. A later run of the same item sees that marker and skips
immediately, unless `-f` is passed.

Unlike the Python `internetarchive` library, `.done` here is never written
for a partial or failed download: any error partway through is returned
immediately, no marker is written, and a subsequent run resumes rather than
silently reporting success. Files already on disk at their expected size are
skipped without re-fetching.

One file is exempt from checksum verification: an item's own
`<id>_files.xml` lists every file's checksum including its own `<file>`
entry, whose md5 was necessarily computed before that self-referential entry
existed — so it can never truthfully match its own declared checksum. This
is an archive.org quirk confirmed against multiple real items' metadata, not
a sign of a bad download.
