#!/bin/sh
# Boot a kitbashOS ISO in qemu and check that it comes up as a kitbash host.
#
# usage: packaging/iso/smoke.sh <iso> [serial log]
#
# Passes when the serial console shows the firstboot script finishing, which
# means deploy/install.sh ran, kitbashd is up and both binaries answered
# --version. The architecture comes from the ISO's name, or from $KITBASH_ARCH,
# and decides the qemu binary and the machine:
#
#   x86_64   qemu-system-x86_64, the default machine, an IDE cdrom, ttyS0
#   aarch64  qemu-system-aarch64 -M virt, UEFI firmware, a virtio-scsi cdrom,
#            ttyAMA0
#
# Needs no root. KVM is used when /dev/kvm is writable and the guest is this
# machine's architecture, hvf on an Apple silicon Mac, otherwise TCG, which
# works everywhere and just takes longer.
set -eu

usage() {
	echo "usage: $0 <iso> [serial log]" >&2
	exit 2
}

[ $# -ge 1 ] || usage
iso=$1
log=${2:-$(mktemp -t kitbash-smoke.XXXXXX)}
marker=kitbash-firstboot-ok

[ -f "$iso" ] || { echo "no such iso: $iso" >&2; exit 1; }

# The ISO is kitbash-<ver>-<arch>.iso, so it says what it is.
arch=${KITBASH_ARCH:-}
if [ -z "$arch" ]; then
	case "$(basename "$iso")" in
		*-aarch64.iso) arch=aarch64 ;;
		*-x86_64.iso)  arch=x86_64 ;;
		*) echo "cannot tell the arch from $(basename "$iso"), set KITBASH_ARCH" >&2; exit 1 ;;
	esac
fi

# Hardware acceleration only helps when the guest is the host's own
# architecture: KVM on Linux, and Hypervisor.framework on an Apple silicon Mac,
# which is where the aarch64 image is accepted by hand (packaging/iso/README.md).
accel=tcg
case "$(uname -s):$(uname -m):$arch" in
	Linux:"$arch":"$arch")
		if [ -w /dev/kvm ]; then
			accel=kvm
		fi
		;;
	Darwin:arm64:aarch64) accel=hvf ;;
esac

# UEFI firmware for the aarch64 machine, which has no BIOS. Ubuntu's
# qemu-efi-aarch64 package is AAVMF: a 64 MiB code image and a variable store
# the guest writes, so the store is copied somewhere writable first. Other
# distributions ship one unpadded QEMU_EFI.fd that -bios takes instead.
vars=
fw_args=
# Sets fw_args, and vars when it made a copy of the variable store. Both are
# globals on purpose: called in a $() the mktemp below would happen in a
# subshell and the trap in this one would never delete the copy.
find_firmware() {
	local code= template=
	for f in /usr/share/AAVMF/AAVMF_CODE.fd \
		/usr/share/qemu-efi-aarch64/QEMU_EFI-pflash.raw \
		/usr/share/edk2/aarch64/QEMU_EFI-silent-pflash.raw \
		/usr/share/edk2/aarch64/QEMU_EFI-pflash.raw; do
		[ -f "$f" ] || continue
		code=$f
		break
	done
	for f in /usr/share/AAVMF/AAVMF_VARS.fd \
		/usr/share/qemu-efi-aarch64/vars-template-pflash.raw \
		/usr/share/edk2/aarch64/vars-template-pflash.raw; do
		[ -f "$f" ] || continue
		template=$f
		break
	done
	if [ -n "$code" ] && [ -n "$template" ]; then
		vars=$(mktemp -t kitbash-aavmf.XXXXXX)
		cat "$template" > "$vars"
		fw_args="-drive if=pflash,format=raw,unit=0,readonly=on,file=$code
			-drive if=pflash,format=raw,unit=1,file=$vars"
		return 0
	fi
	for f in /usr/share/qemu-efi-aarch64/QEMU_EFI.fd \
		/usr/share/edk2/aarch64/QEMU_EFI.fd \
		/usr/share/AAVMF/AAVMF_CODE.fd \
		/opt/homebrew/share/qemu/edk2-aarch64-code.fd; do
		[ -f "$f" ] || continue
		fw_args="-bios $f"
		return 0
	done
	echo "no UEFI firmware for aarch64: install qemu-efi-aarch64" >&2
	return 1
}

case "$arch" in
x86_64)
	qemu_bin=qemu-system-x86_64
	timeout=${KITBASH_SMOKE_TIMEOUT:-300}
	if [ "$accel" = tcg ]; then
		machine="-machine accel=tcg"
	else
		machine="-machine accel=$accel -cpu host"
	fi
	media="-cdrom $iso -boot d"
	;;
aarch64)
	qemu_bin=qemu-system-aarch64
	# Emulated aarch64 on an emulating host is slower than the x86 job and
	# the arm runners have no nested virtualisation, so this waits longer.
	timeout=${KITBASH_SMOKE_TIMEOUT:-900}
	if [ "$accel" = tcg ]; then
		# cortex-a72 rather than "max": TCG emulates it fully and it is
		# what every arm board Alpine's virt kernel targets looks like.
		machine="-machine virt,accel=tcg -cpu cortex-a72"
	else
		machine="-machine virt,accel=$accel -cpu host"
	fi
	find_firmware
	machine="$machine $fw_args"
	# The virt machine has no IDE, so the ISO arrives as a real cdrom on a
	# virtio-scsi bus, which the initramfs already carries drivers for.
	media="-drive if=none,id=cd0,file=$iso,format=raw,media=cdrom,readonly=on
		-device virtio-scsi-pci,id=scsi0
		-device scsi-cd,drive=cd0,bus=scsi0.0"
	;;
*)
	echo "unsupported arch: $arch" >&2
	exit 1
	;;
esac

: > "$log"

# shellcheck disable=SC2086
$qemu_bin \
	$machine \
	-m 2048 -smp 2 \
	-nographic -no-reboot \
	$media \
	-nic user,model=virtio-net-pci \
	>"$log" 2>&1 </dev/null &
qemu=$!

cleanup() {
	kill "$qemu" 2>/dev/null || true
	wait "$qemu" 2>/dev/null || true
	[ -z "$vars" ] || rm -f "$vars"
}
trap cleanup EXIT INT TERM

echo ">>> booting $(basename "$iso") as $arch (accel=$accel), serial log $log"
rc=1
i=0
while [ "$i" -lt "$timeout" ]; do
	if grep -q "$marker" "$log"; then
		rc=0
		break
	fi
	if ! kill -0 "$qemu" 2>/dev/null; then
		echo ">>> qemu exited before the guest reported $marker" >&2
		break
	fi
	i=$((i + 1))
	sleep 1
done

cleanup
trap - EXIT INT TERM

echo ">>> serial log tail ($log):"
tail -n 30 "$log"

if [ "$rc" -eq 0 ]; then
	echo ">>> PASS: $marker after ${i}s on $arch"
else
	echo ">>> FAIL: no $marker within ${timeout}s on $arch" >&2
fi
exit "$rc"
