# sourceforge

Mirrors the files area of a [SourceForge](https://sourceforge.net) project
(`https://sourceforge.net/projects/<name>/files/`) to a local directory:
every file in every folder, and none of the site's own pages around them
(summary, reviews, support, ads, ...). Registers itself as the
`sourceforge` method of the [`mirror` CLI](../README.md), which picks it
automatically for SourceForge project URLs.

No SourceForge account is needed.

## Usage

```
mirror [sourceforge] [-wait <duration>] [-retries <n>] <SourceForge URL> <destdir>
```

`<SourceForge URL>` can be:

- any project URL — `https://sourceforge.net/projects/<name>/`, its
  `/files/`, `/reviews/` or other pages, `/p/<name>/`, or `sf.net` — for
  the whole files area;
- a folder in the files area, such as
  `https://sourceforge.net/projects/<name>/files/<folder>/`, for just that
  folder and everything under it;
- a file, such as
  `https://sourceforge.net/projects/<name>/files/<folder>/<file>/download`
  or `https://downloads.sourceforge.net/project/<name>/<folder>/<file>`,
  for just that file.

| Flag | Default | Meaning |
|---|---|---|
| `-wait` | `0` | Delay between requests, e.g. `1s` or `500ms`. |
| `-retries` | `5` | How many times to retry a request that failed with 429 Too Many Requests, a 5xx status, a network error or a bad checksum. |

Each project is mirrored into its own `<destdir>/<name>` subdirectory, with
the files area's folder structure below it. Mirroring a folder or file
puts it at the same place under `<destdir>/<name>` as mirroring the whole
project would, so partial and full mirrors of a project fit together.

## How it works

Every files-area page embeds a JSON description of the folder's entries
for its own scripts (`net.sf.files`): each entry's name, whether it's a
folder or a file, and a file's md5 checksum. Folders are listed by reading
that, so nothing depends on the layout of the page's HTML, and folders are
walked recursively. SourceForge shows all of a folder's entries on one
page, so there's no paging.

Files are fetched from `downloads.sourceforge.net/project/<name>/<path>`,
which redirects to a download mirror. Each download is written under a
temporary name, its md5 checksum checked against the listing, its
modification time set to the upload time SourceForge gives in
`Last-Modified`, and only then renamed into place.

Entries that aren't files hosted on SourceForge — links to an external
site — and files that aren't downloadable yet (staged for a later release)
are skipped and logged.

## Re-running

A later run lists the folders again, and downloads only files that are
new, or that no longer match their listed md5 checksum (changed on
SourceForge, or damaged locally). Files that have no checksum in the
listing are downloaded once and then kept. Files removed from SourceForge
are left in place.

A file or folder that fails doesn't stop the run; the rest is still
mirrored, and the run then fails, naming what failed, so that running it
again picks those up.

## Rate limiting

Requests are made one at a time. One that SourceForge answers with 429 or
a 5xx status, or that fails on the network or with a bad checksum, is
tried again after 2s, 4s, 8s, ... (at most 10 minutes), or after as long as
the response's `Retry-After` header says. `-wait` adds a fixed pause
between all requests; it can be set once in the config file:

```toml
[sourceforge]
wait = "1s"
```

Requests carry a `mirrortools/<version>` User-Agent.
