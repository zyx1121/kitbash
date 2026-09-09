```
██╗  ██╗██╗████████╗██████╗  █████╗ ███████╗██╗  ██╗
██║ ██╔╝██║╚══██╔══╝██╔══██╗██╔══██╗██╔════╝██║  ██║
█████╔╝ ██║   ██║   ██████╔╝███████║███████╗███████║
██╔═██╗ ██║   ██║   ██╔══██╗██╔══██║╚════██║██╔══██║
██║  ██╗██║   ██║   ██████╔╝██║  ██║███████║██║  ██║
╚═╝  ╚═╝╚═╝   ╚═╝   ╚═════╝ ╚═╝  ╚═╝╚══════╝╚═╝  ╚═╝
                                                    
```

# kitbash

> An operating system for AI agents. Files, Packages, Processes, Telemetry. Nothing built for a human at a terminal.

An organization installs kitbash on one machine. Every member gets a Linux user and connects their agent over SSH to an MCP endpoint that exposes the whole system. The agent writes source into Files, builds it into a Package, runs it as a Process, and everything it does lands in Telemetry. Third party tools arrive as Packages with schemas, never as host installs.

## Status

Version 2 hardening complete (v0.7.0): narrowed Process permits, enforced limits, shared images, backups, e2e in CI; what stays open is in PLAN.md 5.5. Read [PLAN.md](PLAN.md): positioning, the four object model, kits, kitbashOS, and version 1 milestones. Machine readable definitions live in [`spec/`](spec/).

## Install

**Prerequisites.** One Proxmox VE virtual machine, not an LXC container: nested
container runtimes inside LXC are unreliable and rootless podman is how every
Process runs (PLAN.md 5.4). The Alpine cloud image install.sh was written for,
3.23 at the time of writing, 4 cores, 4 GB of memory, 40 GB of disk. Nothing
else is installed on the host: install.sh puts components 2 to 6 of PLAN.md 4.2
there and everything third party arrives as a Package.

**1. Build the apk.** On any Alpine host with `alpine-sdk` and `go`, as a non
root user (abuild refuses root):

```sh
git archive --format=tar.gz --prefix=kitbash-0.6.2/ -o kitbash-0.6.2.tar.gz v0.6.2
sh packaging/apk/build.sh kitbash-0.6.2.tar.gz
```

The tarball must be named `kitbash-<pkgver>.tar.gz` for the `pkgver` in
`packaging/apk/APKBUILD`. On its first run build.sh creates a signing key with
`abuild-keygen -a -n -q`; the package lands in `~/packages/`. Copy the package
and the public half of the key to the kitbash host:

```sh
scp ~/packages/*/*/kitbashd-0.6.2-r0.apk ~/.abuild/*.rsa.pub root@kitbash.example.org:
```

**2. Install the host.** As root on the kitbash machine. The key goes in first,
so apk verifies the signature it was built with:

```sh
cp *.rsa.pub /etc/apk/keys/
apk add kitbashd-0.6.2-r0.apk
KITBASH_DNS=1.1.1.1 sh /usr/share/kitbash/install.sh
```

install.sh is idempotent. It writes `/etc/resolv.conf` when the cloud image left
it empty, installs the host packages, sets cgroups v2 unified mode and the `tun`
and `fuse` modules, creates the `kitbash-users` and `kitbash-admin` groups,
removes the cloud image's `alpine` user, enables kitbashd, seeds `/org` from
`/usr/share/kitbash/org`, and writes the sshd rule that gives every member the
MCP surface and no shell. It configures no firewall: kitbashd also listens on
TCP 4318 for Processes, and scoping that port is the operator's.

**3. Create the first admin.** On the console, as root:

```sh
kitbash-adduser alice 'ssh-ed25519 AAAA... alice@laptop' admin
```

That admin creates every other member through the surface with `users_create`.
`kitbash-adduser` stays as the bootstrap and break glass path.

**4. Connect an agent.** On the member's own machine:

```sh
claude mcp add kitbash -- ssh alice@kitbash.example.org
```

**Verify.** On the host:

```sh
kitbash-mcp --version
rc-service kitbashd status
```

From the member's machine, one MCP session that lists the surface:

```sh
{ printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"check","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'; sleep 2; } \
  | ssh alice@kitbash.example.org | grep -o '"name":"[a-z]*_[a-z_]*"' | wc -l
```

22, the whole built in surface: 4 `fs`, 4 `pkg`, 4 `proc`, 2 `tel`, 5 `users`
and 3 `approvals`. Each running Process with `expose: mcp` adds its own tools on
top of those. The `sleep` holds stdin open long enough for the answer; a session
whose stdin closes first is a session that ends before it replies.

**Upgrade.** Install the new package and run install.sh again:

```sh
apk add kitbashd-<newver>-r0.apk
sh /usr/share/kitbash/install.sh
```

install.sh restarts kitbashd, and kitbashd restores Processes at every start: it
runs each registered container again as its owner, so an upgrade brings
Processes back the way a reboot does.

**Where state lives.**

| Path | What is there |
|------|---------------|
| `/org/<folder>` | Shared Files, one git repository per top level folder, group `kitbash-admin` |
| `/home/<member>` | A member's Files, a git repository, mode 0700 |
| `/var/lib/kitbash/kitbashd.db` | Telemetry, Process registrations, approvals and settings. Root only |
| `/var/log/kitbashd.log` | The daemon's log, both streams of the OpenRC service |
| `/org/.archive/<member>` | The home of a removed member, root only and invisible to the surface |

Built images live in each member's own rootless container store; the image
labels are the build history.

**What needs the daemon.** `fs`, `pkg` and `proc` work without kitbashd: the
Telemetry those calls record is dropped and one line goes to the session log.
`tel_query`, `tel_retention`, the `users` family, the `approvals` family, a
member's write to `/org`, the Process token and fan out secret, `/mcp` for
Processes and boot restore all need it running.

## Development

Go 1.26 and git are the only requirements. `make build` produces two static
binaries: `bin/kitbash-mcp`, which sshd runs as the connecting user through
`ForceCommand`, and `bin/kitbashd`, the resident daemon OpenRC starts at boot.

| Target | What it does |
|--------|--------------|
| `make build` | Static binaries for this host in `bin/kitbash-mcp` and `bin/kitbashd` |
| `make test` | `go test ./...`, including the MCP surface over an in memory transport |
| `make lint` | `gofmt -l` and `go vet ./...` |
| `make build-linux` | `bin/kitbash-mcp-linux-amd64` and `bin/kitbashd-linux-amd64`, the binaries a kitbash host runs |

The server serves the roots `/org` and `/home/<user>`. Set `KITBASH_ROOTS` to a
colon separated list of absolute paths to point it somewhere else, which is how
the tests and a local run against a fixture tree work:

```
KITBASH_ROOTS=/tmp/fixture/org:/tmp/fixture/home/tester bin/kitbash-mcp
```

Telemetry is exported to kitbashd over `/run/kitbash/kitbashd.sock`, and
`KITBASH_SOCKET` points it somewhere else under the same rule: both overrides
are ignored inside an SSH session, so a member never chooses either. Without a
daemon on the socket the whole surface still works; the records are dropped and
one line goes to the server log for the session.

kitbashd is the other end of that socket. It stores Telemetry in
`/var/lib/kitbash/kitbashd.db`, and `KITBASH_SOCKET` and `KITBASH_STORE` move
both somewhere writable, which is how a local run outside a kitbash host works:

```
KITBASH_SOCKET=/tmp/kitbashd.sock KITBASH_STORE=/tmp/kitbashd.db bin/kitbashd
```

## License

Private, all rights reserved.
