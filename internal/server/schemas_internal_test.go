package server

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// surfacePath is the normative surface, read from the repository rather than
// embedded: this test is the one place the transcription in schemas.go is held
// to the specification it was transcribed from.
const surfacePath = "../../spec/mcp-surface.yaml"

// The tel schemas are transcribed by hand, so they are diffed against
// spec/mcp-surface.yaml rather than trusted. A property the specification
// gained and the surface did not, such as producer, is a tool that silently
// refuses an input the specification allows.
func TestTelSchemasMatchTheSurfaceSpecification(t *testing.T) {
	surface := readSurface(t)
	cases := []struct {
		name string
		want any
		got  string
	}{
		{
			name: "tel_query input",
			want: dig(t, surface, "families", "tel", "tools", "tel_query", "input"),
			got:  string(telQueryInputSchema),
		},
		{
			name: "telAttributes",
			want: dig(t, surface, "$defs", "telAttributes"),
			got:  telAttributesDef,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := normalise(t, tc.want)
			var got any
			if err := json.Unmarshal([]byte(tc.got), &got); err != nil {
				t.Fatalf("the transcribed schema is not JSON: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("the transcribed schema is\n%s\nand the specification says\n%s",
					pretty(t, got), pretty(t, want))
			}
		})
	}
}

// readSurface decodes spec/mcp-surface.yaml.
func readSurface(t *testing.T) map[string]any {
	t.Helper()
	body, err := os.ReadFile(surfacePath)
	if err != nil {
		t.Fatalf("reading %s: %v", surfacePath, err)
	}
	var surface map[string]any
	if err := yaml.Unmarshal(body, &surface); err != nil {
		t.Fatalf("decoding %s: %v", surfacePath, err)
	}
	return surface
}

// dig walks a decoded YAML document by key.
func dig(t *testing.T, document map[string]any, keys ...string) any {
	t.Helper()
	var value any = document
	for _, key := range keys {
		node, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%s: %q is not under a mapping", surfacePath, key)
		}
		value, ok = node[key]
		if !ok {
			t.Fatalf("%s has no %q", surfacePath, key)
		}
	}
	return value
}

// normalise puts a YAML value into the shapes encoding/json decodes into, so
// the two sides are compared as documents rather than as Go types.
func normalise(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding the specification as JSON: %v", err)
	}
	var out any
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("decoding the specification as JSON: %v", err)
	}
	return out
}

// pretty renders one side of a difference readably.
func pretty(t *testing.T, value any) string {
	t.Helper()
	out, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	return string(out)
}
