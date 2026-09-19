package manifest_test

import (
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/manifest"
)

// A host name is what the reverse proxy matches a Host header against and what
// kitbashd obtains a certificate for, so what is not a name is refused here
// rather than somewhere further in, see PLAN.md section 2.3.
func TestValidHostname(t *testing.T) {
	cases := []struct {
		name  string
		valid bool
		why   string
	}{
		{"app.loki.kitbash.example", true, "the shape of a derived name"},
		{"status.example.org", true, "a name a unit declares"},
		{"a.b", true, "two single character labels are a name"},
		{"a-b.c-d.example", true, "a hyphen inside a label"},
		{"1app.2loki.example", true, "a label may start with a digit"},

		{"", false, "nothing is not a name"},
		{"app", false, "a bare label is not a name the internet resolves"},
		{"app.loki.example.", false, "a trailing dot is not the presentation this proxy matches"},
		{"App.Loki.Example", false, "upper case is not folded here: one spelling is the rule"},
		{"app..example", false, "an empty label"},
		{"-app.example", false, "a label may not start with a hyphen"},
		{"app-.example", false, "a label may not end with a hyphen"},
		{"app_1.example", false, "an underscore is not a host name character"},
		{"app.example/etc/passwd", false, "a path in a name is a header injection, not a name"},
		{"evil.example@good.example", false, "an at sign is a header injection, not a name"},
		{"app.example:8080", false, "a port is not part of the name"},
		{"app example.org", false, "a space is not a host name character"},
		{"app.example\n", false, "a line break is not a host name character"},
		{"127.0.0.1", true, "four numeric labels are a name by shape; the routing table is what refuses it"},
	}
	for _, c := range cases {
		if got := manifest.ValidHostname(c.name); got != c.valid {
			t.Errorf("ValidHostname(%q) = %v, want %v: %s", c.name, got, c.valid, c.why)
		}
	}
}

// The two lengths are the DNS ones, and a name over either is refused: a
// certificate is never obtained for a name no resolver would answer.
func TestValidHostnameLengths(t *testing.T) {
	label := strings.Repeat("a", manifest.MaxHostLabel)
	if !manifest.ValidHostname(label + ".example") {
		t.Errorf("a label of %d characters is a label", manifest.MaxHostLabel)
	}
	if manifest.ValidHostname(label + "a.example") {
		t.Errorf("a label of %d characters was accepted, over the %d a label may be",
			manifest.MaxHostLabel+1, manifest.MaxHostLabel)
	}
	// Three labels of 63 and one of 61 is 253 characters with the dots, which
	// is the longest name there is; one more is not a name.
	longest := strings.Join([]string{label, label, label, strings.Repeat("b", 61)}, ".")
	if len(longest) != manifest.MaxHostname {
		t.Fatalf("the test name is %d characters, want %d", len(longest), manifest.MaxHostname)
	}
	if !manifest.ValidHostname(longest) {
		t.Errorf("a name of %d characters is a name", manifest.MaxHostname)
	}
	if manifest.ValidHostname(longest + "b") {
		t.Errorf("a name of %d characters was accepted, over the %d a name may be",
			manifest.MaxHostname+1, manifest.MaxHostname)
	}
}
