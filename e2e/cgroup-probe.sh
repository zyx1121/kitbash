#!/bin/sh
# Does the kernel let a member move a process the way a container runtime does?
#
# The tree kitbashd builds gives a member the directory of a Process's cgroup
# and its three delegation files, and their rootless podman then makes the
# container's cgroup below it and moves the container into it. Moving a process
# between two cgroups needs write access to the common ancestor's cgroup.procs
# as well as to the destination's, so a refusal has two possible causes and a
# container in the way makes them hard to tell apart.
#
# This is the same shape with nothing in the way: a probe cgroup below the
# member's, delegated exactly as a Process's is, and a move out of the member's
# leaf into a child of it, run as the member. Nothing is left behind.
#
#   cgroup-probe.sh <member>
#
# Run as root.
set -u

name=${1:?usage: cgroup-probe.sh <member>}
member=/sys/fs/cgroup/kitbash/$name

[ "$(id -u)" = 0 ] || { echo "run as root"; exit 0; }
[ -d "$member" ] || { echo "$member does not exist"; exit 0; }

mkdir -p "$member/probe" || exit 0
chown "$name" "$member/probe" "$member/probe/cgroup.procs" \
  "$member/probe/cgroup.threads" "$member/probe/cgroup.subtree_control" 2>/dev/null || true

# The probe starts where a session is placed, because that is where the podman
# that starts a Process runs.
if [ -f "$member/run/cgroup.procs" ]; then
  echo $$ > "$member/run/cgroup.procs" 2>/dev/null || echo "the probe could not join $member/run"
fi
echo "the probe is in $(cut -d: -f3 /proc/self/cgroup | head -1)"

setpriv --reuid "$(id -u "$name")" --regid "$(id -g "$name")" --init-groups /bin/sh -c '
  set -u
  d='"$member"'/probe/child
  mkdir "$d" 2>&1 || { echo "the member could not create $d"; exit 1; }
  if echo $$ > "$d/cgroup.procs" 2>&1; then
    echo "the member moved into $(cut -d: -f3 /proc/self/cgroup | head -1)"
  else
    echo "the member could not write $d/cgroup.procs"
  fi
' || echo "the probe failed"

rmdir "$member/probe/child" 2>/dev/null || true
rmdir "$member/probe" 2>/dev/null || true
