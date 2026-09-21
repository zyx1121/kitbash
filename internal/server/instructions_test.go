package server_test

import (
	"os"
	"path/filepath"
	"sort"
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
		"A mount source is any folder a manifest above it describes.",
		// The sentence that turns the two unit example into a rule, which is
		// what the M11 rounds got wrong on their own, see PLAN.md 5.6.
		"Units of one Package reach each other on localhost, and exactly one of them declares expose.",
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
//
// It is read through Units(), which is the loader's own view of a composed
// Package: two units, exactly one of them the face, one built from the folder
// and one an upstream image pinned by digest. A text that taught two faces, or
// none, would be a Package the host answers invalid-manifest for.
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
	units, err := m.Units()
	if err != nil {
		t.Fatalf("the units of the example do not load:\n%s\n%v", body, err)
	}
	if len(units) != 2 {
		t.Fatalf("the example declares %d units, want the two the text teaches: %+v", len(units), units)
	}
	faces := 0
	for _, unit := range units {
		if unit.Name == "" {
			t.Errorf("the unit %+v carries no name, which a Package of two units requires", unit)
		}
		switch unit.Expose {
		case manifest.ExposeMCP, manifest.ExposeHTTP:
			faces++
			if unit.Build != "." || unit.Port == 0 {
				t.Errorf("the face is %+v, want build . and the port it listens on", unit)
			}
		}
	}
	if faces != 1 {
		t.Fatalf("%d units of the example declare a face, want exactly one: %+v", faces, units)
	}
	// The unit behind the face is the other half of what a unit may be: an
	// upstream image pinned by digest, reached on localhost.
	sidecar := units[1]
	if !strings.Contains(sidecar.Image, "@sha256:") || sidecar.Build != "" {
		t.Errorf("the second unit is %+v, want an image pinned by digest", sidecar)
	}
	if !strings.Contains(strings.Join(environmentOf(units[0]), " "), "localhost") {
		t.Errorf("the face is given %v, want an address on localhost for the unit beside it",
			environmentOf(units[0]))
	}
}

// environmentOf is one unit's environment as a list, so a test can say what
// the face is told without depending on the order of a map.
func environmentOf(unit manifest.Unit) []string {
	out := make([]string, 0, len(unit.Environment))
	for name, value := range unit.Environment {
		out = append(out, name+"="+value)
	}
	sort.Strings(out)
	return out
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
