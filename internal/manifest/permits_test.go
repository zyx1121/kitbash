package manifest_test

import (
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/manifest"
)

// TestPermitsMatchTools is the tool half of the narrowing: a * stands for any
// run of characters inside one name, so a Package says fs_* for a family and
// *_* for everything namespaced, see PLAN.md section 2.3.
func TestPermitsMatchTools(t *testing.T) {
	permits := manifest.Permits{Tools: []string{"fs_read", "fs_l*", "tel_*", "*_transcode"}}
	cases := []struct {
		tool string
		want bool
	}{
		{"fs_read", true},
		{"fs_list", true},
		{"fs_logs_anything", true}, // fs_l* is any run after the prefix
		{"fs_write", false},        // named by no glob
		{"tel_query", true},
		{"tel_", true}, // the run a * stands for may be empty
		{"ffmpeg_transcode", true},
		{"transcode", false}, // *_transcode still needs the underscore
		{"", false},
	}
	for _, c := range cases {
		if got := permits.Match(c.tool); got != c.want {
			t.Errorf("Match(%q) = %v, want %v", c.tool, got, c.want)
		}
	}

	// The zero block is the empty surface every Process starts from.
	var none manifest.Permits
	for _, tool := range []string{"fs_read", "proc_list", "anything"} {
		if none.Match(tool) {
			t.Errorf("a Package that declares no permits matched %q", tool)
		}
	}

	// A * on its own is every tool, which is what an owner's own kit declares
	// when it means the whole surface.
	all := manifest.Permits{Tools: []string{"*"}}
	if !all.Match("users_remove") {
		t.Error(`the glob "*" matched no tool`)
	}
}

// TestPermitsAllowsPaths is the path half: a prefix covers the path itself and
// everything below it, and a * is exactly one whole component.
func TestPermitsAllowsPaths(t *testing.T) {
	permits := manifest.Permits{Paths: []string{"/org", "/home/*/flows"}}
	cases := []struct {
		path string
		want bool
	}{
		{"/org", true},
		{"/org/handbook", true},
		{"/org/handbook/README.md", true},
		{"/org/", true},          // a trailing slash names the same folder
		{"/organisation", false}, // a prefix is components, not characters
		{"/home/alice/flows", true},
		{"/home/alice/flows/nightly.yaml", true},
		{"/home/alice", false}, // the * component is not a wildcard for the rest
		{"/home/alice/notes", false},
		{"/home/flows", false}, // a * component matches exactly one
		{"/home/alice/deep/flows", false},
		{"/org/../home/alice", false}, // a double dot is refused, never cleaned
		{"/org/./handbook", false},
		{"org/handbook", false}, // relative
		{"/", false},
		{"", false},
	}
	for _, c := range cases {
		if got := permits.Allows(c.path); got != c.want {
			t.Errorf("Allows(%q) = %v, want %v", c.path, got, c.want)
		}
	}

	var none manifest.Permits
	if none.AnyPath() || none.Allows("/org") {
		t.Error("a Package that declares no prefix allowed a path")
	}
	if !permits.AnyPath() {
		t.Error("AnyPath is false for a block that declares two prefixes")
	}
}

// TestPermitsRefuseGlobsTheyCannotHonour keeps a typo from becoming a rule:
// an entry this build cannot read permits nothing rather than being taken as
// a literal name or as a half matched component.
func TestPermitsRefuseGlobsTheyCannotHonour(t *testing.T) {
	bad := manifest.Permits{
		Tools: []string{"fs read", "fs/read", ""},
		Paths: []string{"/home/al*", "org/flows", "/org/../secrets", "/"},
	}
	err := bad.Validate()
	if err == nil {
		t.Fatal("Validate accepted globs this build cannot honour")
	}
	for _, want := range []string{`"fs read"`, `"fs/read"`, `"/home/al*"`, `"org/flows"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate said %q, which does not name %s", err, want)
		}
	}
	if bad.Match("fs read") || bad.Match("fs/read") {
		t.Error("a tool glob that does not validate still matched")
	}
	if bad.AnyPath() || bad.Allows("/home/alice") || bad.Allows("/org/secrets") {
		t.Error("a path prefix that does not validate still allowed a path")
	}
}

// TestPermitsFromAManifest reads the block off provides, which is where a
// Package declares it and where pkg_inspect shows it.
func TestPermitsFromAManifest(t *testing.T) {
	m, err := manifest.Parse([]byte(`name: workflow
description: Runs a graph whose steps call the tools of the owner's Processes.
provides:
  permits:
    tools: [fs_read, fs_list, "*_*"]
    paths: [/org, /home/*]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	permits := m.Permits()
	if err := permits.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !permits.Match("fs_read") || !permits.Match("ffmpeg_transcode") {
		t.Errorf("permits.tools = %v, which does not carry the declared globs", permits.Tools)
	}
	if permits.Match("nounderscore") {
		t.Error("a tool matching no glob was permitted")
	}
	if !permits.Allows("/org/flows/nightly.yaml") || !permits.Allows("/home/tester/notes") {
		t.Errorf("permits.paths = %v, which does not carry the declared prefixes", permits.Paths)
	}
	if permits.Allows("/etc/passwd") {
		t.Error("a path under no prefix was allowed")
	}

	// A manifest with no block permits nothing, which is the empty surface.
	plain, err := manifest.Parse([]byte(`name: handbook
description: How this organization works. Read before writing anything into /org.
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := plain.Permits(); got.Match("fs_read") || got.AnyPath() {
		t.Errorf("a manifest with no permits block answered %+v", got)
	}
}

// TestPermitsSchemaRefusesWhatTheGuardCannotRead keeps the manifest and this
// package saying the same thing: a block that would not be honoured is
// refused when it is written, not when a Process calls something.
func TestPermitsSchemaRefusesWhatTheGuardCannotRead(t *testing.T) {
	for _, block := range []string{
		"    tools: [\"fs read\"]",
		"    tools: [\"fs.read\"]",
		"    paths: [org/flows]",
		"    paths: [\"/home/al*\"]",
		"    scopes: [everything]",
	} {
		doc := `name: workflow
description: Runs a graph whose steps call the tools of the owner's Processes.
provides:
  permits:
` + block + "\n"
		if _, err := manifest.Parse([]byte(doc)); err == nil {
			t.Errorf("the schema accepted a permits block of\n%s", block)
		}
	}
}

// TestPermitsJSONRoundTrip is the trip from the store to the session child:
// what kitbashd wrote is what KITBASH_PERMITS carries and what the surface
// reads back.
func TestPermitsJSONRoundTrip(t *testing.T) {
	want := manifest.Permits{Tools: []string{"fs_read", "*_*"}, Paths: []string{"/org", "/home/*"}}
	got, err := manifest.ParsePermits(want.JSON())
	if err != nil {
		t.Fatalf("ParsePermits: %v", err)
	}
	if strings.Join(got.Tools, ",") != strings.Join(want.Tools, ",") ||
		strings.Join(got.Paths, ",") != strings.Join(want.Paths, ",") {
		t.Errorf("round trip gave %+v, want %+v", got, want)
	}

	// The empty block is an object with two empty lists, so a child never has
	// to tell an absent declaration from an empty one.
	if encoded := string(manifest.Permits{}.JSON()); encoded != `{"tools":[],"paths":[]}` {
		t.Errorf("the empty block is %s", encoded)
	}
	empty, err := manifest.ParsePermits([]byte(`{}`))
	if err != nil {
		t.Fatalf("ParsePermits of an empty object: %v", err)
	}
	if empty.Match("fs_read") || empty.AnyPath() {
		t.Errorf("the empty block permitted something: %+v", empty)
	}
	if _, err := manifest.ParsePermits(nil); err != nil {
		t.Errorf("ParsePermits of nothing: %v", err)
	}

	for _, bad := range []string{`{"tools":"fs_read"}`, `not json`, `{"scopes":[]}`, `{"tools":["fs read"]}`} {
		if _, err := manifest.ParsePermits([]byte(bad)); err == nil {
			t.Errorf("ParsePermits accepted %s", bad)
		}
	}
}
