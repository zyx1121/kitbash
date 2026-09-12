// Package e2e is the end to end job of .github/workflows/e2e.yml: the whole
// system on one host, driven the way a member's agent drives it.
//
// Nothing here runs with go test ./... . Every test skips unless KITBASH_E2E is
// set, because each one needs what only the job builds: two Linux users with
// subordinate id ranges, a seeded /org, a rootless container runtime and a
// kitbashd running as root on the default socket. The workflow starts and stops
// the daemons; these tests drive the surface between those steps.
//
// The health probe is not driven here. kitbashd probes the path a Package
// declares on the Process's endpoint, and the echo fixture is an expose: mcp
// Process on stdio: it publishes no port, so it has no endpoint to request and
// no HTTP server to answer with. Proving a probe end to end needs a second
// fixture image with an HTTP listener, which is a build this job's eight
// minute budget does not have room for; the probe is covered by the unit tests
// in internal/daemon, see health_test.go.
//
// The client is written here rather than imported. kitbash's own Go client is
// what the server was developed against, so a second implementation of the wire
// protocol is the point: newline delimited JSON-RPC over the stdio of
// kitbash-mcp, started as each member with sudo the way sshd starts it with
// ForceCommand. The third client is the TypeScript SDK in client/mcp.mjs, which
// speaks streamable HTTP to the Process receiver from inside a container.
package e2e
