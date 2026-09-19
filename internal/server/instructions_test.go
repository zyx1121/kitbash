package server_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/server"
)

// instructionsBudget is what the text may cost. Claude Code puts instructions
// in the system prompt of every turn, so the paragraph is measured, not just
// written, see PLAN.md section 4.5. It carries a whole minimal Package now,
// which is what the first trial spent fifteen calls learning.
const instructionsBudget = 2048

func TestInitializeCarriesTheInstructions(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	res := s.InitializeResult()
	if res == nil {
		t.Fatal("initialize returned no result")
	}
	if res.Instructions == "" {
		t.Fatal("initialize carried no instructions")
	}
	if res.Instructions != server.Instructions {
		t.Fatalf("initialize carried other text than the constant:\n%q", res.Instructions)
	}
	// The sentences an agent has to act on, so a rewrite that drops one fails
	// here rather than in a session.
	for _, want := range []string{
		"/org", "kitbash.yaml", "pkg_build", "proc_run", "expose: mcp", "secrets_set",
		"Folders inside a Package need no manifest of their own.",
		"Write all files of a Package in one fs_write with files.",
		"A mounted source folder needs its own kitbash.yaml.",
	} {
		if !strings.Contains(res.Instructions, want) {
			t.Errorf("instructions no longer mention %q", want)
		}
	}
}

func TestInstructionsStayWithinTheirBudget(t *testing.T) {
	if n := len(server.Instructions); n > instructionsBudget {
		t.Errorf("instructions are %d bytes, budget is %d", n, instructionsBudget)
	}
	// The escape, not the character: this file follows the same rule it checks.
	if strings.Contains(server.Instructions, "—") {
		t.Error("instructions contain an em dash")
	}
	if server.Instructions != strings.TrimSpace(server.Instructions) {
		t.Error("instructions carry leading or trailing whitespace")
	}
	// The prose is one paragraph either side of the example, and a double
	// space there is a join that lost a word. The example is indented on
	// purpose, so it is measured by its own rules above.
	for _, prose := range strings.Split(server.Instructions, "\n\n") {
		if strings.Contains(prose, "\n") {
			continue
		}
		if strings.Contains(prose, "  ") {
			t.Errorf("a prose paragraph carries a double space:\n%s", prose)
		}
	}
}

// TestTheExampleManifestLoads is the guard on the paragraph every agent reads:
// the manifest the instructions teach is written into a folder and read back
// with the real loader, so a schema change that would refuse it fails here
// rather than in the first Package somebody writes from it.
func TestTheExampleManifestLoads(t *testing.T) {
	dir := t.TempDir()
	body := exampleFile(t, server.ExampleManifest, manifest.FileName)
	// <member> is the one placeholder in the text, and a mount source is a
	// real path, so the reader substitutes their own name and so does this.
	body = strings.ReplaceAll(body, "<member>", "tester")
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), []byte(body), 0o644); err != nil {
		t.Fatalf("writing the example manifest: %v", err)
	}
	m, err := manifest.Load(dir)
	if err != nil {
		t.Fatalf("the example manifest in the instructions does not load:\n%s\n%v", body, err)
	}
	if m.Name == "" || m.Description == "" {
		t.Fatalf("the example manifest is not enough to make a folder visible: %+v", m)
	}
	if !m.IsPackage() {
		t.Fatal("the example manifest carries no deploy block, so it is not a Package")
	}
	units, ok := m.Raw["deploy"].(map[string]any)["units"].([]any)
	if !ok || len(units) != 1 {
		t.Fatalf("the example declares %v, want one unit", m.Raw["deploy"])
	}
	unit, _ := units[0].(map[string]any)
	if unit["build"] != "." || unit["expose"] != "http" {
		t.Fatalf("the example unit is %+v, want build . and expose http", unit)
	}
}

// TestTheExampleContainerfileIsBuildable holds the other half of the example
// to the shape a container build needs: a FROM first and a command to run.
// What it is not is a build, which is the e2e job's.
func TestTheExampleContainerfileIsBuildable(t *testing.T) {
	body := exampleFile(t, server.ExampleContainerfile, "Dockerfile")
	lines := []string{}
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			lines = append(lines, trimmed)
		}
	}
	if len(lines) == 0 {
		t.Fatal("the example Dockerfile is empty")
	}
	if !strings.HasPrefix(lines[0], "FROM ") {
		t.Fatalf("the example Dockerfile begins with %q, want a FROM", lines[0])
	}
	instructions := map[string]bool{}
	for _, line := range lines {
		instructions[strings.SplitN(line, " ", 2)[0]] = true
	}
	for _, want := range []string{"WORKDIR", "COPY", "RUN", "CMD"} {
		if !instructions[want] {
			t.Errorf("the example Dockerfile carries no %s", want)
		}
	}
}

// exampleFile is one file of the example as it would be written: the first
// line names the file and the rest is its body, indented by two spaces so a
// reader sees where one file ends and the next begins.
func exampleFile(t *testing.T, block, name string) string {
	t.Helper()
	if !strings.Contains(server.Instructions, block) {
		t.Fatalf("the instructions no longer carry the %s of the example", name)
	}
	lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
	if lines[0] != name {
		t.Fatalf("the example block begins with %q, want %s", lines[0], name)
	}
	body := make([]string, 0, len(lines)-1)
	for _, line := range lines[1:] {
		trimmed, found := strings.CutPrefix(line, "  ")
		if !found {
			t.Fatalf("the line %q of the example is not indented under %s", line, name)
		}
		body = append(body, trimmed)
	}
	return strings.Join(body, "\n") + "\n"
}
