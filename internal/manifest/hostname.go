package manifest

import (
	"regexp"
	"strings"
)

// A unit with expose: http may declare one host name of its own, which
// kitbashd serves in place of the <name>.<member>.<domain> it would otherwise
// derive, see PLAN.md section 2.3. It lives here beside ValidSecretName
// because it is manifest vocabulary: the unit declares the name, the daemon
// routes by it, and two spellings of the rule would be two rules.

// MaxHostname is the longest host name there is, in bytes. It is the length
// of a DNS name in presentation form, which is what a Host header carries.
const MaxHostname = 253

// MaxHostLabel is the longest one label may be.
const MaxHostLabel = 63

// hostLabel is one label of a host name kitbash serves: lower case letters,
// digits and hyphens, never at an end. Upper case is not accepted rather than
// folded, because the name a member declares is the name kitbashd obtains a
// certificate for and the name it matches a Host against, and one spelling is
// easier to reason about than a rule for making two into one.
var hostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ValidHostname reports whether name is a host name kitbashd routes by: a
// lower case DNS name of at least two labels, each of them valid, with no
// trailing dot and no more than MaxHostname bytes in all.
//
// Two labels are the floor because a name kitbash serves sits in a domain; a
// bare label is not a name the internet resolves. Everything a Host header can
// carry that is not a name, a path, a port, an at sign or a space among them,
// is refused by the label pattern rather than by a rule of its own.
func ValidHostname(name string) bool {
	if name == "" || len(name) > MaxHostname {
		return false
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) > MaxHostLabel || !hostLabel.MatchString(label) {
			return false
		}
	}
	return true
}
