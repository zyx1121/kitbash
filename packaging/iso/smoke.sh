#!/bin/sh
# Boot a kitbashOS ISO in qemu and check that it comes up as a kitbash host.
#
# usage: packaging/iso/smoke.sh <iso> [serial log]
#
# Passes when the serial console shows the firstboot script finishing, which
# means deploy/install.sh ran, kitbashd is up and both binaries answered
# --version. Needs qemu-system-x86_64 and no root: KVM is used when /dev/kvm is
# writable, otherwise this falls back to TCG and just takes longer.
set -eu

usage() {
	echo "usage: $0 <iso> [serial log]" >&2
	exit 2
}

[ $# -ge 1 ] || usage
iso=$1
log=${2:-$(mktemp -t kitbash-smoke.XXXXXX)}
timeout=${KITBASH_SMOKE_TIMEOUT:-300}
marker=kitbash-firstboot-ok

[ -f "$iso" ] || { echo "no such iso: $iso" >&2; exit 1; }
: > "$log"

accel="-machine accel=tcg"
if [ -w /dev/kvm ]; then
	accel="-enable-kvm -cpu host"
fi

qemu-system-x86_64 \
	$accel \
	-m 2048 -smp 2 \
	-nographic -no-reboot \
	-cdrom "$iso" -boot d \
	-nic user,model=virtio-net-pci \
	>"$log" 2>&1 </dev/null &
qemu=$!

cleanup() {
	kill "$qemu" 2>/dev/null || true
	wait "$qemu" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo ">>> booting $(basename "$iso"), serial log $log"
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
	echo ">>> PASS: $marker after ${i}s"
else
	echo ">>> FAIL: no $marker within ${timeout}s" >&2
fi
exit "$rc"
