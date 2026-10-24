#!/usr/bin/env bash
# Builds the .debs for amd64 and arm64. mirror is pure Go (no cgo), so both
# are cross-compiled on whatever machine this runs on.
#
# Requires Go and nfpm (https://nfpm.goreleaser.com):
#   go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest
#
# Usage:
#   packaging/build-deb.sh                 # version from git, amd64 and arm64
#   VERSION=1.2.3 ARCHES=amd64 packaging/build-deb.sh
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
. packaging/version.sh

if ! command -v nfpm >/dev/null 2>&1; then
  echo "build-deb.sh: nfpm not found on PATH; see packaging/README.md" >&2
  exit 1
fi

for arch in ${ARCHES:-amd64 arm64}; do
  mkdir -p dist/bin
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch \
    go build -trimpath -ldflags="-s -w -X $VERSION_VAR=$VERSION" -o dist/bin/mirror ./cmd/mirror
  VERSION="$VERSION" ARCH="$arch" nfpm pkg --config packaging/nfpm.yaml --packager deb \
    --target "dist/mirrortools_${VERSION}_${arch}.deb"
  rm -rf dist/bin
  ls -l "dist/mirrortools_${VERSION}_${arch}.deb"
done
