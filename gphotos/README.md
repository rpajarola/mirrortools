# gphotos

Mirrors a Google Photos library to a local directory by talking directly to
the same undocumented `batchexecute` API the photos.google.com web app
itself uses. Registers itself as the `gphotos` method of the [`mirror`
CLI](../README.md).

This is **not** the official API. As of March 2025, Google removed the
`photoslibrary.readonly` scope, so there is no supported OAuth-based way for
a third-party tool to bulk-read an existing library at all — Google Takeout
is the only sanctioned bulk-export path. This tool exists because that gap
leaves cookie-based session replay as the only option for an ongoing,
incremental mirror.

Because it rides on your logged-in browser session instead of OAuth, the
one thing it needs from you is a valid set of session cookies for
`photos.google.com`.

## Usage

```
go run ./cmd/mirror gphotos -cookies <path> [-after DATE] [-before DATE] [-last DURATION] [-companions] <account-email> <destdir>
```

| Flag | Default | Meaning |
|---|---|---|
| `-cookies` | `cookies.txt` | Session cookies — a Netscape `cookies.txt` export, or (macOS) a path straight to a `.binarycookies` file. Format is auto-detected. See [Getting cookies](#getting-cookies). |
| `-after` | none | Only download items taken on/after this date (`YYYY-MM-DD`). |
| `-before` | now | Only download items taken on/before this date (`YYYY-MM-DD`). |
| `-last` | none | Only download items from the last `Nd`/`Nw`/`Nm`/`Ny` (e.g. `30d`, `6m`). Alternative to `-after`, relative to `-before` or now. Mutually exclusive with `-after`. |
| `-companions` | `true` | Also fetch each item's RAW/DNG companion for a Pixel RAW+JPEG capture pair. Pass `-companions=false` to disable. |

`<account-email>` isn't used to select the account — that's determined
entirely by whichever session `-cookies` belongs to — it's only there for
logging, so any label that helps you identify the account is fine.

Downloaded files land under `destdir/YYYY/MM/`, organized by the date each
item was actually taken (in its own local capture time zone, not UTC). A
`.gphotos-index.json` in `destdir` tracks what's already been mirrored so
re-running the command only fetches what's new.

Every video also gets its poster-frame thumbnail saved alongside it as
`<name>.jpg` — there's no local, dependency-free way to produce one (that
would mean decoding the video, e.g. via `ffmpeg`), and without it a video
sitting in the mirror has no preview at all in most file browsers or
galleries.

## Getting cookies

Whichever route you use, you need to actually be logged into
`photos.google.com` in that browser first.

### Option A: a `cookies.txt` export

Works with Chrome, Firefox, or any Chromium-based browser.

1. Install a cookie-export extension — [Get cookies.txt
   LOCALLY](https://chromewebstore.google.com/detail/get-cookiestxt-locally/cclelndahbckbenkjhflpdbgdldlbecc)
   for Chrome, or [its Firefox
   equivalent](https://addons.mozilla.org/en-US/firefox/addon/get-cookies-txt-locally/).
2. For best results, do this in a private/incognito window so the session
   doesn't linger in your normal browsing profile afterward: open one, log
   into `photos.google.com`, then open a *second* tab before closing the
   Google Photos tab (closing the last tab of a private window can end the
   session).
3. Click the extension, export cookies for `google.com`, save as
   `cookies.txt`.
4. Close the private window once you're done, so that session isn't left
   open anywhere.
5. Point `-cookies` at the saved file.

The export is a snapshot, and Google's short-lived session cookie (see
[Device Bound Session Credentials](#device-bound-session-credentials-dbsc)
below) starts going stale within minutes of export. Use it soon after
exporting, and see that section if it doesn't work at all.

### Option B: point directly at a browser's live cookie jar (macOS)

On macOS, Safari and other WebKit-based apps store cookies in Apple's
`.binarycookies` format. `-cookies` auto-detects this format, so you can
skip the export step entirely and point it straight at the file — every run
picks up whatever's currently live in the browser, no re-export needed.

Safari's cookie jar is usually at:

```
~/Library/Containers/com.apple.Safari/Data/Library/Cookies/Cookies.binarycookies
```

(The exact container path can vary by macOS version. If that path doesn't
exist, look for other `Cookies.binarycookies` files under
`~/Library/Containers/*/Data/Library/Cookies/` or the legacy
`~/Library/Cookies/Cookies.binarycookies`.)

Log into `photos.google.com` in Safari, then:

```
go run ./cmd/mirror gphotos -cookies ~/Library/Containers/com.apple.Safari/Data/Library/Cookies/Cookies.binarycookies someone@gmail.com ./photos
```

## Device Bound Session Credentials (DBSC)

If cookies exported from **Chrome** seem to stop working within minutes, or
fail immediately with `WIZ_global_data not found`, this is very likely why.

Chrome's short-lived session cookie for Google (`__Secure-1PSIDTS` /
`__Secure-3PSIDTS`) can be cryptographically bound to the specific browser
process that created it, via a private key held in that machine's secure
hardware storage — a security feature called [Device Bound Session
Credentials](https://developer.chrome.com/docs/web-platform/device-bound-session-credentials).
The point of DBSC is exactly to make a copy-pasted cookie value useless
outside its original browser: Google's servers can tell the cookie is
missing its cryptographic proof of origin and refuse it, generally within
a few minutes. No amount of retrying, re-exporting quickly, or fiddling
with HTTP headers works around this — the private key never leaves the
device, so a separate process has no way to complete the required proof.

This tool tries to recover from ordinary short-lived-cookie expiry on its
own: it periodically calls Google's cookie-rotation endpoint during a long
run, and retries once via rotation if the initial session bootstrap fails
(see `refreshSession` / `rotateCookies` in `gphotos.go`). None of that helps
against DBSC specifically — rotation itself gets rejected the same way,
since it also requires that private-key proof.

### Workarounds

Try these roughly in order of effort:

1. **Use Safari instead of Chrome.** DBSC is currently a Chrome/Chromium
   feature; Safari isn't enrolled in it. Export via Safari's live cookie
   jar as described in [Option B](#option-b-point-directly-at-a-browsers-live-cookie-jar-macos)
   above. This is the most reliable fix and needs no configuration change.

2. **Use Firefox.** Same reasoning as Safari — Firefox doesn't implement
   DBSC. Export a `cookies.txt` from Firefox instead of Chrome (Option A).

3. **Disable DBSC in Chrome**, then log in and export fresh cookies:
   - `chrome://flags/#enable-bound-session-credentials` → Disabled, or
   - if your organization manages Chrome via policy, the
     `DeviceBoundSessionCredentialsEnabled` policy set to `false`.

   This only helps if DBSC is being applied client-side by Chrome itself.
   If your account is subject to Google Workspace's own server-side
   session-binding policy (see next point), disabling the Chrome flag won't
   change anything, because the server enforces it regardless of what the
   client presents.

4. **Check whether it's a Google Workspace policy, not just Chrome.** If
   your account is on a Google Workspace domain, an admin can independently
   turn on ["session binding" for cookie
   theft](https://knowledge.workspace.google.com/admin/security/prevent-cookie-theft-with-session-binding)
   at the account level. If so, no client-side change — Safari, Firefox, or
   disabling the Chrome flag — will avoid it, since the server is the one
   enforcing it. Whoever administers the Workspace domain can check the
   admin console for this setting.

If none of the above helps, the account's session cookies are being bound
one way or another, and there's no remaining workaround for cookie-based
access — Google Takeout is the fallback for a one-off bulk export.
