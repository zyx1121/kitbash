package bridge

import (
	"context"
	"errors"
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

// A session that will not open is one of two things, and only what the runtime
// wrote tells them apart. The kernel refuses to move the new process into the
// container's cgroup when this session is outside its member's, which is a
// session kitbashd did not place rather than a Package that is down, see
// sessions_join in spec/kitbashd-api.yaml.
func TestAnExecOutsideTheMemberCgroupIsNamed(t *testing.T) {
	p := &proc.Process{
		Package:   "/home/tester/import-mcp",
		Container: "kitbash-import-mcp-import-mcp",
	}
	refused := "crun: write to /sys/fs/cgroup/kitbash/loki/0192f000-0000-7000-8000-000000000000/" +
		"libpod-abc/cgroup.procs: Permission denied"

	prob := connectProblem(p, errors.New("EOF"), refused)
	if prob.Detail != "this session is outside the member's cgroup; kitbashd could not place it" {
		t.Errorf("detail is %q, want the placement named", prob.Detail)
	}
	if !strings.Contains(prob.Fix, "new session") {
		t.Errorf("fix is %q, want it to say what to do", prob.Fix)
	}

	// Anything else is the Package, and the runtime's words go with it.
	other := connectProblem(p, errors.New("EOF"), "Error: container is not running")
	if other.Detail == "this session is outside the member's cgroup; kitbashd could not place it" {
		t.Error("a Package that is down was reported as a cgroup problem")
	}
	if !strings.Contains(other.Fix, "proc_list") {
		t.Errorf("fix is %q, want the Package's own next step", other.Fix)
	}
}

// The runtime's error output is kept for the failure that follows it, and no
// more of it than one line of trouble.
func TestTheExecOutputIsKeptOnceAndBounded(t *testing.T) {
	b := &Bridge{execErrors: map[string]*boundedSink{}}
	sink := &boundedSink{}
	b.execErrors["one"] = sink
	sink.Write([]byte(strings.Repeat("x", ExecErrorBytes+100)))

	got := b.execOutput("one")
	if len(got) != ExecErrorBytes {
		t.Errorf("kept %d bytes, want at most %d", len(got), ExecErrorBytes)
	}
	if b.execOutput("one") != "" {
		t.Error("the output was read twice; a later failure is a later message")
	}
}
