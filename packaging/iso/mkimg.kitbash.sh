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
	# One architecture for now. The apk builds for aarch64 as well, so this
	# is the only line that has to change once there is an arm64 runner.
	arch="x86_64"
	# mkimage names the output ${image_name}-${RELEASE}-${ARCH}.iso and
	# build.sh passes --tag <pkgver>, so the file name is kitbash-<pkgver>-x86_64.iso.
	image_name="kitbash"

	# A kitbash host is headless: the console is the break glass path
	# (PLAN.md 4.5), and on a VM that console is a serial line. tty0 comes
	# first and ttyS0 last, because the last console on the command line is
	# the one /dev/console points at, which is where OpenRC writes.
	kernel_cmdline="console=tty0 console=ttyS0,115200"
	# Same as profile_base without "quiet": an appliance that is only
	# watched over a serial line should say what it is doing.
	initfs_cmdline="modules=loop,squashfs,sd-mod,usb-storage"

	apkovl="genapkovl-kitbash.sh"
	hostname="kitbash"

	# 1. kitbashd itself, from the local repository build.sh indexes.
	# 2. What deploy/install.sh step 2 installs, so install.sh is offline.
	# 3. What setup-disk needs for an unattended sys install to a blank disk.
	apks="$apks
		kitbashd
		podman crun passt fuse-overlayfs shadow shadow-subids git openssh
		qemu-guest-agent curl nftables
		alpine-base alpine-conf openrc
		e2fsprogs sfdisk syslinux grub-bios grub-efi dosfstools
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
