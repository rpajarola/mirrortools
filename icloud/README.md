# icloud

Mirrors iCloud Photos to a local directory by talking directly to the
private CloudKit web API (`setup.icloud.com`, `idmsa.apple.com/appleauth`)
that `icloud.com` itself uses. Registers itself as the `icloud` method of
the [`mirror` CLI](../README.md).

**This has not been tested against a real Apple account** — everything
below is derived from reading (not vendoring) two existing Go clients that
do implement this against real accounts —
[chyroc/icloudgo](https://github.com/chyroc/icloudgo) (a Go port of the
well-known Python [pyicloud](https://github.com/picklepete/pyicloud)) and
[alvaroaleman/icloud-go](https://github.com/alvaroaleman/icloud-go) — plus
unit tests against synthetic responses shaped to match. Endpoint URLs,
header names, and the CloudKit query/record JSON shapes are confirmed
against icloudgo's source specifically; this package's own code (session
persistence format, filename/date-folder layout, the download index) is
original and untested against the real API. Expect to need at least one
round of fixes against your actual account, the same way the `gphotos`
backend needed several — see its README for what that process tends to
look like.

Both reference libraries pull in dependencies well beyond what this repo
otherwise needs (an embedded database, a CLI framework, protobuf,
Kubernetes API machinery), so rather than depend on either, this
reimplements just the auth + list + download subset in stdlib Go, matching
`gphotos`'s zero-dependency design.

## Usage

```
go run ./cmd/mirror icloud [-password-file ...] [-session-dir ...] [-album ...] [-live-photo-video] [-after DATE] [-before DATE] [-last DURATION] <apple-id-email> <destdir>
```

| Flag | Default | Meaning |
|---|---|---|
| `-password-file` | none | Path to a file containing the Apple ID password. `$ICLOUD_PASSWORD` is used if this isn't set. There's no plain `-password` flag — passing a secret as a bare command-line argument leaks it into shell history and process listings (`ps`), which a file path doesn't. Only needed the first time, or after a saved session expires; see [Sign-in and 2FA](#sign-in-and-2fa). |
| `-session-dir` | a per-account directory under the OS user cache dir | Where the session (cookies, trust token) is persisted across runs. |
| `-album` | `All Photos` | Which smart album to mirror — see [Albums](#albums). |
| `-live-photo-video` | `true` | Also download a Live Photo's paired video. |
| `-after` / `-before` / `-last` | none | Same as `gphotos`: restrict by taken date. Applied as a client-side filter over whatever the listing returns, not as a server-side query bound. |

`<apple-id-email>` is both the login identifier and the account mirrored —
unlike `gphotos`, where the email is cosmetic and the account is whatever
the cookies belong to, iCloud has no equivalent of a pre-exported cookies
file.

Downloaded files land under `destdir/YYYY/MM/`, by taken date in UTC. A
`.icloud-index.json` in `destdir` tracks what's already been mirrored (and
where in the library listing the last run left off) so re-running only
fetches what's new.

## Sign-in and 2FA

Unlike `gphotos`, there's no cookie file to export ahead of time — the
first run needs your Apple ID password and, if the account has two-factor
authentication on (nearly all do), a 2FA code.

1. First run: provide `-password-file` (e.g. `-password-file <(echo "$MY_SECRET")`
   or a real file with restrictive permissions) or `$ICLOUD_PASSWORD`. If
   the account needs 2FA, you'll be prompted on the terminal for the
   6-digit code sent to one of your trusted devices.
2. Once verified, the session — cookies plus Apple's session and trust
   tokens — is saved to `-session-dir`. This is the same mechanism as a
   browser answering "remember this device" after 2FA: Apple's trust token
   is what lets a session skip 2FA on future sign-ins.
3. Later runs try the saved session first (`validateToken`) and, if it's
   still good, skip straight to mirroring — no password, no prompt, nothing
   printed about it beyond a log line. Only if that check fails does it
   fall back to a fresh sign-in, which needs the password again.

There's no equivalent of `gphotos`'s Device Bound Session Credentials
problem here — Apple's session/trust tokens aren't (as far as either
reference implementation indicates) bound to the originating process the
way Chrome's are, so a saved session should keep working across runs and
across machines if you copy `-session-dir` over. If a saved session does
stop validating, the tool falls back to a fresh sign-in automatically; you
only notice if that also needs a password you haven't provided.

## Albums

Only iCloud's built-in smart albums are supported for now: `All Photos`
(the default), `Favorites`, `Videos`, `Selfies`, `Panoramas`,
`Screenshots`, `Hidden`, `Recently Deleted`. Custom, user-created albums
aren't — the underlying CloudKit query is structured differently for those
and hasn't been implemented.

## Live Photos

A Live Photo comes back from the API as one record with two download
URLs — a still image and a paired video — rather than as two separate
library items the way a RAW+JPEG pair does in Google Photos. There's no
extra lookup step: `-live-photo-video` (on by default) just downloads both
URLs from the same listing response, saving the video alongside the still
under the same base filename with a `.MOV` extension.

## Known simplifications

- **Date bucketing uses UTC**, not each photo's own capture-location
  timezone (`gphotos` does the latter). iCloud does return a
  `timeZoneOffset` field, but its exact unit isn't confirmed against real
  traffic the way `gphotos`'s was, and getting a folder boundary off by a
  few hours near midnight is a low-stakes enough cosmetic issue that it's
  not worth guessing at an unverified field to fix.
- **Incremental sync is offset-based, not date-based.** "All Photos" is
  listed oldest-added-first: newly added items always appear at the
  highest offsets, so `.icloud-index.json` remembers where the last run
  left off and resumes from there rather than re-listing the whole
  library's metadata every time. If you ever suspect a gap (e.g. an old
  photo re-added to the library after being deleted), delete
  `.icloud-index.json` to force a full re-walk — already-downloaded files
  won't be re-fetched, since the index also tracks which item IDs already
  have files on disk regardless of where the listing walk resumes.
- **`-after`/`-before`/`-last` don't bound the listing walk**, only filter
  what's downloaded from it — every run still lists (not downloads) the
  full range between the resume offset and the end of the library.
