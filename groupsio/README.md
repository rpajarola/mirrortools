# groupsio

Mirrors a [groups.io](https://groups.io) group's Files section to a local
directory via [groups.io's REST API](https://groups.io/api). Registers
itself as the `groupsio` method of the [`mirror` CLI](../README.md).

groups.io has no anonymous file listing, so an account with access to the
group is required.

## Usage

```
go run ./cmd/mirror groupsio -email <email> -password-file <path> <group> <destdir>
```

| Flag | Default | Meaning |
|---|---|---|
| `-email` | *(required)* | groups.io account email. |
| `-password-file` | *(required)* | Path to a file holding the account's password, one line. Kept out of the command line and shell history — never pass the password directly as a flag value. |

`<group>` is the group's short name — the `xyz` in `https://groups.io/g/xyz`.

For example:

```
go run ./cmd/mirror groupsio -email you@example.com -password-file pw.txt some-group ./mirror
```

Each group is mirrored into its own `<destdir>/<group>` subdirectory. The
group's own description lands at `<destdir>/<group>/README`; each
subdirectory gets its own `README` alongside its contents; each downloaded
file gets a sibling `<name>.desc` holding its groups.io description.

A file already on disk at its expected size is skipped without
re-fetching, so re-running the command against the same destination only
fetches what's new. Directories are always re-listed on every run (cheap,
and the only way to notice files added to the group since the last mirror).

## Notes

- If a file's API-provided `download_url` fails, this falls back to the
  group's ordinary web-UI file URL before giving up on that file.
- Errors on one file or one subdirectory are logged and skipped rather than
  aborting the whole mirror — a problem partway through a large group's
  file tree doesn't cost the files already fetched elsewhere in it.
