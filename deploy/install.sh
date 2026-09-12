#!/bin/sh
# kitbash host install. Idempotent: run it as often as you like.
# Turns a fresh Alpine VM into a kitbash host. Expects the kitbashd apk to be
# reachable as $KITBASH_APK (local path) or already installed.
set -eu

log() { printf '\033[1;36m[kitbash]\033[0m %s\n' "$*"; }
need_root() { [ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }; }
need_root

# 1. DNS. cloud-init on the Alpine cloud image does not write resolv.conf.
if [ ! -s /etc/resolv.conf ]; then
  log "writing /etc/resolv.conf"
  printf 'nameserver %s\nnameserver 8.8.8.8\n' "${KITBASH_DNS:-1.1.1.1}" > /etc/resolv.conf
fi

# 2. Host packages. Exactly the seven components plus their runtime deps.
log "installing host packages"
apk add -q --no-progress podman crun passt fuse-overlayfs shadow shadow-subids git openssh qemu-guest-agent curl

# 3. cgroups v2 and kernel modules for rootless podman. kitbashd owns
#    /sys/fs/cgroup/kitbash: it creates one root owned ceiling cgroup per
#    Process under the member's cgroup, writes the manifest's limits into it,
#    and starts the container beneath it, which is what makes those limits an
#    enforcement rather than a record. Nothing here creates any of it.
sed -i 's/^#\?rc_cgroup_mode=.*/rc_cgroup_mode="unified"/' /etc/rc.conf
grep -q '^rc_cgroup_mode="unified"' /etc/rc.conf || echo 'rc_cgroup_mode="unified"' >> /etc/rc.conf
for m in tun fuse; do grep -qx "$m" /etc/modules || echo "$m" >> /etc/modules; modprobe "$m" 2>/dev/null || true; done
# sysinit, not boot: kitbashd depends on the cgroups service, and a service in
# the default runlevel can only need one that is already up by then.
rc-update -q add cgroups sysinit 2>/dev/null || true
rc-update -q del cgroups boot 2>/dev/null || true
rc-service -q cgroups start 2>/dev/null || true
rc-update -q add qemu-guest-agent default 2>/dev/null || true
rc-service -q qemu-guest-agent start 2>/dev/null || true

# 4. Boot time prerequisites that live in tmpfs. The kitbashd apk ships them
#    as /usr/share/kitbash/rootless-prereqs.sh and kitbashd.initd runs that
#    from start_pre, so the daemon no longer waits for local (issue #88). The
#    local.d hook stays, calling the same script, for a host installed before
#    that change; on a host that has both, running it twice changes nothing.
cat > /etc/local.d/kitbash-rootless.start <<'S'
#!/bin/sh
# Rootless podman prerequisites. kitbashd.initd runs this same script from
# start_pre; this hook is what a host installed before that had instead.
[ -x /usr/share/kitbash/rootless-prereqs.sh ] || exit 0
exec /usr/share/kitbash/rootless-prereqs.sh
S
chmod +x /etc/local.d/kitbash-rootless.start
rc-update -q add local default 2>/dev/null || true
/etc/local.d/kitbash-rootless.start

# 5. Groups. kitbash-users get the MCP ForceCommand, kitbash-admin may also manage.
getent group kitbash-users >/dev/null || addgroup -S kitbash-users
getent group kitbash-admin >/dev/null || addgroup -S kitbash-admin

# 6. The cloud image default user is a human convenience. Remove it.
if id alpine >/dev/null 2>&1; then
  log "removing default user alpine"
  deluser --remove-home alpine >/dev/null 2>&1 || true
fi

# 7. kitbashd apk.
if [ -n "${KITBASH_APK:-}" ] && [ -f "$KITBASH_APK" ]; then
  log "installing $KITBASH_APK"
  apk add -q --no-progress --allow-untrusted "$KITBASH_APK"
fi
command -v kitbash-mcp >/dev/null || log "warning: kitbash-mcp not installed yet; sshd ForceCommand will fail until it is"

# 8. kitbashd: the OTLP receiver and Telemetry store. It owns the socket every
#    member session exports to, so it starts before sshd is reconfigured.
if command -v kitbashd >/dev/null && [ -x /etc/init.d/kitbashd ]; then
  log "enabling kitbashd"
  rc-update -q add kitbashd default 2>/dev/null || true
  rc-service -q kitbashd restart
  for _ in 1 2 3 4 5; do [ -S /run/kitbash/kitbashd.sock ] && break; sleep 1; done
  [ -S /run/kitbash/kitbashd.sock ] || log "warning: kitbashd did not create /run/kitbash/kitbashd.sock; see /var/log/kitbashd.log"
else
  log "warning: kitbashd not installed yet; Telemetry is unavailable until it is"
fi

# 9. /org: shared and root owned. Every top level folder under it is its own
#    git repository (PLAN.md 2.1); /org itself is a plain directory.
mkdir -p /org
seed="${KITBASH_ORG_SEED:-/usr/share/kitbash/org}"
if [ -d "$seed" ]; then
  for src in "$seed"/*/; do
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
fi
#    /org is group kitbash-admin and setgid, so what one admin adds stays
#    writable by the next, and every repository is shared at group level so
#    two admins committing do not fight over object permissions
#    (PLAN.md 2.1). Members read it; only admins write it, and a member's
#    write to /org is an approval rather than a permission.
#    /org/.archive holds the homes of removed members and stays root only:
#    it carries no kitbash.yaml, so the surface cannot see it either.
prune=""
[ -d /org/.archive ] && prune="-path /org/.archive -prune -o"
# shellcheck disable=SC2086
find /org $prune -exec chown -h root:kitbash-admin {} + 2>/dev/null || true
# shellcheck disable=SC2086
find /org $prune -type d -exec chmod 2775 {} +
# g+w,o+r rather than a fixed mode: a file in /org may be a script, and 664
# would take its executable bit away.
# shellcheck disable=SC2086
find /org $prune -type f -exec chmod g+w,o+r {} +
for repo in /org/*/.git; do
  [ -d "$repo" ] || continue
  git -C "$(dirname "$repo")" config core.sharedRepository group
done
if [ -d /org/.archive ]; then
  chown root:root /org/.archive
  chmod 700 /org/.archive
fi

# 10. sshd: regular users get the MCP surface and nothing else.
#     Port 22 is the only port this installer configures. kitbashd also opens
#     TCP 4318, the Process receiver: rootless containers reach the host there
#     and export Telemetry with the token kitbashd minted for them. Every
#     request on it needs that token, but the port is bound on every address
#     because host.containers.internal resolves to the host's primary one, so
#     scope it in the host firewall to the container network and whatever else
#     must reach it. kitbash installs no firewall; that is the operator's.
cat > /etc/ssh/sshd_config.d/60-kitbash.conf <<'S'
# kitbash: SSH is the MCP transport. Members never get a shell.
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
Match Group kitbash-users
    ForceCommand /usr/bin/kitbash-mcp
    PermitTTY no
    AllowTcpForwarding no
    AllowAgentForwarding no
    X11Forwarding no
    PermitTunnel no
S
sshd -t
rc-service -q sshd restart

log "done. create the first admin on this console with: kitbash-adduser <name> '<ssh public key>' admin"
log "that admin creates every other member through the MCP surface with users_create; kitbash-adduser stays as the bootstrap and break glass path"
log "the admin argument puts the member in kitbash-admin, which users_create, approvals and reading another member's Telemetry all require"
