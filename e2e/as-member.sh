#!/bin/sh
# Run one command as a member, from inside that member's cgroup.
#
#   as-member.sh <member> <command> [argument...]
#
# The environment is the one a session gets and nothing of the job's: HOME,
# USER, LOGNAME, PATH and the XDG_RUNTIME_DIR rootless podman needs. The Go
# driver builds the same environment for kitbash-mcp.
#
# The cgroup is the part that is not decoration. kitbashd starts every Process
# under /sys/fs/cgroup/kitbash/<member>/<process id> and podman exec has to move
# the new process into that container's cgroup, which the kernel allows only
# with write access to the common ancestor of both. A process the job started
# from the runner's own cgroup shares nothing with it but the root cgroup, so
# the exec is refused. A member session does not hit this because kitbash-mcp
# calls POST /kitbash/v1/sessions/join at start; this script does the same thing
# by hand, which is why it needs root before it drops to the member.
#
# A host whose cgroup tree kitbashd could not build has no leaf to join. The
# command still runs: the Processes there are unplaced too, and the exec that
# would have been refused is then an exec into a container in the same place.
set -eu

name=${1:?usage: as-member.sh <member> <command> [argument...]}
shift
[ $# -gt 0 ] || { echo "as-member.sh: no command" >&2; exit 1; }

# Root is needed to join the cgroup, so a call that is not root becomes one.
if [ "$(id -u)" != 0 ]; then
  exec sudo -n /bin/sh "$0" "$name" "$@"
fi

leaf=/sys/fs/cgroup/kitbash/$name/run
if [ -f "$leaf/cgroup.procs" ]; then
  if ! echo $$ > "$leaf/cgroup.procs" 2>/dev/null; then
    echo "as-member.sh: could not join $leaf" >&2
  fi
else
  echo "as-member.sh: $leaf does not exist; running outside the member's cgroup" >&2
fi

home=$(getent passwd "$name" | cut -d: -f6)
uid=$(id -u "$name")
gid=$(id -g "$name")

# setpriv rather than another sudo: it execs the command in this process, so
# the cgroup joined above is the one the command runs in.
exec setpriv --reuid "$uid" --regid "$gid" --init-groups \
  env -i \
  HOME="$home" \
  USER="$name" \
  LOGNAME="$name" \
  PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  XDG_RUNTIME_DIR="/run/user/$uid" \
  TMPDIR=/tmp \
  "$@"
