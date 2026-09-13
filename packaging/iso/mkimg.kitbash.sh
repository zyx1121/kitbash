# Alpine mkimage profile for kitbashOS, see PLAN.md section 4.
#
# Based on profile_virt: the virt kernel, no firmware for hardware a VM does
# not have. build.sh copies this file into aports/scripts/ where mkimage.sh
# picks it up as profile "kitbash".
#
# Everything deploy/install.sh needs is in apks, so the first boot of the
# image works with no network at all.

profile_kitbash() {
	profile_virt
	profile_abbrev="kitbash"
	title="kitbashOS"
	desc="Alpine plus kitbashd.
		An operating system layer for AI agents.
		MCP over SSH, Files, Packages, Processes, Telemetry."
	# Both architectures the apk builds for. mkimage builds the one it is
	# given with --arch and skips a profile whose list does not name it.
	arch="x86_64 aarch64"
	# mkimage names the output ${image_name}-${RELEASE}-${ARCH}.iso and
	# build.sh passes --tag <pkgver>, so the file name is
	# kitbash-<pkgver>-x86_64.iso or kitbash-<pkgver>-aarch64.iso.
	image_name="kitbash"

	# A kitbash host is headless: the console is the break glass path
	# (PLAN.md 4.5), and on a VM that console is a serial line. tty0 comes
	# first and the serial line last, because the last console on the
	# command line is the one /dev/console points at, which is where OpenRC
	# writes. x86 has an 8250 at ttyS0; qemu's aarch64 "virt" machine and
	# every arm server console is a PL011 at ttyAMA0.
	case "$ARCH" in
		aarch64|arm*) kitbash_console="ttyAMA0" ;;
		*)            kitbash_console="ttyS0" ;;
	esac
	kernel_cmdline="console=tty0 console=$kitbash_console,115200"
	# genapkovl-kitbash.sh runs as a separate process and puts a getty on
	# the same line, so it has to be told which one.
	export KITBASH_ISO_CONSOLE="$kitbash_console"
	# Same as profile_base without "quiet": an appliance that is only
	# watched over a serial line should say what it is doing.
	initfs_cmdline="modules=loop,squashfs,sd-mod,usb-storage"

	apkovl="genapkovl-kitbash.sh"
	hostname="kitbash"

	# The bootloaders setup-disk installs on a sys install. x86_64 can be
	# BIOS or UEFI and syslinux covers the first; aarch64 is UEFI only and
	# Alpine has neither syslinux nor grub-bios for it.
	case "$ARCH" in
		aarch64|arm*) kitbash_bootloaders="grub-efi efibootmgr" ;;
		*)            kitbash_bootloaders="syslinux grub-bios grub-efi" ;;
	esac

	# 1. kitbashd itself, from the local repository build.sh indexes.
	# 2. What deploy/install.sh step 2 installs, so install.sh is offline.
	# 3. What setup-disk needs for an unattended sys install to a blank disk.
	apks="$apks
		kitbashd
		podman crun passt fuse-overlayfs shadow shadow-subids git openssh
		qemu-guest-agent curl nftables
		alpine-base alpine-conf openrc
		e2fsprogs sfdisk dosfstools $kitbash_bootloaders
		"

	# The setup-alpine answerfile, published at /kitbash/answers on the ISO.
	kitbash_answers="${KITBASH_ISO_ANSWERS:-$scriptdir/kitbash-answers}"
}

# Extra files at the ISO root. mkimage has no hook for this, so the profile
# brings its own section: a section is a directory that gets merged into the
# image, and this one holds /kitbash/answers.
build_kitbash_extra() {
	mkdir -p "$DESTDIR"/kitbash
	cp "$kitbash_answers" "$DESTDIR"/kitbash/answers
}

section_kitbash_extra() {
	# Sections are global to mkimage.sh, so every other profile skips this.
	[ "$PROFILE" = "kitbash" ] || return 0
	build_section kitbash_extra $(checksum < "$kitbash_answers")
}
