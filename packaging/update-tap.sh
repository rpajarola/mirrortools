#!/usr/bin/env bash
# Points a formula in the Homebrew tap at a release's source tarball, and
# sets its version if the formula gives one (for tags Homebrew can't parse).
#
#   TAP_TOKEN=<token> packaging/update-tap.sh <formula> <tag>
#
# TAP_TOKEN needs contents:write on rpajarola/homebrew-tap.
set -euo pipefail

formula=$1
tag=$2
repo=${GITHUB_REPOSITORY:-rpajarola/$formula}
tap=${TAP_REPO:-rpajarola/homebrew-tap}
url="https://github.com/$repo/archive/refs/tags/$tag.tar.gz"

sha=$(curl -fsSL "$url" | shasum -a 256 | cut -d' ' -f1)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
git clone -q "https://x-access-token:$TAP_TOKEN@github.com/$tap.git" "$work/tap"
f="$work/tap/Formula/$formula.rb"
sed -i.bak -E \
  -e "s|^(  url )\".*\"|\1\"$url\"|" \
  -e "s|^(  sha256 )\".*\"|\1\"$sha\"|" \
  -e "s|^(  version )\".*\"|\1\"${tag#v}\"|" "$f"
rm -f "$f.bak"
cd "$work/tap"
if git diff --quiet; then
  echo "update-tap.sh: $formula already at $tag"
  exit 0
fi
git -c user.name="github-actions[bot]" -c user.email="41898282+github-actions[bot]@users.noreply.github.com" \
  commit -qam "$formula ${tag#v}"
git push -q
echo "update-tap.sh: $formula -> $tag ($sha)"
