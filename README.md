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

Planning. Read [PLAN.md](PLAN.md): positioning, the four object model, kits, kitbashOS, and version 1 milestones.

## Development

Go 1.26 and git are the only requirements. `make build` produces a static
`bin/kitbash-mcp`, the binary sshd runs as the connecting user through
`ForceCommand`.

| Target | What it does |
|--------|--------------|
| `make build` | Static binary for this host in `bin/kitbash-mcp` |
| `make test` | `go test ./...`, including the MCP surface over an in memory transport |
| `make lint` | `gofmt -l` and `go vet ./...` |
| `make build-linux` | `bin/kitbash-mcp-linux-amd64`, the binary a kitbash host runs |

The server serves the roots `/org` and `/home/<user>`. Set `KITBASH_ROOTS` to a
colon separated list of absolute paths to point it somewhere else, which is how
the tests and a local run against a fixture tree work:

```
KITBASH_ROOTS=/tmp/fixture/org:/tmp/fixture/home/tester bin/kitbash-mcp
```

## License

Private, all rights reserved.
