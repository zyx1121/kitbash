package server_test

import (
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/server"
)

// instructionsBudget is what the text may cost. Claude Code puts instructions
// in the system prompt of every turn, so the paragraph is measured, not just
// written, see PLAN.md section 4.5.
const instructionsBudget = 1200

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
	for _, want := range []string{"/org", "kitbash.yaml", "pkg_build", "proc_run", "expose: mcp", "secrets_set"} {
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
	if strings.Contains(server.Instructions, "\u2014") {
		t.Error("instructions contain an em dash")
	}
	if strings.Contains(server.Instructions, "\n") {
		t.Error("instructions are one paragraph and carry no newline")
	}
	if server.Instructions != strings.TrimSpace(server.Instructions) {
		t.Error("instructions carry leading or trailing whitespace")
	}
	if strings.Contains(server.Instructions, "  ") {
		t.Error("instructions carry a double space, which is a join that lost a word")
	}
}
