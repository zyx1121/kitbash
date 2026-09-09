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
		if got := permits.Match(c.tool, nil); got != c.want {
			t.Errorf("Match(%q) = %v, want %v", c.tool, got, c.want)
		}
	}

	// The zero block is the empty surface every Process starts from.
	var none manifest.Permits
	for _, tool := range []string{"fs_read", "proc_list", "anything"} {
		if none.Match(tool, nil) {
			t.Errorf("a Package that declares no permits matched %q", tool)
		}
	}

	// A * on its own is every tool, which is what an owner's own kit declares
	// when it means the whole surface.
	all := manifest.Permits{Tools: []string{"*"}}
	if !all.Match("users_remove", nil) {
		t.Error(`the glob "*" matched no tool`)
	}
}

// packageTools is the predicate the surface hands Match: on a real session it
// is the bridge, which published the tools of the owner's Processes.
func packageTools(names ...string) manifest.PackageTool {
	held := map[string]bool{}
	for _, name := range names {
		held[name] = true
	}
	return func(name string) bool { return held[name] }
}

// TestPermitsPackagesIsTheOtherKitsAndNoBuiltIn is the reserved word: a kit
// that composes other kits says packages and gets exactly their tools. The
// glob that looks like it would say the same, *_*, says every built in too,
// which is the reason the word exists.
func TestPermitsPackagesIsTheOtherKitsAndNoBuiltIn(t *testing.T) {
	permits := manifest.Permits{Tools: []string{"fs_read", "fs_list", manifest.PermitPackages}}
	running := packageTools("echo_echo", "ffmpeg_transcode")

	for _, tool := range []string{"echo_echo", "ffmpeg_transcode", "fs_read", "fs_list"} {
		if !permits.Match(tool, running) {
			t.Errorf("Match(%q) is false, want the declared tools and the Package tools", tool)
		}
	}
	for _, tool := range []string{"fs_write", "users_remove", "approvals_approve", "proc_run", "tel_query"} {
		if permits.Match(tool, running) {
			t.Errorf("Match(%q) is true; packages must admit no built in", tool)
		}
	}
	// A Package tool of a Process that is not running is not on the surface
	// and is not admitted either.
	if permits.Match("ffmpeg_probe", running) {
		t.Error("packages admitted a tool no running Process publishes")
	}
	// Without the predicate there are no Package tools, so the word admits
	// nothing and the two named tools are all that is left.
	if permits.Match("echo_echo", nil) {
		t.Error("packages admitted a tool with no predicate to ask")
	}
	if !permits.Match("fs_read", nil) {
		t.Error("a literal name stopped matching when the predicate was absent")
	}

	// A glob is still a glob: an author who writes one has declared what it
	// matches, built ins included.
	wide := manifest.Permits{Tools: []string{"*_*"}}
	if !wide.Match("fs_write", running) || !wide.Match("echo_echo", running) {
		t.Error(`the glob "*_*" stopped matching what it says`)
	}
	// And the word is not a name: nothing is called packages.
	if !permits.Match("packages", packageTools("packages")) {
		t.Error("a Package tool named packages was refused by the predicate")
	}
	if permits.Match("packages", running) {
		t.Error("the reserved word matched itself as a literal tool name")
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
	if bad.Match("fs read", nil) || bad.Match("fs/read", nil) {
		t.Error("a tool glob that does not validate still matched")
	}
	if bad.AnyPath() || bad.Allows("/home/alice") || bad.Allows("/org/secrets") {
		t.Error("a path prefix that does not validate still allowed a path")
	}

	// One entry is bounded as well as the lists: a glob longer than the
	// longest tool name MCP allows matches nothing this surface publishes, and
	// a prefix longer than a Linux path names nothing.
	long := manifest.Permits{
		Tools: []string{strings.Repeat("a", manifest.MaxToolGlobBytes+1)},
		Paths: []string{"/" + strings.Repeat("b", manifest.MaxPathPrefixBytes)},
	}
	err = long.Validate()
	if err == nil {
		t.Fatal("Validate accepted an entry over the per entry limit")
	}
	for _, want := range []string{"over the limit of 63", "over the limit of 4096"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate said %q, which does not name %s", err, want)
		}
	}
	if long.Match(strings.Repeat("a", manifest.MaxToolGlobBytes+1), nil) || long.AnyPath() {
		t.Error("an entry over the per entry limit still matched")
	}
	// The last entry that fits is still honoured.
	fits := manifest.Permits{Tools: []string{strings.Repeat("a", manifest.MaxToolGlobBytes)}}
	if err := fits.Validate(); err != nil {
		t.Errorf("Validate refused an entry at the limit: %v", err)
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
	if !permits.Match("fs_read", nil) || !permits.Match("ffmpeg_transcode", nil) {
		t.Errorf("permits.tools = %v, which does not carry the declared globs", permits.Tools)
	}
	if permits.Match("nounderscore", nil) {
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
	if got := plain.Permits(); got.Match("fs_read", nil) || got.AnyPath() {
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
	if empty.Match("fs_read", nil) || empty.AnyPath() {
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
