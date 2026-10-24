#!/bin/sh
# Removes mirrortools installed from the .pkg. Mirrored data is never
# touched.
#   sudo /usr/local/share/mirrortools/uninstall.sh
set -e
if [ "$(id -u)" != 0 ]; then
  echo "run with sudo" >&2
  exit 1
fi
rm -f /usr/local/bin/mirror
rm -rf /usr/local/share/mirrortools
pkgutil --forget io.github.rpajarola.mirrortools >/dev/null 2>&1 || true
echo "mirrortools removed"
