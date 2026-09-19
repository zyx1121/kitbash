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

v0.12.1: the first release built and published by CI. The version 1 loop (PLAN.md 5.3, M1 to M6) and the hardening that followed it (narrowed Process permits, enforced limits, shared images, backups, an end to end job in CI) are in. What stays open is in PLAN.md 5.5. Read [PLAN.md](PLAN.md): positioning, the four object model, kits, kitbashOS, releases, and version 1 milestones. Machine readable definitions live in [`spec/`](spec/).

## Install

**Prerequisites.** One Proxmox VE virtual machine, not an LXC container: nested
container runtimes inside LXC are unreliable and rootless podman is how every
Process runs (PLAN.md 5.4). The Alpine cloud image install.sh was written for,
3.23 at the time of writing, 4 cores, 4 GB of memory, 40 GB of disk. Nothing
else is installed on the host: install.sh puts components 2 to 6 of PLAN.md 4.2
there and everything third party arrives as a Package.

**1. Get the release.** Every release carries, for x86_64 and for aarch64, the
apk, the ISO and the two static binaries, plus the one signing public key and
one `SHA256SUMS`. Each asset is named for its architecture. The repository is
public, so plain `curl` is enough:

```sh
# This runs on the workstation, not on the kitbash host. A Mac's uname -m
# says arm64 and the release says aarch64; set arch by hand when the host is
# not the architecture of the machine doing the downloading.
case $(uname -m) in arm64) arch=aarch64 ;; *) arch=$(uname -m) ;; esac
base=https://github.com/zyx1121/kitbash/releases/download/v0.12.1
apk=kitbashd-0.12.1-r0.$arch.apk
pub=builder-6a9c3ef1.rsa.pub
curl -fLO "$base/$apk"
curl -fLO "$base/$pub"
curl -fLO "$base/SHA256SUMS"
grep -F -e "$apk" -e "$pub" SHA256SUMS | sha256sum -c
scp "$apk" "$pub" root@kitbash.example.org:
```

`SHA256SUMS` covers every file on the release, so the `grep` narrows it to the
two that were downloaded. That is what makes the check portable: busybox
`sha256sum`, which is the one on an Alpine host, has no `--ignore-missing` and
plain `-c` fails on the seven files that are not there. Two lines ending in
`OK` is the whole check. Anything else means the download is not what CI built,
and the apk is the wrong thing to install.

To build the apk yourself instead, on an Alpine host of the architecture you
want, with `alpine-sdk` and `go`, as a non root user: `git archive --format=tar.gz --prefix=kitbash-0.12.1/ -o kitbash-0.12.1.tar.gz v0.12.1`
then `sh packaging/apk/build.sh kitbash-0.12.1.tar.gz`. The package lands in
`~/packages/`, signed with the key build.sh creates on its first run.

**2. Install the host.** As root on the kitbash machine. The key goes in first,
so apk verifies the signature it was built with:

```sh
cp *.rsa.pub /etc/apk/keys/
apk add kitbashd-0.12.1-r0."$(uname -m)".apk
KITBASH_DNS=1.1.1.1 sh /usr/share/kitbash/install.sh
```

install.sh is idempotent. It writes `/etc/resolv.conf` when the cloud image left
it empty, installs the host packages, sets cgroups v2 unified mode and the `tun`
and `fuse` modules, creates the `kitbash-users` and `kitbash-admin` groups,
removes the cloud image's `alpine` user, enables kitbashd, seeds `/org` from
`/usr/share/kitbash/org`, writes the sshd rule that gives every member the MCP
surface and no shell, and writes one nftables ruleset.

That ruleset decides one port. kitbashd listens on TCP 4318 for Processes, and
the receiver accepts a connection only on the loopback interface, which is
where a rootless Process arrives: a container reaches it as
`host.containers.internal`, and pasta delivers that over loopback, with the
host's own primary address as the source. 4318 from any real interface is
dropped, whatever source address it claims, so neither a spoofed address nor
the next machine to be handed this one by DHCP inherits the permission. The
token kitbashd minted for the Process still decides whose records they are.
Port 22 is accepted before any of it, so a ruleset that loads is never a
ruleset that locks the operator out, and every other port is left as it was:
this is not a host firewall. `install.sh` rewrites `/etc/nftables.nft` on every
run and stops with a non zero exit if the ruleset does not load, because a host
that skipped it has the receiver open; operator rules belong in
`/etc/nftables.d/<name>.nft`, which that file includes after kitbash's own table
and `install.sh` never touches.

**Give the host a domain.** Optional, and what turns the reverse proxy on. With
one, every Process with `expose: http` is reached at
`https://<name>.<member>.<domain>`: `proc_run` answers the address and
`proc_list` shows it, so sharing a web application is giving somebody its
address. Set it at install, or later in `/etc/conf.d/kitbashd` followed by
`rc-service kitbashd restart`:

```sh
KITBASH_DOMAIN=kitbash.example.org sh /usr/share/kitbash/install.sh
```

The DNS record is one wildcard: `*.kitbash.example.org` and
`kitbash.example.org` both pointing at the host's public address. Two labels
are below the domain, `<name>.<member>`, so the record has to be `*` at that
level or a wildcard covering both, which is what a provider's `*` record under
a delegated zone does.

A unit may ask for one name of its own with `hostname`, outside the domain:
under `KITBASH_DOMAIN` there are derived names and nothing else, so a name
there is refused and no member can take the apex or a neighbour's address. What
makes a declared name the member's is that it points here, so kitbashd resolves
it when the Process is run and at every start and serves it only while the
answer carries an address of this host, or is a `CNAME` to the derived name.
Behind a gateway or a static NAT the host's own addresses are not the public
one, so name it with `KITBASH_PUBLIC_ADDRESS`; in `gateway` mode a host that
was not told it serves no declared name at all.

`KITBASH_TLS` decides who holds the certificate. `acme`, the default with a
domain, has kitbashd listen on 80 and 443 and obtain one certificate per host
name from Let's Encrypt, which needs both ports reachable from the internet.
There is no wildcard certificate, because a wildcard covers one label and these
names have two. `gateway` has kitbashd listen on 80 alone and trust the `Host`
a gateway in front of it forwards, which is how a host behind one public
address is put on the internet: the gateway terminates TLS and kitbashd answers
which names it serves, at `GET /.kitbash/ask?domain=<name>`, 200 for a name on
its routing table and 404 for anything else. With Caddy:

```caddyfile
{
	on_demand_tls {
		ask http://<host>:80/.kitbash/ask
	}
}
https:// {
	tls {
		on_demand
	}
	reverse_proxy <host>:80
}
```

A catch all site is what makes every served name, default and declared, get its
certificate the first time it is asked for, and the ask keeps the gateway from
obtaining one for a name kitbashd does not serve. In this mode `/.kitbash/` is
reserved on every served name: kitbashd answers it and no Process receives it.

With a gateway, set `KITBASH_GATEWAY_ADDRESS` to the address it forwards from
and install.sh narrows the accept on 80 to it, so the proxy is reachable
through the gateway and from nowhere else.

install.sh writes `KITBASH_DOMAIN`, `KITBASH_TLS`, `KITBASH_PUBLIC_ADDRESS` and
`KITBASH_GATEWAY_ADDRESS` to `/etc/conf.d/kitbashd`
and opens 80, and 443 in `acme` mode, in the ruleset above. Running it again
without either variable leaves the file as it is. A host with no domain routes
nothing and an `http` Process keeps its internal port, which is how kitbash
works without this.

**Run something on time.** A unit with `expose: none` may declare `schedule`,
five cron fields read in UTC, and kitbashd starts it on time: `proc_run`
registers the job and starts nothing, answering `state: scheduled` and the
`nextRun` it will make, and at each tick the daemon starts the container as the
owner with the same environment, secrets, mounts and ceiling any start gets.
Every tick writes one `kitbash.schedule` record, so "did it run this morning" is
a `tel_query`; `proc_logs` reads the last run and `proc_stop` unregisters the
schedule. A tick during a run is skipped and recorded, a run longer than 6 hours
is stopped, and ticks missed while the host was down are not made up.

**Or boot the ISO.** `kitbash-0.12.1-x86_64.iso` and `kitbash-0.12.1-aarch64.iso`
on the release are Alpine's own image with the kitbashd apk and its
dependencies on it. Booted from a VM's CD drive either comes up as a working
kitbash host in memory; `setup-alpine -f /media/cdrom/kitbash/answers` on its
console installs it to `/dev/vda` unattended. The aarch64 image is UEFI only.
See [`packaging/iso/README.md`](packaging/iso/README.md).

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
nft list chain inet kitbash input
```

From the member's machine, one MCP session that lists the surface:

```sh
{ printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"check","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'; sleep 2; } \
  | ssh alice@kitbash.example.org | grep -o '"name":"[a-z]*_[a-z_]*"' | wc -l
```

25, the whole built in surface: 4 `fs`, 4 `pkg`, 4 `proc`, 2 `tel`, 5 `users`,
3 `approvals` and 3 `secrets`. Each running Process with `expose: mcp` adds its
own tools on top of those. The `sleep` holds stdin open long enough for the answer; a session
whose stdin closes first is a session that ends before it replies.

**Upgrade.** Download the new release's apk, install it and run install.sh again:

```sh
apk add kitbashd-<newver>-r0.<arch>.apk
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
| `/var/lib/kitbash/secrets/<member>/<NAME>` | The values a member set with `secrets_set`. Root only, 0700 and 0600, outside the database so the nightly backup carries none |
| `/var/lib/kitbash/certs` | The certificates the reverse proxy obtained in `acme` mode. Root only, 0700 |
| `/etc/conf.d/kitbashd` | What the host tells kitbashd about itself, `KITBASH_DOMAIN` and `KITBASH_TLS` among them |
| `/var/log/kitbashd.log` | The daemon's log, both streams of the OpenRC service |
| `/org/.archive/<member>` | The home of a removed member, root only and invisible to the surface |

Built images live in each member's own rootless container store; the image
labels are the build history.

**What needs the daemon.** `fs`, `pkg` and `proc` work without kitbashd: the
Telemetry those calls record is dropped and one line goes to the session log.
`tel_query`, `tel_retention`, the `users` family, the `approvals` family, the
`secrets` family, a member's write to `/org`, the Process token and fan out
secret, `/mcp` for Processes and boot restore all need it running.

## Development

Go 1.26 and git are the only requirements. `make build` produces two static
binaries: `bin/kitbash-mcp`, which sshd runs as the connecting user through
`ForceCommand`, and `bin/kitbashd`, the resident daemon OpenRC starts at boot.

| Target | What it does |
|--------|--------------|
| `make build` | Static binaries for this host in `bin/kitbash-mcp` and `bin/kitbashd` |
| `make test` | `go test ./...`, including the MCP surface over an in memory transport |
| `make lint` | `gofmt -l` and `go vet ./...` |
| `make build-linux` | `bin/kitbash-mcp-linux-<goarch>` and `bin/kitbashd-linux-<goarch>` for `amd64` and `arm64`, the binaries a kitbash host runs |

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

## Releases

The version has one source, `pkgver` in `packaging/apk/APKBUILD`, and a release
is one commit (PLAN.md 4.8):

```sh
make bump VERSION=0.2.0   # writes the version everywhere, commits "Release 0.2.0"
git push -u origin release/0.2.0 && gh pr create --fill
```

Merging that pull request is the release. `release.yml` builds the apk and the
ISO on Alpine, once on an x86_64 runner and once on an arm64 one, boots each ISO
in QEMU on the runner that built it, then creates the tag and the GitHub release
with `kitbashd-<ver>-r0.<arch>.apk`, the signing public key, the four binaries,
`kitbash-<ver>-<arch>.iso` and one `SHA256SUMS`. Neither architecture is
emulated and neither is cross built by abuild. A push to `main` whose version
already has a tag releases nothing. `make check-version` is the guard CI runs on
every push: every version the README names must be `pkgver`.

## License

[MIT](LICENSE)
