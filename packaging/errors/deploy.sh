#!/bin/sh
# Publish the error pages to https://kitbash.zyx.tw/errors/. The gateway CT on
# pve serves /home/user/gateway/sites/kitbash with Caddy's file_server; this
# renders the pages and copies them there. Run from a machine that can
# `ssh pve`; the release pipeline does not run it, an operator does after a
# change to packaging/errors.
set -eu
cd "$(dirname "$0")/../.."
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
go run ./packaging/errors "$out"
# COPYFILE_DISABLE keeps macOS tar from adding ._ files and xattr headers.
COPYFILE_DISABLE=1 tar -C "$out" --no-xattrs -cf - errors \
	| ssh pve 'pct exec 200 -- sh -c "mkdir -p /home/user/gateway/sites/kitbash && rm -rf /home/user/gateway/sites/kitbash/errors && tar -xf - -C /home/user/gateway/sites/kitbash"'
check() {
	code=$(curl -s -o /dev/null -w '%{http_code}' -L "$1")
	[ "$code" = 200 ] || { echo "deploy: $1 answers $code" >&2; exit 1; }
}
check https://kitbash.zyx.tw/errors/
for slug in $(sed -n 's/^\tSlug[A-Za-z]* *= *"\([a-z0-9-]*\)"/\1/p' internal/problem/problem.go); do
	# The type URI has no trailing slash; the gateway redirects it, so check both.
	check "https://kitbash.zyx.tw/errors/$slug"
	check "https://kitbash.zyx.tw/errors/$slug/"
done
echo "deploy: every error page answers 200 at https://kitbash.zyx.tw/errors/"
