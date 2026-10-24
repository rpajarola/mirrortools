#!/usr/bin/env bash
# Builds a macOS installer package with a universal (arm64 and x86_64)
# binary, on macOS with Go and the Xcode command line tools.
#
#   packaging/build-pkg.sh                 # version from git
#   VERSION=1.2.3 packaging/build-pkg.sh
#
# Set PKG_SIGN_IDENTITY to a "Developer ID Installer" identity to sign it.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
. packaging/version.sh

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
root="$work/root"
share="$root/usr/local/share/mirrortools"
mkdir -p "$root/usr/local/bin" "$share" "$root/usr/local/share/man/man1"

for arch in arm64 amd64; do
  CGO_ENABLED=0 GOOS=darwin GOARCH=$arch \
    go build -trimpath -ldflags="-s -w -X $VERSION_VAR=$VERSION" -o "$work/mirror-$arch" ./cmd/mirror
done
lipo -create -output "$root/usr/local/bin/mirror" "$work/mirror-arm64" "$work/mirror-amd64"

cp README.md LICENSE "$share/"
for f in */README.md; do
  case $f in
    ftpmirror/*) name=ftp ;;
    httpmirror/*) name=http ;;
    *) name=${f%%/*} ;;
  esac
  cp "$f" "$share/$name.md"
done
install -m 0644 man/mirror.1 "$root/usr/local/share/man/man1/"
install -m 0755 packaging/macos/uninstall.sh "$share/uninstall.sh"

pkgbuild --root "$root" \
  --identifier io.github.rpajarola.mirrortools --version "$VERSION" \
  --install-location / --ownership recommended "$work/mirrortools.pkg"

mkdir -p dist
out="dist/mirrortools-$VERSION.pkg"
sign=()
[ -n "${PKG_SIGN_IDENTITY:-}" ] && sign=(--sign "$PKG_SIGN_IDENTITY")
productbuild --package "$work/mirrortools.pkg" ${sign[@]+"${sign[@]}"} "$out"
ls -l "$out"
