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

## Install

On a fresh Alpine Linux VM, as root:

```sh
apk add --allow-untrusted kitbashd-0.1.0-r0.apk
KITBASH_DNS=1.1.1.1 sh /usr/share/kitbash/install.sh
kitbash-adduser alice 'ssh-ed25519 AAAA... alice@laptop' admin
```

The apk is built from a source tarball with `packaging/apk/build.sh` on any Alpine host. install.sh is idempotent and turns the machine into a kitbash host: rootless podman, cgroups v2, the `/org` shared repositories, and an sshd rule that gives every member the MCP surface and nothing else.

On a member's machine, one line connects their agent:

```sh
claude mcp add kitbash -- ssh alice@kitbash.example.org
```

## Status

Version 1 complete (v0.6.2): all six milestones done and the backlog cleared; what stays open is in PLAN.md 5.5. Read [PLAN.md](PLAN.md): positioning, the four object model, kits, kitbashOS, and version 1 milestones. Machine readable definitions live in [`spec/`](spec/).

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
