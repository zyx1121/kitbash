#!/bin/sh
# Build the kitbashd apk on an Alpine host from a git archive of this repo.
# usage: packaging/apk/build.sh <path to kitbash-<ver>.tar.gz>
# Runs as a non root user (abuild refuses root). Output lands in ~/packages/.
set -eu
tar=$1
ver=$(sed -n 's/^pkgver=//p' "$(dirname "$0")/APKBUILD")
[ "$(basename "$tar")" = "kitbash-$ver.tar.gz" ] || { echo "tarball must be named kitbash-$ver.tar.gz" >&2; exit 1; }
work=$(mktemp -d); cp "$(dirname "$0")/APKBUILD" "$work/"; cp "$tar" "$work/"
cd "$work"
[ -f ~/.abuild/abuild.conf ] || abuild-keygen -a -n -q
abuild -F checksum 2>/dev/null || abuild checksum
abuild -r
ls ~/packages/*/*/kitbashd-*.apk
