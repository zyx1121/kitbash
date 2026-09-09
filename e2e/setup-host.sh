#!/bin/sh
# The kitbash host the end to end job drives, built on a GitHub runner.
#
# deploy/install.sh is the operator's path and it is written for Alpine: apk,
# OpenRC and sshd. The runner is Ubuntu with podman already on it, so this
# replicates only the parts the surface depends on, in install.sh's own order
# and with its ownership model:
#
#   the kitbash-users and kitbash-admin groups
#   two members through deploy/kitbash-adduser, which is the same account,
#     subordinate id block and runtime directory that users_create makes
#   /org seeded from deploy/org, root:kitbash-admin and 2775, one git
#     repository per top level folder, shared at group level
#   kitbash-mcp at /usr/bin/kitbash-mcp, which is where a session runs it from
#
# It installs no sshd rule: these tests run kitbash-mcp through sudo rather
# than over SSH, so the ForceCommand has nothing to serve here.
#
# Run as root from the repository root. KITBASH_BIN names the directory holding
# the two built binaries.
set -eu

log() { printf '\033[1;36m[e2e]\033[0m %s\n' "$*"; }
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }

bin=${KITBASH_BIN:-bin}
admin=${KITBASH_E2E_ADMIN:-admin1}
member=${KITBASH_E2E_MEMBER:-member1}
key=${KITBASH_E2E_SSH_KEY:?KITBASH_E2E_SSH_KEY must name a public key file}
if [ -f "$key" ]; then key=$(cat "$key"); fi

# 1. What rootless podman needs and the runner may not carry. podman itself is
#    preinstalled; the rest is installed only when it is missing, because an
#    apt-get update costs more than this whole job.
missing=""
for pair in "newuidmap uidmap" "fuse-overlayfs fuse-overlayfs" "crun crun" "curl curl" "git git"; do
  command -v "${pair%% *}" >/dev/null 2>&1 || missing="$missing ${pair##* }"
done
if [ -n "$missing" ]; then
  log "installing$missing"
  # shellcheck disable=SC2086
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq $missing >/dev/null
fi

# 2. Unprivileged user namespaces, which is what a rootless container is made
#    of. Ubuntu's AppArmor restricts them by profile; a member here has no
#    profile, so the restriction is lifted for the job.
sysctl -q -w kernel.apparmor_restrict_unprivileged_userns=0 2>/dev/null || true
sysctl -q -w kernel.unprivileged_userns_clone=1 2>/dev/null || true
mount --make-rshared / 2>/dev/null || true

# 3. No systemd user session on a runner, so the runtime manages cgroups
#    itself. install.sh does not write this file: an Alpine host runs OpenRC
#    and podman reads the same default there.
mkdir -p /etc/containers
cat > /etc/containers/containers.conf <<'CONF'
[engine]
cgroup_manager = "cgroupfs"
events_logger = "file"
CONF

# 4. The binaries. /usr/bin/kitbash-mcp is the path sshd's ForceCommand and
#    kitbashd's own MCP sessions both name.
install -m 0755 "$bin/kitbash-mcp-linux-amd64" /usr/bin/kitbash-mcp
install -m 0755 "$bin/kitbashd-linux-amd64" /usr/bin/kitbashd
log "installed $(/usr/bin/kitbash-mcp --version) at /usr/bin/kitbash-mcp"

# 5. Groups. kitbash-users get the surface, kitbash-admin may also manage.
getent group kitbash-users >/dev/null || groupadd -r kitbash-users
getent group kitbash-admin >/dev/null || groupadd -r kitbash-admin

# 6. The two members, through the operator's own script.
#
#    shadow locks /etc/subuid with a file of that name plus .lock, which is the
#    same name kitbash's own allocation takes its lock on, and shadow refuses a
#    lock file it did not write ("existing lock file without a PID"). The
#    allocation leaves its file behind, so the next useradd would refuse to
#    run. Clearing it between accounts is this job's workaround, not a rule of
#    the platform: on the host the two never run this close together.
unlock_subids() { rm -f /etc/subuid.lock /etc/subgid.lock; }

unlock_subids
sh deploy/kitbash-adduser "$admin" "$key" admin
unlock_subids
sh deploy/kitbash-adduser "$member" "$key"
#    users_create runs the same useradd from inside kitbashd, so the files are
#    cleared here too rather than only between these two.
unlock_subids

# 6b. /run/user/<uid> is where rootless podman keeps its state, and on this
#     host nothing but this loop makes it. A kitbash host runs no logind:
#     install.sh writes /etc/local.d/kitbash-rootless.start, which makes the
#     directory for every member at boot, and this is that step.
#
#     A member here must stay a user logind knows nothing about. Given a
#     session, logind removes this directory as soon as the session ends, and
#     given a lingering one it starts a systemd user manager, which rootless
#     podman then asks for a scope to keep its pause process in: that scope is
#     outside the cgroup tree kitbashd delegates, and a container started from
#     it cannot be moved into its own cgroup. The sessions the accounts were
#     created through are ended here, and every later command runs through
#     setpriv, which opens none.
if command -v loginctl >/dev/null 2>&1; then
  for m in "$admin" "$member"; do loginctl terminate-user "$m" >/dev/null 2>&1 || true; done
  sleep 2
fi
for m in "$admin" "$member"; do
  uid=$(id -u "$m")
  mkdir -p "/run/user/$uid"
  chown "$m" "/run/user/$uid"
  chmod 700 "/run/user/$uid"
done

# 7. /org, seeded and owned as install.sh leaves it.
mkdir -p /org
for src in deploy/org/*/; do
  [ -d "$src" ] || continue
  name=$(basename "$src")
  if [ ! -d "/org/$name/.git" ]; then
    log "seeding /org/$name"
    mkdir -p "/org/$name"
    cp -R "$src". "/org/$name/"
    git -C "/org/$name" init -q --initial-branch=main
    git -C "/org/$name" -c user.name=root -c user.email=root@kitbash add -A
    git -C "/org/$name" -c user.name=root -c user.email=root@kitbash commit -q -m "Seed $name" || true
  fi
done
find /org -exec chown -h root:kitbash-admin {} + 2>/dev/null || true
find /org -type d -exec chmod 2775 {} +
find /org -type f -exec chmod g+w,o+r {} +
for repo in /org/*/.git; do
  [ -d "$repo" ] || continue
  git -C "$(dirname "$repo")" config core.sharedRepository group
done

log "host ready: $admin is an admin, $member is a member, /org holds $(ls /org | wc -l) folders"
