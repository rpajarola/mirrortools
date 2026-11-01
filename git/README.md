# git

Mirrors a remote Git repository as a bare clone (`git clone --mirror`), or
updates an existing bare mirror in place (`git remote update --prune`).
Registers itself as the `git` method of the [`mirror` CLI](../README.md).

## Usage

```
mirror [git] <git URL> [<destdir>]
```

Accepted sources:

- `https://` and `http://` repository URLs
- `git://` URLs
- `ssh://` and `git+ssh://` URLs
- SCP-style `[user@]host:path` (e.g. `git@github.com:user/repo.git`)

The bare repository lands at `<destdir>/<host>/<path>`, with any trailing
`.git` stripped from `<path>` so that `https://host/user/repo`,
`https://host/user/repo.git` and `git@host:user/repo.git` all update the
same local mirror.

## Auto-detection

When `mirror` is invoked without a method (`mirror <source> [<destdir>]`) or
with `http` (`mirror http <source> [<destdir>]`), Git repositories are
detected automatically:

- by URL form (`git://`, `ssh://`, `git+ssh://`, `git@host:path`,
  `[user@]host:path.git`, `http(s)://.../*.git`, and repository URLs on
  GitHub, GitLab, Codeberg, Bitbucket, Gitea, sourcehut and
  `*.googlesource.com`);
- by probing `<url>/info/refs?service=git-upload-pack` for any other
  `http://` or `https://` URL.

## Re-runs and resuming

Initial clones are written to `<destdir>/<host>/<path>.gitmirror-tmp` and
renamed into place once complete, so an interrupted clone never leaves a
half-initialized repository at the real path. Re-running `mirror` on an
already-cloned repository runs `git remote update --prune` to fetch all
updated refs and prune deleted ones.
