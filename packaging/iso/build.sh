#!/bin/sh
# Build the kitbashOS ISO from a kitbashd apk.
#
# usage: packaging/iso/build.sh <path to kitbashd apk> <outdir> [arch]
# output: <outdir>/kitbash-<pkgver>-<arch>.iso
#
# The architecture is the third argument, or $KITBASH_ARCH, or this host's.
# It has to match the apk: the ISO is built natively, never under emulation.
#
# Runs on any Alpine 3.23 host, as a normal user or as root in an alpine:3.23
# container. Needs: alpine-sdk alpine-conf xorriso squashfs-tools grub grub-efi
# mtools dosfstools git, plus syslinux and grub-bios on x86_64.
#
# The version is pkgver in packaging/apk/APKBUILD and nothing else.
set -eu

usage() {
	echo "usage: $0 <kitbashd apk> <outdir> [arch]" >&2
	exit 2
}

[ $# -ge 2 ] && [ $# -le 3 ] || usage
apkfile=$(realpath "$1")
outdir=$2
[ -f "$apkfile" ] || { echo "no such apk: $apkfile" >&2; exit 1; }

here=$(cd "$(dirname "$0")" && pwd)
repo=$(dirname "$(dirname "$here")")
pkgver=$(sed -n 's/^pkgver=//p' "$repo/packaging/apk/APKBUILD")
[ -n "$pkgver" ] || { echo "could not read pkgver from packaging/apk/APKBUILD" >&2; exit 1; }

# Alpine's name for the architecture. uname -m already says x86_64 and
# aarch64 on Linux; arm64 is what macOS and Go call the same thing.
arch=${3:-${KITBASH_ARCH:-$(uname -m)}}
case "$arch" in
	arm64|aarch64) arch=aarch64 ;;
	amd64|x86_64)  arch=x86_64 ;;
	*) echo "unsupported arch: $arch (kitbash releases x86_64 and aarch64)" >&2; exit 1 ;;
esac

# apk verifies the kitbashd apk against the keys in mkimage's apk root, and
# mkimage fills that root from /etc/apk/keys plus the build key. Unless the
# ISO is built with the very key the apk was signed with, the repo's signing
# keys have to be trusted here, and --hostkeys below carries them into the
# image as well.
for key in "$repo"/packaging/apk/keys/*.rsa.pub; do
	[ -f "$key" ] || continue
	if [ -f "/etc/apk/keys/$(basename "$key")" ]; then
		continue
	fi
	if [ -w /etc/apk/keys ]; then
		cp "$key" /etc/apk/keys/
		echo ">>> trusting $(basename "$key")"
	else
		echo "warning: $(basename "$key") is not in /etc/apk/keys and this user cannot put it there" >&2
	fi
done

# mkimage.sh runs "apk add --initdb --no-chown", and apk-tools 3 refuses
# --no-chown as root, so the whole build has to happen as somebody else. A root
# caller, which is what GitHub Actions is in an alpine:3.23 container, gets a
# build user and re-runs this script as them.
if [ "$(id -u)" = 0 ] && [ -z "${KITBASH_ISO_DEMOTED:-}" ]; then
	user=${KITBASH_ISO_USER:-kitbash-build}
	if ! id "$user" >/dev/null 2>&1; then
		adduser -D -G abuild "$user" 2>/dev/null || adduser -D "$user"
	fi
	home=$(getent passwd "$user" | cut -d: -f6)
	work=${KITBASH_ISO_WORK:-$home/iso-work}
	mkdir -p "$work" "$outdir"
	chown -R "$user" "$work" "$outdir"
	if [ -n "${PACKAGER_PRIVKEY:-}" ]; then
		# The key is usually root owned and the build user has to read it.
		mkdir -p "$home/.abuild"
		cp "$PACKAGER_PRIVKEY" "$PACKAGER_PRIVKEY.pub" "$home/.abuild/"
		chown -R "$user" "$home/.abuild"
		PACKAGER_PRIVKEY="$home/.abuild/$(basename "$PACKAGER_PRIVKEY")"
	fi
	echo ">>> running as $user: apk refuses mkimage's --no-chown as root"
	exec su -s /bin/sh "$user" -c \
		"KITBASH_ISO_DEMOTED=1 KITBASH_ISO_WORK='$work' PACKAGER_PRIVKEY='${PACKAGER_PRIVKEY:-}' sh '$0' '$apkfile' '$outdir' '$arch'"
fi

work=${KITBASH_ISO_WORK:-$HOME/iso-work}
aports=$work/aports
localrepo=$work/repo
mkdir -p "$work" "$localrepo/$arch"
mkdir -p "$outdir"
outdir=$(realpath "$outdir")

# 1. The signing key. mkimage signs the modloop and the boot repository index
#    with it, so one has to exist even in a throwaway container.
if [ -z "${PACKAGER_PRIVKEY:-}" ] && [ -f "$HOME/.abuild/abuild.conf" ]; then
	# shellcheck disable=SC1091
	. "$HOME/.abuild/abuild.conf"
fi
if [ -z "${PACKAGER_PRIVKEY:-}" ]; then
	echo ">>> no abuild key, generating one"
	# -a installs the pubkey into /etc/apk/keys, which needs root or doas.
	abuild-keygen -a -n -q || abuild-keygen -n -q
	# shellcheck disable=SC1091
	. "$HOME/.abuild/abuild.conf"
fi
[ -f "$PACKAGER_PRIVKEY" ] || { echo "PACKAGER_PRIVKEY $PACKAGER_PRIVKEY does not exist" >&2; exit 1; }
export PACKAGER_PRIVKEY
echo ">>> signing with $PACKAGER_PRIVKEY"

# 2. A local repository holding just the kitbashd apk. mkimage resolves the
#    package out of it like any other, so the ISO carries kitbashd and every
#    dependency it pulls in.
rm -f "$localrepo/$arch"/*.apk
# apk asks the repository for <pkgname>-<pkgver>-r<pkgrel>.apk, whatever the
# file was called when it got here: the release names the two apks
# kitbashd-<ver>-r0.x86_64.apk and kitbashd-<ver>-r0.aarch64.apk so they can
# share one release, and apk would never find either under that name.
pkgrel=$(sed -n 's/^pkgrel=//p' "$repo/packaging/apk/APKBUILD")
cp "$apkfile" "$localrepo/$arch/kitbashd-$pkgver-r${pkgrel:-0}.apk"
apk index --description "kitbash-$pkgver" \
	--rewrite-arch "$arch" \
	--output "$localrepo/$arch/APKINDEX.tar.gz" \
	"$localrepo/$arch"/*.apk
abuild-sign -k "$PACKAGER_PRIVKEY" "$localrepo/$arch/APKINDEX.tar.gz"

# 3. aports, shallow and only scripts/, for mkimage.sh and its profiles.
if [ -d "$aports/.git" ]; then
	echo ">>> updating aports"
	git -C "$aports" fetch --quiet --depth 1 origin 3.23-stable
	git -C "$aports" reset --quiet --hard FETCH_HEAD
	git -C "$aports" clean -qfd scripts
else
	# The GitHub mirror is the default: gitlab.alpinelinux.org answers
	# clones from GitHub Actions runners with 418.
	echo ">>> cloning aports"
	git clone --quiet --depth 1 --filter=blob:none --sparse \
		--branch 3.23-stable \
		"${KITBASH_APORTS_URL:-https://github.com/alpinelinux/aports.git}" "$aports"
	git -C "$aports" sparse-checkout set scripts
fi
git config --global --add safe.directory "$aports" 2>/dev/null || true

# 4. Our profile, our overlay generator, and the two inputs they read.
cp "$here/mkimg.kitbash.sh" "$here/genapkovl-kitbash.sh" "$aports/scripts/"
chmod +x "$aports/scripts/genapkovl-kitbash.sh"
cp "$here/answers" "$aports/scripts/kitbash-answers"
rm -rf "$aports/scripts/kitbash-keys"
mkdir -p "$aports/scripts/kitbash-keys"
cp "$repo"/packaging/apk/keys/*.rsa.pub "$aports/scripts/kitbash-keys/"
# The key that signs the boot repository index has to be trusted by the
# installed system as well, otherwise apk refuses the ISO's own packages.
cp "$PACKAGER_PRIVKEY.pub" "$aports/scripts/kitbash-keys/"
export KITBASH_ISO_KEYSDIR="$aports/scripts/kitbash-keys"
export KITBASH_ISO_ANSWERS="$aports/scripts/kitbash-answers"

# 5. Build. --tag is the release string mkimage puts in the file name, so the
#    ISO is kitbash-<pkgver>-<arch>.iso and never carries a build date. The
#    mkimage cache is per architecture: only the kernel and apks sections
#    carry $ARCH in their key, and the apk overlay differs between the two.
iso="kitbash-$pkgver-$arch.iso"
rm -f "$outdir/$iso"
echo ">>> mkimage $iso"
# From aports/scripts, because mkimage.sh reads $apkovl relative to the
# current directory when it checksums the overlay generator. Run it anywhere
# else and a changed genapkovl-kitbash.sh reuses the cached overlay.
cd "$aports/scripts"
./mkimage.sh \
	--tag "$pkgver" \
	--hostkeys \
	--outdir "$outdir" \
	--workdir "$work/mkimage-$arch" \
	--arch "$arch" \
	--repository https://dl-cdn.alpinelinux.org/alpine/v3.23/main \
	--repository https://dl-cdn.alpinelinux.org/alpine/v3.23/community \
	--extra-repository "$localrepo" \
	--profile kitbash

[ -f "$outdir/$iso" ] || { echo "mkimage produced no $iso" >&2; ls -l "$outdir" >&2; exit 1; }
ls -lh "$outdir/$iso"
