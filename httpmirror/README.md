# httpmirror

Recursively mirrors everything under a path of an HTTP(S) server to a local
directory, keeping the server's directory structure. Registers itself as the
`http` method of the [`mirror` CLI](../README.md).

## Usage

```
go run ./cmd/mirror http [-update] [-wait 1s] <http(s) URL> <destdir>
```

| Flag | Default | Meaning |
|---|---|---|
| `-update` | `false` | Re-check everything earlier runs already mirrored (conditional requests, so unchanged files cost a `304` only), re-read directory listings for new files, and retry URLs that failed with a 4xx. |
| `-wait` | `0` | Delay between requests. |

Files land at `<destdir>/<host>/<path>`. A URL ending in `/` is saved as
`index.html` in that directory; a query string stays part of the file name
(`page.php?id=3`). Each file's mtime is set from its `Last-Modified` header.
Bytes are stored exactly as served (no transparent gzip decoding).

## What gets fetched

The scope is the start URL's directory, after following redirects:
`https://host/pub`, `https://host/pub/` and `https://host/pub/index.html`
all mirror `/pub/`. Links (`<a>`, `<area>`, `<frame>`, `<iframe>`,
redirects) are only followed if they stay on the same scheme and host and
under that path.

The exception is page requisites of the HTML pages saved: images
(including `srcset`), stylesheets, scripts, icons, media, and whatever
stylesheets (`<link>`, `<style>` and `style=""`) reference through `url()`
and `@import`. These are fetched wherever they are, other hosts included,
so the mirrored pages still render. Requisites aren't crawled any further,
except that a fetched stylesheet's own references are fetched too.

## Directory listings

Server-generated directory listings are only used to find the files they
list. Their HTML isn't saved (the directory itself is still created), and
links on them that carry a query string are ignored, which covers column
sorting such as Apache's `?C=N;O=D` and Caddy's `?sort=name&order=asc`. A
page is treated as a listing if any of these is true:

- its `<title>` starts with `Index of` (Apache, nginx, lighttpd, ...),
  `Directory listing for` (Python's `http.server`, Tomcat) or `Directory:`
  (Jetty);
- it has an IIS-style `[To Parent Directory]` link;
- it has at least two column-sorting links back to itself;
- it's Go `http.FileServer`'s bare `<pre>` of links.

## Re-runs and resuming

Crawl state is kept in a SQLite database, `<destdir>/.httpmirror.db`. Every
discovered URL is recorded along with whether it's done, so an interrupted
run picks up where it stopped. By default a re-run doesn't touch anything
already mirrored and makes no requests at all if nothing is left to do.

A URL that fails transiently (network error, `408`, `429`, `5xx`) is retried
up to 3 times with backoff. If it still fails, it's recorded as an error,
the run carries on, Mirror exits with an error at the end, and the next run
retries it. A URL that fails with any other 4xx is logged and recorded as
failed. It doesn't fail the run and is only retried with `-update`.
