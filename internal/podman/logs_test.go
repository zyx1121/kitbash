package podman

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePodman puts a script named podman first on PATH, so the CLI runs it the
// way it runs the real one. The two streams are what is being tested, and a
// script is the only fake that has two of them.
func fakePodman(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + argv + "\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, Binary), []byte(script), 0o755); err != nil {
		t.Fatalf("writing the fake runtime: %v", err)
	}
	t.Setenv("PATH", dir)
	return argv
}

// The Process of issue #130: one line on stdout, a traceback and an error line
// on stderr, and an exit of 0. podman logs writes the container's stdout to
// its own stdout and the container's stderr to its own stderr, so a reader
// that keeps only the first answers a member debugging a dead Process with
// nothing.
func TestLogsCarryStderrInterleavedWithStdout(t *testing.T) {
	fakePodman(t, `echo "agent: kitbash MCP server registered"
echo "Traceback (most recent call last):" 1>&2
echo "  File \"/app/agent.py\", line 1, in <module>" 1>&2
echo "ERROR: the agent could not start" 1>&2
echo "agent: exiting"`)

	lines, err := NewCLI().Logs(context.Background(), "kitbash-agent-agent", 200)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	want := []string{
		"agent: kitbash MCP server registered",
		"Traceback (most recent call last):",
		`  File "/app/agent.py", line 1, in <module>`,
		"ERROR: the agent could not start",
		"agent: exiting",
	}
	if len(lines) != len(want) {
		t.Fatalf("Logs answered %d lines, want %d: %q", len(lines), len(want), lines)
	}
	for i, line := range want {
		// The order is the runtime's: both descriptors are one pipe, so the
		// stderr of a run sits between the stdout lines it sat between.
		if lines[i] != line {
			t.Errorf("line %d is %q, want %q", i, lines[i], line)
		}
	}
}

// And the command is still podman logs with a tail: nothing selects one stream
// on the command line either, which is the other way this reads half a run.
func TestLogsAskForBothStreamsOfTheContainer(t *testing.T) {
	argv := fakePodman(t, "echo out")

	if _, err := NewCLI().Logs(context.Background(), "kitbash-agent-agent", 50); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	recorded, err := os.ReadFile(argv)
	if err != nil {
		t.Fatalf("reading the recorded command line: %v", err)
	}
	got := strings.TrimSpace(string(recorded))
	if got != "logs --tail 50 kitbash-agent-agent" {
		t.Errorf("the runtime was asked %q", got)
	}
}

// A failing podman still reports what it printed, so the server log keeps the
// cause of a refusal the way it did when stderr was read on its own.
func TestLogsOfAContainerThatIsGoneCarryTheCause(t *testing.T) {
	fakePodman(t, `echo "Error: no such container" 1>&2
exit 125`)

	lines, err := NewCLI().Logs(context.Background(), "kitbash-agent-agent", 200)
	if err == nil {
		t.Fatalf("Logs answered %q for a container that is gone, want an error", lines)
	}
	if !strings.Contains(err.Error(), "no such container") {
		t.Errorf("the error is %q, want the runtime's own message", err)
	}
}
