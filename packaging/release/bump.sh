#!/bin/sh
# Start a release: set the version everywhere and make the release commit.
# usage: packaging/release/bump.sh X.Y.Z
# Merging that commit to main is what releases: release.yml sees a pkgver
# without a tag, builds the apk and the ISO, and creates tag and release.
set -eu
cd "$(dirname "$0")/../.."
new=${1:?usage: bump.sh X.Y.Z}
case "$new" in
	*.*.*) ;;
	*) echo "bump: '$new' is not X.Y.Z" >&2; exit 1 ;;
esac
old=$(sed -n 's/^pkgver=//p' packaging/apk/APKBUILD)
[ "$new" != "$old" ] || { echo "bump: already $old" >&2; exit 1; }
[ -z "$(git status --porcelain)" ] || { echo "bump: working tree not clean" >&2; exit 1; }
sed -i.bak "s/^pkgver=.*/pkgver=$new/; s/^pkgrel=.*/pkgrel=0/" packaging/apk/APKBUILD
for f in README.md packaging/iso/README.md; do
	[ -f "$f" ] || continue
	sed -i.bak -E "s/(kitbashd-|kitbash-)$old([-.])/\1$new\2/g; s/\bv$old\b/v$new/g" "$f"
done
find . -name '*.bak' -not -path './.git/*' -delete
sh packaging/release/check.sh
git add -A
git commit -q -m "Release $new"
echo "bump: committed 'Release $new'. Open a PR, merge it, and release.yml does the rest."
