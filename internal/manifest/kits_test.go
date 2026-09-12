package manifest_test

import (
	"errors"
	"testing"

	"github.com/zyx1121/kitbash/internal/manifest"
)

func TestParseAUnitThatNamesItsBuilderAndRunner(t *testing.T) {
	m, err := manifest.Parse([]byte(`name: trainer
description: A job built by one kit and run by another, on a machine this one is not.
deploy:
  units:
    - type: container
      build: .
      builder: /org/nix-build
      runner: /home/alice/my-runner
      expose: none
      limits: { memory: "512Mi" }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	unit, ok := m.Unit()
	if !ok {
		t.Fatal("the manifest carries no unit")
	}
	if unit.Builder != "/org/nix-build" {
		t.Errorf("the builder is %q, want the Package folder the manifest names", unit.Builder)
	}
	if unit.Runner != "/home/alice/my-runner" {
		t.Errorf("the runner is %q, want the Package folder the manifest names", unit.Runner)
	}
	// The unit travels to a run kit as the manifest wrote it, so what it wrote
	// has to be kept rather than rebuilt from the typed fields.
	if unit.Raw["build"] != "." || unit.Raw["limits"] == nil {
		t.Errorf("the raw unit is %v, want the unit as the manifest wrote it", unit.Raw)
	}
}

func TestParseAUnitThatNamesNeither(t *testing.T) {
	m, err := manifest.Parse([]byte(`name: ffmpeg
description: Transcode and probe media files. Use for any audio or video conversion.
deploy:
  units:
    - type: container
      build: .
      expose: mcp
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	unit, ok := m.Unit()
	if !ok {
		t.Fatal("the manifest carries no unit")
	}
	if unit.Builder != "" || unit.Runner != "" {
		t.Errorf("the unit names the builder %q and the runner %q, want neither: this is every manifest written before the fields existed",
			unit.Builder, unit.Runner)
	}
}

func TestParseRefusesABuilderOrRunnerThatIsNotAPackagePath(t *testing.T) {
	cases := map[string]string{
		"relative builder":  "      builder: nix-build\n",
		"builder with dots": "      builder: /org/../etc\n",
		"empty runner":      "      runner: \"\"\n",
		"runner with space": "      runner: /org/my runner\n",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := manifest.Parse([]byte(`name: trainer
description: A job whose manifest names a kit by something that is not a Package path.
deploy:
  units:
    - type: container
      build: .
` + line))
			if err == nil {
				t.Fatal("expected an error")
			}
			var invalid *manifest.ErrInvalid
			if !errors.As(err, &invalid) {
				t.Fatalf("error is %T, want ErrInvalid", err)
			}
		})
	}
}

func TestAcceptsArgsBindsAHookOnTheSchema(t *testing.T) {
	tool := manifest.Tool{
		Name: manifest.ToolRun,
		Input: map[string]any{
			"type":                 "object",
			"required":             []any{"package", "digest", "name", "unit"},
			"additionalProperties": false,
			"properties": map[string]any{
				"package": map[string]any{"type": "string"},
				"digest":  map[string]any{"type": "string"},
				"name":    map[string]any{"type": "string"},
				"unit":    map[string]any{"type": "object"},
			},
		},
	}
	args := map[string]any{
		"package": "/org/trainer",
		"digest":  "sha256:" + "ab",
		"name":    "trainer",
		"unit":    map[string]any{"type": "container"},
	}
	if err := tool.AcceptsArgs(args); err != nil {
		t.Errorf("a run tool refused the arguments its hook is called with: %v", err)
	}
	delete(args, "unit")
	if err := tool.AcceptsArgs(args); err == nil {
		t.Error("a run tool accepted a call without the unit its schema requires")
	}
}
