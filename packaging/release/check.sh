#!/bin/sh
# The version has one source, pkgver in packaging/apk/APKBUILD. Everything
# else that names a version must agree with it. CI runs this on every push;
# release.yml refuses to release when it fails.
set -eu
cd "$(dirname "$0")/../.."
ver=$(sed -n 's/^pkgver=//p' packaging/apk/APKBUILD)
case "$ver" in
	*.*.*) ;;
	*) echo "check: pkgver '$ver' is not X.Y.Z" >&2; exit 1 ;;
esac
fail=0
# Every kitbashd-X.Y.Z, kitbash-X.Y.Z, vX.Y.Z and kitbash-X.Y.Z-x86_64.iso in
# the operator facing files must be this version.
for f in README.md packaging/iso/README.md; do
	[ -f "$f" ] || continue
	bad=$(grep -noE '(kitbashd-|kitbash-|\bv)[0-9]+\.[0-9]+\.[0-9]+' "$f" | grep -v -- "-$ver\$" | grep -v "v$ver\$" || true)
	if [ -n "$bad" ]; then
		echo "check: $f names a version other than $ver:" >&2
		echo "$bad" >&2
		fail=1
	fi
done
[ "$fail" = 0 ] || exit 1
echo "check: every version mention is $ver"
