package manifest_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/manifest"
)

// unitWith is one Package manifest whose container unit carries the block
// under test, so every case below differs by that block alone.
func unitWith(block string) string {
	return `name: ffmpeg
description: Transcode and probe media files. Use for any audio or video conversion.
deploy:
  units:
    - type: container
      build: .
      expose: mcp
` + block
}

// A unit declares the names it needs and nothing else: what a member reads in
// pkg_inspect is a list of names, and the values are theirs, see PLAN.md
// section 2.3.
func TestUnitCarriesTheDeclaredSecretNames(t *testing.T) {
	m, err := manifest.Parse([]byte(unitWith("      secrets: [ANTHROPIC_API_KEY, OPENAI_API_KEY]\n")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	unit, ok := m.Unit()
	if !ok {
		t.Fatal("the manifest has no unit")
	}
	if len(unit.Secrets) != 2 || unit.Secrets[0] != "ANTHROPIC_API_KEY" || unit.Secrets[1] != "OPENAI_API_KEY" {
		t.Errorf("the unit declares %v, want the two names in manifest order", unit.Secrets)
	}
	// The manifest is answered as it was written, so pkg_inspect shows the
	// declared names without anything having to copy them out.
	if !strings.Contains(fmt.Sprint(m.Raw), "ANTHROPIC_API_KEY") {
		t.Error("the raw document does not carry the declared names")
	}
}

// Every refusal a secrets block can earn, each one its own guard. A name the
// schema would take and the rules would not is the reason two of them are in
// code rather than in spec/manifest.schema.json.
func TestSecretsRefusals(t *testing.T) {
	cases := map[string]string{
		"a lower case name":            "      secrets: [anthropic_api_key]\n",
		"a name starting with a digit": "      secrets: [1KEY]\n",
		"a name with a hyphen":         "      secrets: [ANTHROPIC-API-KEY]\n",
		"a name over 64 characters":    "      secrets: [A" + strings.Repeat("B", 64) + "]\n",
		"the same name twice":          "      secrets: [KEY, KEY]\n",
		"more than sixteen names":      "      secrets: [" + names(17) + "]\n",
		"a name kitbashd speaks for":   "      secrets: [KITBASH_TELEMETRY_TOKEN]\n",
		"any other KITBASH_ name":      "      secrets: [KITBASH_ANYTHING]\n",
		"a name env also sets": "      env: { ANTHROPIC_API_KEY: \"in the manifest\" }\n" +
			"      secrets: [ANTHROPIC_API_KEY]\n",
	}
	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := manifest.Parse([]byte(unitWith(block)))
			if err == nil {
				t.Fatal("the manifest was accepted")
			}
			var invalid *manifest.ErrInvalid
			if !errors.As(err, &invalid) {
				t.Fatalf("error is %T, want ErrInvalid", err)
			}
		})
	}
	// Sixteen names, all of them legal, is a manifest kitbash takes.
	if _, err := manifest.Parse([]byte(unitWith("      secrets: [" + names(16) + "]\n"))); err != nil {
		t.Errorf("sixteen names were refused: %v", err)
	}
}

// Every variable kitbashd speaks for is a KITBASH_ name, which is what makes
// the prefix the whole rule. A name added to internal/daemon's ownedEnv that
// did not start with it would be a name a manifest could claim.
func TestEveryOwnedNameIsRefused(t *testing.T) {
	for _, owned := range []string{
		"KITBASH_TELEMETRY_ENDPOINT", "KITBASH_TELEMETRY_TOKEN", "KITBASH_PROCESS",
		"KITBASH_PACKAGE", "KITBASH_USER", "KITBASH_MCP_ENDPOINT", "KITBASH_FANOUT_SECRET",
		"KITBASH_CALLER", "KITBASH_PERMITS",
	} {
		if !strings.HasPrefix(owned, manifest.OwnedEnvPrefix) {
			t.Errorf("%s does not start with %s, so the prefix is not the whole rule", owned, manifest.OwnedEnvPrefix)
		}
		if _, err := manifest.Parse([]byte(unitWith("      secrets: [" + owned + "]\n"))); err == nil {
			t.Errorf("a unit declaring %s was accepted", owned)
		}
	}
}

// The name rule is one rule, spelled here and read by internal/secrets and by
// kitbashd's registration check.
func TestValidSecretName(t *testing.T) {
	for _, name := range []string{"K", "KEY", "ANTHROPIC_API_KEY", "A1_2", "A" + strings.Repeat("B", 63)} {
		if !manifest.ValidSecretName(name) {
			t.Errorf("%q is not accepted as a secret name", name)
		}
	}
	for _, name := range []string{
		"", "_KEY", "1KEY", "key", "KEY-NAME", "KEY.NAME", "KEY NAME", "KEY\n",
		"A" + strings.Repeat("B", 64),
	} {
		if manifest.ValidSecretName(name) {
			t.Errorf("%q is accepted as a secret name", name)
		}
	}
}

// secrets is a family of the surface, so a Package may not be named after it:
// its tools would collide with secrets_set and the rest, see PLAN.md 2.3.
func TestSecretsIsAReservedPackageName(t *testing.T) {
	if !manifest.Reserved("secrets") {
		t.Error("a Package named secrets is not refused")
	}
}

// names is a legal secrets list of n entries.
func names(n int) string {
	list := make([]string, 0, n)
	for i := range n {
		list = append(list, fmt.Sprintf("KEY_%d", i))
	}
	return strings.Join(list, ", ")
}
