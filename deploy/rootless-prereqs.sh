#!/bin/sh
# Rootless podman prerequisites that live in tmpfs and do not survive a reboot.
#
# The kitbashd apk installs this as /usr/share/kitbash/rootless-prereqs.sh and
# kitbashd.initd runs it from start_pre, because boot restore starts every
# registered Process as its owner and needs both of the things below (PLAN.md
# section 2.3). deploy/install.sh runs it too, and leaves an
# /etc/local.d/kitbash-rootless.start that calls it, for a host installed
# before the daemon did this itself. Idempotent; run it as often as you like.
set -u

# Rootless podman mounts inside its own namespace, and a mount made there is
# only visible to the container when / propagates it. Nothing else reports
# this one, so say so here: a host whose / is private starts a Process that
# cannot see its own mounts.
mount --make-rshared / || echo "kitbash: mount --make-rshared / failed; rootless containers may not see their own mounts" >&2

# XDG_RUNTIME_DIR, where rootless podman keeps its state. A kitbash host runs
# OpenRC and has no logind, so nothing else creates these. uid 65534 is nobody.
for u in $(awk -F: '$3>=1000 && $3<65534 {print $1":"$3}' /etc/passwd); do
	name=${u%%:*}; uid=${u##*:}
	d=/run/user/$uid; mkdir -p "$d"; chown "$name" "$d"; chmod 700 "$d"
done

# Neither failure is a reason to leave the host without a daemon: start_pre
# calls this, and a non zero exit there is a kitbashd that does not start.
# A member whose directory could not be made shows up again as that member's
# Process failing to restore, in /var/log/kitbashd.log; the mount is only ever
# reported by the warning above, which OpenRC puts on the console.
exit 0
