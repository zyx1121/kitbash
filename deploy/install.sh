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

# 3. cgroups v2 and kernel modules for rootless podman.
sed -i 's/^#\?rc_cgroup_mode=.*/rc_cgroup_mode="unified"/' /etc/rc.conf
grep -q '^rc_cgroup_mode="unified"' /etc/rc.conf || echo 'rc_cgroup_mode="unified"' >> /etc/rc.conf
for m in tun fuse; do grep -qx "$m" /etc/modules || echo "$m" >> /etc/modules; modprobe "$m" 2>/dev/null || true; done
rc-update -q add cgroups boot 2>/dev/null || true
rc-service -q cgroups start 2>/dev/null || true
rc-update -q add qemu-guest-agent default 2>/dev/null || true
rc-service -q qemu-guest-agent start 2>/dev/null || true

# 4. Boot time prerequisites that live in tmpfs.
cat > /etc/local.d/kitbash-rootless.start <<'S'
#!/bin/sh
# Rootless podman prerequisites that do not survive a reboot.
mount --make-rshared / 2>/dev/null || true
for u in $(awk -F: '$3>=1000 && $3<65534 {print $1":"$3}' /etc/passwd); do
  name=${u%%:*}; uid=${u##*:}
  d=/run/user/$uid; mkdir -p "$d"; chown "$name" "$d"; chmod 700 "$d"
done
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
chown -R root:kitbash-users /org
chmod 750 /org
find /org -type d -exec chmod 750 {} +
find /org -type f -exec chmod 640 {} +

# 10. sshd: regular users get the MCP surface and nothing else.
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

log "done. add members with: kitbash-adduser <name> '<ssh public key>' [admin]"
log "the admin argument puts the member in kitbash-admin, which tel_retention and reading another member's Telemetry both require"
