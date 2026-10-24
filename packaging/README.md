# Packages

mirrortools is packaged as a `.deb` for Debian and Ubuntu (amd64 and arm64,
published in the shared apt repository
[rpajarola/apt](https://github.com/rpajarola/apt)), a `.pkg` for macOS
(universal), and a Homebrew formula in
[rpajarola/homebrew-tap](https://github.com/rpajarola/homebrew-tap).
Publishing a GitHub release builds all of them
([`.github/workflows/release.yml`](../.github/workflows/release.yml)).

## Install

**Ubuntu and Debian**, from the [rpajarola apt repository](https://rpajarola.github.io/apt),
which also has the other packages built this way:

```
sudo curl -fsSLo /usr/share/keyrings/rpajarola.gpg https://rpajarola.github.io/apt/rpajarola.gpg
echo "deb [signed-by=/usr/share/keyrings/rpajarola.gpg] https://rpajarola.github.io/apt stable main" \
  | sudo tee /etc/apt/sources.list.d/rpajarola.list
sudo apt update
sudo apt install mirrortools
```

or a downloaded package: `sudo apt install ./mirrortools_<version>_<arch>.deb`.

**macOS** with Homebrew:

```
brew install rpajarola/tap/mirrortools
```

or the `.pkg` from a release: `sudo installer -pkg mirrortools-<version>.pkg -target /`
(the package is not signed, so double-clicking it needs right-click → Open).

## What is installed

Debian and Ubuntu:

```
/usr/bin/mirror
/usr/share/man/man1/mirror.1
/usr/share/doc/mirrortools/                # README.md, and <method>.md for each method
```

macOS (`.pkg`):

```
/usr/local/bin/mirror
/usr/local/share/man/man1/mirror.1
/usr/local/share/mirrortools/              # README.md, <method>.md, uninstall.sh
```

Remove it with `sudo /usr/local/share/mirrortools/uninstall.sh`. Mirrored
data is never touched.

## Build

```
go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest   # once
packaging/build-deb.sh      # dist/mirrortools_<version>_{amd64,arm64}.deb
packaging/build-pkg.sh      # on macOS: dist/mirrortools-<version>.pkg
```

The version is the git tag without its `v`, or `0.0.0~git<sha>`; set
`VERSION` to override it. mirror is pure Go, so `build-deb.sh`
cross-compiles both architectures on any machine (set `ARCHES` to build
fewer). `build-pkg.sh` builds both macOS architectures and joins them,
and signs the package if `PKG_SIGN_IDENTITY` names a "Developer ID
Installer" identity.

## Releasing

Tag, then publish a GitHub release for the tag:

```
git tag v0.1.0 && git push origin v0.1.0
gh release create v0.1.0 --generate-notes
```

The release workflow then attaches the `.deb`s and the `.pkg`, has
[rpajarola/apt](https://github.com/rpajarola/apt) rebuild the apt
repository (from the `.deb`s of every release of every project in it),
and updates the formula's URL and checksum in
[rpajarola/homebrew-tap](https://github.com/rpajarola/homebrew-tap). To
rebuild a release's packages, run the workflow by hand with its tag.

One-time setup: a `PACKAGES_TOKEN` secret, a fine-grained token with
contents read and write on `rpajarola/homebrew-tap` and `rpajarola/apt`
(the same token in every project):

```
gh secret set PACKAGES_TOKEN
```

Without it, the release still gets its packages, but the formula is not
updated and the apt repository is only rebuilt by its weekly run (or by
hand: Actions → publish → Run workflow in rpajarola/apt). The signing key
lives in rpajarola/apt.
