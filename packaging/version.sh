# Sourced by the build scripts: sets VERSION from the environment, the git
# tag (without its v), or 0.0.0~git<sha>.
VERSION_VAR=github.com/rpajarola/mirrortools/internal/version.Version
if [ -z "${VERSION:-}" ]; then
  if tag=$(git describe --tags --exact-match 2>/dev/null); then
    VERSION="${tag#v}"
  else
    VERSION="0.0.0~git$(git rev-parse --short HEAD)"
  fi
fi
