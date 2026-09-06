package manifest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/manifest"
)

// withTools writes a manifest folder whose one tool refers to ref for its
// input schema and returns the folder.
func withTools(t *testing.T, ref string) (*manifest.Manifest, string) {
	t.Helper()
	dir := t.TempDir()
	m, err := manifest.Parse([]byte(`name: probe
description: Probe a media file and report what is inside it.
provides:
  tools:
    - name: probe
      description: Report the streams of one media file.
      input: { $ref: ` + ref + ` }
      output: { type: object }
deploy:
  units:
    - type: container
      build: .
      expose: mcp
`))
	if err != nil {
		t.Fatalf("parsing the manifest: %v", err)
	}
	return m, dir
}

func TestResolveSchemasInlinesARef(t *testing.T) {
	m, dir := withTools(t, "schemas/probe.in.json")
	if err := os.MkdirAll(filepath.Join(dir, "schemas"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "schemas", "probe.in.json"),
		`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`)

	tools, err := m.ResolveSchemas(dir)
	if err != nil {
		t.Fatalf("ResolveSchemas: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("resolved %d tools, want 1", len(tools))
	}
	if got := tools[0].Input["type"]; got != "object" {
		t.Errorf("the input schema was not inlined: %+v", tools[0].Input)
	}
	if _, stillRef := tools[0].Input["$ref"]; stillRef {
		t.Error("the input schema is still a reference")
	}
	if err := tools[0].ValidateInput([]byte(`{"path":"/org/a/b.mp4"}`)); err != nil {
		t.Errorf("a valid input was refused: %v", err)
	}
	if err := tools[0].ValidateInput([]byte(`{}`)); err == nil {
		t.Error("an input missing a required property was accepted")
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func TestResolveSchemasRefusesBadRefs(t *testing.T) {
	// An absolute reference never reaches here: spec/manifest.schema.json
	// refuses a $ref that begins with a slash before the manifest parses.
	cases := []struct {
		name string
		ref  string
		want string
	}{
		{name: "parent", ref: "../outside.json", want: "leaves this folder"},
		{name: "dot component", ref: ".hidden/schema.json", want: "reserved"},
		{name: "missing", ref: "schemas/absent.json", want: "does not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, dir := withTools(t, tc.ref)
			if _, err := m.ResolveSchemas(dir); err == nil {
				t.Fatalf("the reference %q was accepted", tc.ref)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error is %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestResolveSchemasRefusesAnOversizeSchema(t *testing.T) {
	m, dir := withTools(t, "schemas/probe.in.json")
	if err := os.MkdirAll(filepath.Join(dir, "schemas"), 0o755); err != nil {
		t.Fatal(err)
	}
	padding := strings.Repeat("x", manifest.MaxSchemaBytes)
	write(t, filepath.Join(dir, "schemas", "probe.in.json"),
		`{"type":"object","description":"`+padding+`"}`)

	_, err := m.ResolveSchemas(dir)
	if err == nil {
		t.Fatal("an oversize schema was accepted")
	}
	if !strings.Contains(err.Error(), "byte limit") {
		t.Errorf("error is %q, want it to mention the byte limit", err)
	}
}

func TestResolveSchemasRefusesASymlinkedSchema(t *testing.T) {
	m, dir := withTools(t, "schemas/probe.in.json")
	if err := os.MkdirAll(filepath.Join(dir, "schemas"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "elsewhere.json")
	write(t, outside, `{"type":"object"}`)
	if err := os.Symlink(outside, filepath.Join(dir, "schemas", "probe.in.json")); err != nil {
		t.Skipf("this filesystem does not do symlinks: %v", err)
	}
	if _, err := m.ResolveSchemas(dir); err == nil {
		t.Fatal("a symlinked schema was accepted")
	}
}

func TestReservedNames(t *testing.T) {
	for _, name := range []string{"fs", "pkg", "proc", "tel", "users", "approvals"} {
		if !manifest.Reserved(name) {
			t.Errorf("%s is a built in family and must be reserved", name)
		}
	}
	if manifest.Reserved("ffmpeg") {
		t.Error("ffmpeg is not a built in family")
	}
}

func TestUnitDefaults(t *testing.T) {
	m, err := manifest.Parse([]byte(`name: ffmpeg
description: Transcode and probe media files. Use for any audio or video conversion.
deploy:
  units:
    - type: container
      build: src
      port: 8080
      env: { LOG_LEVEL: debug }
      limits: { cpu: "1", memory: "512Mi" }
`))
	if err != nil {
		t.Fatalf("parsing the manifest: %v", err)
	}
	unit, ok := m.Unit()
	if !ok {
		t.Fatal("the manifest carries a deploy block but no unit was read")
	}
	if unit.Expose != manifest.ExposeNone {
		t.Errorf("expose defaults to %q, want none", unit.Expose)
	}
	if unit.Restart != manifest.RestartAlways {
		t.Errorf("restart defaults to %q, want always", unit.Restart)
	}
	if unit.Port != 8080 {
		t.Errorf("port is %d, want 8080", unit.Port)
	}
	if unit.Build != "src" {
		t.Errorf("build is %q, want src", unit.Build)
	}
	if unit.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("env is %v, want LOG_LEVEL debug", unit.Env)
	}
	if unit.Limits.CPU != "1" || unit.Limits.Memory != "512Mi" {
		t.Errorf("limits are %+v, want cpu 1 and memory 512Mi", unit.Limits)
	}
}
