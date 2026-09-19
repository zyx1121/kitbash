package server

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
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
			matches(t, tc.got, tc.want)
		})
	}
}

// The users, approvals and secrets families are served by kitbashd, so these
// schemas are the only description of them kitbash-mcp holds and nothing else
// would notice if they drifted. Every tool is diffed on both sides, input and
// output, against spec/mcp-surface.yaml.
func TestUsersAndApprovalsSchemasMatchTheSurfaceSpecification(t *testing.T) {
	surface := readSurface(t)
	cases := []struct {
		family string
		tool   string
		side   string
		got    string
	}{
		{"users", "users_me", "input", string(usersMeInputSchema)},
		{"users", "users_me", "output", string(usersMeOutputSchema)},
		{"users", "users_create", "input", string(usersCreateInputSchema)},
		{"users", "users_create", "output", string(usersCreateOutputSchema)},
		{"users", "users_list", "input", string(usersListInputSchema)},
		{"users", "users_list", "output", string(usersListOutputSchema)},
		{"users", "users_add_key", "input", string(usersAddKeyInputSchema)},
		{"users", "users_add_key", "output", string(usersAddKeyOutputSchema)},
		{"users", "users_remove", "input", string(usersRemoveInputSchema)},
		{"users", "users_remove", "output", string(usersRemoveOutputSchema)},
		{"approvals", "approvals_list", "input", string(approvalsListInputSchema)},
		{"approvals", "approvals_list", "output", string(approvalsListOutputSchema)},
		{"approvals", "approvals_approve", "input", string(approvalsApproveInputSchema)},
		{"approvals", "approvals_approve", "output", string(approvalsApproveOutputSchema)},
		{"approvals", "approvals_reject", "input", string(approvalsRejectInputSchema)},
		{"approvals", "approvals_reject", "output", string(approvalsRejectOutputSchema)},
		{"secrets", "secrets_set", "input", string(secretsSetInputSchema)},
		{"secrets", "secrets_set", "output", string(secretsSetOutputSchema)},
		{"secrets", "secrets_list", "input", string(secretsListInputSchema)},
		{"secrets", "secrets_list", "output", string(secretsListOutputSchema)},
		{"secrets", "secrets_remove", "input", string(secretsRemoveInputSchema)},
		{"secrets", "secrets_remove", "output", string(secretsRemoveOutputSchema)},
	}
	for _, tc := range cases {
		t.Run(tc.tool+" "+tc.side, func(t *testing.T) {
			matches(t, tc.got, dig(t, surface, "families", tc.family, "tools", tc.tool, tc.side))
		})
	}
}

// The fs family is this server's own, and the schemas below are the whole
// wire contract of it, so they are diffed against spec/mcp-surface.yaml the
// way the forwarded families are: a shape that changed in one place and not in
// the other is a specification nobody can trust, see PLAN.md section 4.5.
func TestFsSchemasMatchTheSurfaceSpecification(t *testing.T) {
	surface := readSurface(t)
	cases := []struct {
		family string
		tool   string
		side   string
		got    string
	}{
		{"fs", "fs_list", "input", string(listInputSchema)},
		{"fs", "fs_list", "output", string(listOutputSchema)},
		{"fs", "fs_write", "input", string(writeInputSchema)},
		{"fs", "fs_write", "output", string(writeOutputSchema)},
		{"fs", "fs_history", "input", string(historyInputSchema)},
		{"fs", "fs_history", "output", string(historyOutputSchema)},
		// fs_read publishes no output schema, and the metadata block it
		// returns instead is still an answer a caller parses.
		{"fs", "fs_read", "output", string(readMetaSchema)},
		{"pkg", "pkg_build", "output", string(pkgBuildOutputSchema)},
		{"pkg", "pkg_list", "output", string(pkgListOutputSchema)},
		{"proc", "proc_list", "input", string(procListInputSchema)},
		{"proc", "proc_list", "output", string(procListOutputSchema)},
	}
	for _, tc := range cases {
		t.Run(tc.tool+" "+tc.side, func(t *testing.T) {
			matches(t, tc.got, resolve(t, surface, dig(t, surface, "families", tc.family, "tools", tc.tool, tc.side)))
		})
	}
}

// resolve replaces every {"$ref": "#/$defs/x"} with the definition it names.
// The specification factors the shapes it repeats and the transcription writes
// them out, so the two are compared as the documents a client reads.
func resolve(t *testing.T, surface map[string]any, node any) any {
	t.Helper()
	switch value := normalise(t, node).(type) {
	case map[string]any:
		if ref, held := value["$ref"].(string); held {
			name, found := strings.CutPrefix(ref, "#/$defs/")
			if !found {
				t.Fatalf("%s holds a reference this test cannot resolve: %s", surfacePath, ref)
			}
			return resolve(t, surface, dig(t, surface, "$defs", name))
		}
		out := map[string]any{}
		for key, child := range value {
			out[key] = resolve(t, surface, child)
		}
		return out
	case []any:
		out := make([]any, 0, len(value))
		for _, child := range value {
			out = append(out, resolve(t, surface, child))
		}
		return out
	default:
		return value
	}
}

// matches holds one transcribed schema to the node of the specification it was
// transcribed from. Both sides are trimmed string by string first: a folded
// block scalar in YAML ends with a newline and a JSON string does not, so a
// description written across several lines of the specification is the same
// description here.
func matches(t *testing.T, transcribed string, want any) {
	t.Helper()
	specified := trimmed(normalise(t, want))
	var got any
	if err := json.Unmarshal([]byte(transcribed), &got); err != nil {
		t.Fatalf("the transcribed schema is not JSON: %v", err)
	}
	got = trimmed(got)
	if !reflect.DeepEqual(got, specified) {
		t.Errorf("the transcribed schema is\n%s\nand the specification says\n%s",
			pretty(t, got), pretty(t, specified))
	}
}

// trimmed strips the whitespace a folded YAML scalar carries from every string
// of a decoded document.
func trimmed(value any) any {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		out := map[string]any{}
		for key, child := range v {
			out[key] = trimmed(child)
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, child := range v {
			out = append(out, trimmed(child))
		}
		return out
	default:
		return value
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
