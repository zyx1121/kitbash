package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/proc"
)

// The tests that drive a Package run it in this process over an in memory
// transport, so the argv the bridge would really run is checked here. It is
// the one line that reaches podman, and getting it wrong takes every proxied
// tool down at once.
func TestExecTransportArgv(t *testing.T) {
	runner := podman.NewFake()
	digest := "sha256:" + strings.Repeat("a", 64)
	runner.Entrypoints[digest] = []string{"node", "/app/server.js"}
	b := &Bridge{runner: runner}

	transport, err := b.execTransport(context.Background(), &proc.Process{
		ID:        "0192f000-0000-7000-8000-000000000000",
		Package:   "/home/tester/echo",
		Digest:    digest,
		Container: "kitbash-echo-echo",
	})
	if err != nil {
		t.Fatalf("execTransport: %v", err)
	}
	command, ok := transport.(*mcp.CommandTransport)
	if !ok {
		t.Fatalf("transport is %T, want a command transport", transport)
	}
	want := "podman exec --interactive kitbash-echo-echo node /app/server.js"
	if got := strings.Join(command.Command.Args, " "); got != want {
		t.Errorf("the bridge would run %q, want %q", got, want)
	}
	for _, arg := range command.Command.Args {
		if arg == "--" {
			t.Error("the argv carries a double dash, which podman hands to the runtime as the command")
		}
	}
}

// An image with nothing to run cannot serve an MCP session, and saying so is
// better than execing an empty command.
func TestExecTransportRefusesAnImageWithNoEntrypoint(t *testing.T) {
	runner := podman.NewFake()
	digest := "sha256:" + strings.Repeat("b", 64)
	runner.Entrypoints[digest] = []string{}
	b := &Bridge{runner: runner}

	_, err := b.execTransport(context.Background(), &proc.Process{
		Package:   "/home/tester/echo",
		Digest:    digest,
		Container: "kitbash-echo-echo",
	})
	if err == nil {
		t.Fatal("an image with no entrypoint was accepted")
	}
	if !strings.Contains(err.Error(), "entrypoint") {
		t.Errorf("error is %q, want it to name the missing entrypoint", err)
	}
}
