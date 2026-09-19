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

# 2. Host packages. Exactly the seven components plus their runtime deps, and
#    nftables, which is the kernel's own packet filter and what step 11 writes
#    one ruleset with.
log "installing host packages"
apk add -q --no-progress podman crun passt fuse-overlayfs shadow shadow-subids git openssh qemu-guest-agent curl nftables

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
#    Step 7 is where the apk arrives, so on a host that does not have it yet
#    the shared script is not here to call and this run does the same two
#    things itself. From the next boot on it is the daemon's job either way.
if [ -x /usr/share/kitbash/rootless-prereqs.sh ]; then
  /usr/share/kitbash/rootless-prereqs.sh
else
  log "kitbashd apk not installed yet; making / rshared and the /run/user directories here"
  mount --make-rshared / || log "warning: mount --make-rshared / failed; rootless containers may not see their own mounts"
  for u in $(awk -F: '$3>=1000 && $3<65534 {print $1":"$3}' /etc/passwd); do
    name=${u%%:*}; uid=${u##*:}
    d=/run/user/$uid
    { mkdir -p "$d" && chown "$name" "$d" && chmod 700 "$d"; } || log "warning: could not prepare $d"
  done
fi

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

# 7b. The host's own domain, which is what turns the reverse proxy on. With one,
#     every Process with expose: http is served at
#     https://<name>.<member>.<domain>, or at the hostname its unit declared,
#     and kitbashd is the reverse proxy in front of it (PLAN.md 2.3). Without
#     one nothing is routed and an http Process keeps its internal port, which
#     is how kitbash worked before the proxy existed.
#
#     KITBASH_TLS decides who holds the certificate. acme, the default with a
#     domain, has kitbashd obtain one per host name, which needs 80 and 443
#     reachable from the internet. gateway has it listen on 80 alone and trust
#     the gateway in front of it, which is how a host behind one public address
#     is put on the internet.
#
#     The settings are written to /etc/conf.d/kitbashd, which the OpenRC
#     service exports, so a domain given once survives every boot and every
#     upgrade. A variable that is not in this run's environment leaves the file
#     alone: running install.sh again without KITBASH_DOMAIN does not take the
#     domain away, and KITBASH_DOMAIN= does.
confd=/etc/conf.d/kitbashd
set_confd() {
  [ -f "$confd" ] || : > "$confd"
  # The key is replaced wherever it is, commented out or not, and written
  # once at the end, so this is the same file after two runs as after one.
  grep -v -E "^[[:space:]]*#?[[:space:]]*$1=" "$confd" > "$confd.kitbash-new" || true
  printf '%s="%s"\n' "$1" "$2" >> "$confd.kitbash-new"
  mv "$confd.kitbash-new" "$confd"
}
confd_value() {
  [ -f "$confd" ] || return 0
  sed -n "s/^[[:space:]]*$1=//p" "$confd" | tail -n 1 | sed 's/^"//; s/"$//'
}
if [ -n "${KITBASH_DOMAIN+set}" ]; then
  log "writing KITBASH_DOMAIN to $confd"
  set_confd KITBASH_DOMAIN "$KITBASH_DOMAIN"
fi
if [ -n "${KITBASH_TLS+set}" ]; then
  log "writing KITBASH_TLS to $confd"
  set_confd KITBASH_TLS "$KITBASH_TLS"
fi
domain=$(confd_value KITBASH_DOMAIN)
tls=$(confd_value KITBASH_TLS)
if [ -n "$domain" ] && [ -z "$tls" ]; then
  # A host with a domain and nothing said about TLS obtains its own
  # certificates, which is the default PLAN.md 2.3 names.
  set_confd KITBASH_TLS acme
  tls=acme
fi
if [ -n "$domain" ]; then
  case "$tls" in
    acme|gateway) ;;
    *) echo "kitbash: KITBASH_TLS is \"$tls\"; it is acme or gateway" >&2; exit 1 ;;
  esac
  log "serving http Processes under $domain, TLS $tls"
fi

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
#     Port 22 is one of the two this installer configures. The other is TCP
#     4318, the Process receiver kitbashd opens: rootless containers reach the
#     host there and export Telemetry with the token kitbashd minted for them.
#     Every request on it needs that token, but the port is bound on every
#     address because host.containers.internal resolves to the host's primary
#     one, so step 11 scopes who may open a connection to it.
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

# 11. Firewall: the Process receiver on TCP 4318 and nothing else. kitbashd
#     binds it on every address because rootless podman delivers
#     host.containers.internal to the host's primary address and not to
#     loopback, so the port is scoped here rather than left to the operator
#     (issue #90). Every other port stays as it was: the input policy is
#     accept and 4318 is the only port this ruleset decides.
#
#     Where a Process reaches the receiver from was measured on a kitbash host
#     rather than guessed: Alpine 3.23, podman 5.7, the default rootless
#     network command pasta. A container that curls
#     http://host.containers.internal:4318 arrives on lo with the host's own
#     primary address as both source and destination
#     (tcpdump: "lo In IP 10.10.10.115.38060 > 10.10.10.115.4318"), because
#     pasta gives the container the host's address and the kernel routes a
#     packet to the host's own address through loopback. So the interface is
#     what the ruleset decides on and not the address: an address can be
#     spoofed from the wire, and a host that later takes this one over DHCP
#     would inherit the permission with it. slirp4netns arrives the same way,
#     over loopback from its host side process.
#
#     Written to a temporary file first and checked with nft -c, so a ruleset
#     that does not parse never becomes /etc/nftables.nft and a failed load
#     leaves the running one alone. The directory the ruleset includes is made
#     before the check, because a check is worth nothing if it passes on a
#     path that is not there yet.
log "writing the nftables ruleset"
mkdir -p /etc/nftables.d
cat > /etc/nftables.nft.kitbash-new <<'NFT'
#!/usr/sbin/nft -f
# kitbash owns this file and deploy/install.sh rewrites it on every run.
# Put operator rules in /etc/nftables.d/<name>.nft: the include at the end
# loads them after this table and install.sh never touches them.
#
# One port is scoped here, TCP 4318, the Process receiver kitbashd binds on
# every address. A host that was given a domain also names 80, and in acme mode
# 443, which is where kitbashd serves the reverse proxy of every http Process.
# Everything else is as it was: the policy is accept and no other port is
# decided. This is not a host firewall.

# Replace this table and only this table. No flush ruleset: whatever else
# holds rules on this host, a container runtime among them, stays.
table inet kitbash
delete table inet kitbash

table inet kitbash {
	chain input {
		type filter hook input priority filter; policy accept;

		# SSH first, before any line in this file can drop a packet. The
		# way back into the host never depends on a rule further down.
		tcp dport 22 accept comment "SSH, the MCP transport and the way in"
NFT
#     The reverse proxy, and only on a host that has a domain. Without one
#     kitbashd binds neither port, and a rule naming a listener that does not
#     exist is a rule nobody can check. 443 is acme's alone: in gateway mode
#     the gateway in front of this host holds the certificate and forwards
#     to 80.
if [ -n "$domain" ]; then
  cat >> /etc/nftables.nft.kitbash-new <<'NFT'

		# The reverse proxy of every http Process, see PLAN.md 2.3.
		tcp dport 80 accept comment "The reverse proxy"
NFT
  if [ "$tls" = acme ]; then
    cat >> /etc/nftables.nft.kitbash-new <<'NFT'
		tcp dport 443 accept comment "The reverse proxy, which holds this host's own certificates"
NFT
  fi
fi
cat >> /etc/nftables.nft.kitbash-new <<'NFT'

		# This also carries every Process to the receiver below: a
		# rootless container reaches it as host.containers.internal,
		# which pasta delivers over loopback.
		iif lo accept comment "Whatever the host says to itself, Processes included"

		ct state { established, related } accept comment "Answers to what this host asked for"
		ip protocol icmp accept comment "ICMP"
		ip6 nexthdr icmpv6 accept comment "ICMPv6, which IPv6 needs to work at all"

		# The Process receiver, off the wire. Loopback was accepted
		# above, so what reaches this line came in on a real interface
		# and is not a Process of this host, whatever source address it
		# claims. The token kitbashd minted still decides whose records
		# the accepted ones are; this decides who may open a connection.
		tcp dport 4318 drop comment "The receiver is for this host's Processes only"
	}
}

# Operator rules, loaded last. An empty directory is not an error.
include "/etc/nftables.d/*.nft"
NFT
if nft -c -f /etc/nftables.nft.kitbash-new; then
  mv /etc/nftables.nft.kitbash-new /etc/nftables.nft
else
  rm -f /etc/nftables.nft.kitbash-new
  echo "kitbash: the generated nftables ruleset did not parse; /etc/nftables.nft is unchanged and 4318 is open" >&2
  exit 1
fi
#     boot, not default: the ruleset is up before the network is, and before
#     kitbashd opens the receiver in the default runlevel. The service loads
#     /etc/nftables.nft, so loading it through the service rather than with
#     nft is what makes OpenRC's idea of the state the running one. reload on
#     a started service is the same load without the flush a restart does.
rc-update -q add nftables boot 2>/dev/null || true
if rc-service -q nftables status >/dev/null 2>&1; then
  action=reload
else
  action=start
fi
#     A host whose ruleset did not load has the receiver open to the network,
#     which is the thing this step exists to prevent, so this is the one step
#     that ends the run rather than warning. Everything before it is done and
#     install.sh is idempotent: fix the cause and run it again.
if rc-service -q nftables "$action"; then
  if [ -n "$domain" ]; then
    log "nftables: 4318 is reachable over loopback only, and the reverse proxy is open"
  else
    log "nftables: 4318 is reachable over loopback only"
  fi
else
  echo "kitbash: loading /etc/nftables.nft failed; the ruleset that was running is still running and 4318 may be open" >&2
  exit 1
fi

log "done. create the first admin on this console with: kitbash-adduser <name> '<ssh public key>' admin"
log "that admin creates every other member through the MCP surface with users_create; kitbash-adduser stays as the bootstrap and break glass path"
log "the admin argument puts the member in kitbash-admin, which users_create, approvals and reading another member's Telemetry all require"
