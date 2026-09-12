#!/bin/sh
# What the host looked like when a step failed: the cgroup tree kitbashd built,
# who owns every file in it, and where each of a member's processes is placed.
# The delegation is what a Process's container is started under, so this is the
# first thing to read when a run or an exec was refused.
#
#   collect.sh <member> [member...]
set -u

echo "== the kitbash cgroup tree"
find /sys/fs/cgroup/kitbash -maxdepth 4 \
  \( -name 'cgroup.procs' -o -name 'cgroup.subtree_control' -o -name 'memory.max' -o -type d \) \
  -printf '%M %u:%g %p\n' 2>/dev/null | sort -k3

echo
echo "== controllers"
for f in /sys/fs/cgroup/cgroup.subtree_control /sys/fs/cgroup/kitbash/cgroup.subtree_control; do
  [ -f "$f" ] && printf '%s: %s\n' "$f" "$(cat "$f")"
done

for name in "$@"; do
  echo
  echo "== can $name move a process the way a container runtime does"
  sh "$(dirname "$0")/cgroup-probe.sh" "$name"

  echo
  echo "== the processes of $name"
  for pid in $(pgrep -u "$name" 2>/dev/null); do
    printf '%s %s %s\n' "$pid" \
      "$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null | cut -c1-120)" \
      "$(cut -d: -f3 "/proc/$pid/cgroup" 2>/dev/null | head -1)"
  done
done
