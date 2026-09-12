#!/bin/sh -e
# The apk overlay for the kitbashOS ISO, see PLAN.md section 4.
#
# Same shape as aports/scripts/genapkovl-dhcp.sh: a tarball of /etc that the
# initramfs unpacks over the live root before apk installs /etc/apk/world.
# Nothing in here is human facing. No passwords and no extra users, because
# every member arrives over ssh and the console is the break glass path
# (PLAN.md 4.5).

HOSTNAME="$1"
if [ -z "$HOSTNAME" ]; then
	echo "usage: $0 hostname"
	exit 1
fi

# Where build.sh left the kitbashd signing pubkeys. apk on the live system
# needs them to install kitbashd from the boot repository on the ISO.
keysdir="${KITBASH_ISO_KEYSDIR:-$(dirname "$0")/kitbash-keys}"

cleanup() {
	rm -rf "$tmp"
}

makefile() {
	OWNER="$1"
	PERMS="$2"
	FILENAME="$3"
	cat > "$FILENAME"
	chown "$OWNER" "$FILENAME"
	chmod "$PERMS" "$FILENAME"
}

rc_add() {
	mkdir -p "$tmp"/etc/runlevels/"$2"
	ln -sf /etc/init.d/"$1" "$tmp"/etc/runlevels/"$2"/"$1"
}

tmp="$(mktemp -d)"
trap cleanup EXIT

mkdir -p "$tmp"/etc
makefile root:root 0644 "$tmp"/etc/hostname <<HOSTNAME_EOF
$HOSTNAME
HOSTNAME_EOF

mkdir -p "$tmp"/etc/network
makefile root:root 0644 "$tmp"/etc/network/interfaces <<'INTERFACES_EOF'
auto lo
iface lo inet loopback

auto eth0
iface eth0 inet dhcp
INTERFACES_EOF

# A kitbash VM is headless and its console is a serial line, so the break
# glass path of PLAN.md 4.5 has to exist there. alpine-baselayout ships this
# file with the serial getty commented out. tty1 stays for a VM that does have
# a screen. No other tty: nobody logs in here but the operator.
makefile root:root 0644 "$tmp"/etc/inittab <<'INITTAB_EOF'
# /etc/inittab

::sysinit:/sbin/openrc sysinit
::sysinit:/sbin/openrc boot
::wait:/sbin/openrc default

tty1::respawn:/sbin/getty 38400 tty1
ttyS0::respawn:/sbin/getty -L 115200 ttyS0 vt100

# Stuff to do for the 3-finger salute
::ctrlaltdel:/sbin/reboot

# Stuff to do before rebooting
::shutdown:/sbin/openrc shutdown
INITTAB_EOF

# alpine-base is what the live system is, kitbashd is what makes it a kitbash
# host. apk installs both from the ISO's own repository at boot.
mkdir -p "$tmp"/etc/apk
makefile root:root 0644 "$tmp"/etc/apk/world <<'WORLD_EOF'
alpine-base
kitbashd
WORLD_EOF

mkdir -p "$tmp"/etc/apk/keys
for key in "$keysdir"/*.rsa.pub; do
	[ -f "$key" ] || continue
	install -m 0644 -o root -g root "$key" "$tmp"/etc/apk/keys/
done
ls "$tmp"/etc/apk/keys/*.rsa.pub >/dev/null 2>&1 ||
	echo "warning: no kitbashd signing key found in $keysdir" >&2

# First boot: deploy/install.sh, shipped inside the kitbashd apk, is what turns
# a fresh Alpine into a kitbash host. It is idempotent; the marker only keeps a
# reboot from paying for it twice.
mkdir -p "$tmp"/etc/local.d
makefile root:root 0755 "$tmp"/etc/local.d/kitbash-firstboot.start <<'FIRSTBOOT_EOF'
#!/bin/sh
# First boot of a kitbashOS image: run the host installer once.
set -eu
marker=/var/lib/kitbash/.firstboot-done
if [ -e "$marker" ]; then
	exit 0
fi
if [ ! -f /usr/share/kitbash/install.sh ]; then
	echo "kitbash-firstboot: /usr/share/kitbash/install.sh is missing" > /dev/console
	exit 1
fi

# Detached on purpose. install.sh starts kitbashd, whose init script is after
# local, and OpenRC will not start a service while one it is after is still
# starting: local is starting because of this very script, so calling
# install.sh inline deadlocks. Let local finish first, then do the work.
# Output goes to the console because OpenRC sends local.d output to /dev/null
# unless rc_verbose is set, and the first boot is what an operator watches.
(
	i=0
	while [ ! -e /run/openrc/started/local ] && [ "$i" -lt 120 ]; do
		sleep 1
		i=$((i + 1))
	done
	sh /usr/share/kitbash/install.sh
	kitbashd --version
	kitbash-mcp --version
	rc-service kitbashd status
	mkdir -p /var/lib/kitbash
	touch "$marker"
	echo kitbash-firstboot-ok
) > /dev/console 2>&1 &
FIRSTBOOT_EOF

rc_add devfs sysinit
rc_add dmesg sysinit
rc_add mdev sysinit
rc_add hwdrivers sysinit
rc_add modloop sysinit
# kitbashd needs the unified hierarchy mounted before it can create
# /sys/fs/cgroup/kitbash, and a service in the default runlevel can only need
# one that is already up by then, so cgroups goes in sysinit and not in boot.
# deploy/install.sh moves it the same way on a host it did not image.
rc_add cgroups sysinit

rc_add modules boot
rc_add sysctl boot
rc_add hostname boot
rc_add bootmisc boot
rc_add syslog boot
rc_add networking boot

# sshd is the MCP transport. local runs the firstboot script above, and after
# that the rootless podman prerequisites install.sh writes into
# /etc/local.d/kitbash-rootless.start.
rc_add sshd default
rc_add local default

rc_add mount-ro shutdown
rc_add killprocs shutdown
rc_add savecache shutdown

tar -c -C "$tmp" etc | gzip -9n > "$HOSTNAME".apkovl.tar.gz
