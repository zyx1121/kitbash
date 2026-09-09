package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// MaxPermittedTools and MaxPermittedPaths are the bounds
// spec/manifest.schema.json puts on one permits block. They are repeated here
// because a permits block also arrives as KITBASH_PERMITS, which no validator
// has seen.
const (
	MaxPermittedTools = 64
	MaxPermittedPaths = 32
)

// EnvPermits carries one Process's permits block into the kitbash-mcp
// kitbashd starts for its MCP session, see mcp_for_processes in
// spec/kitbashd-api.yaml. The daemon writes it and the surface reads it; both
// spell it here, because the block and its meaning live in this package.
//
// It is honoured only beside EnvCaller, which only kitbashd sets: a member's
// own SSH session is narrowed by nothing, and setting this variable by hand in
// one is not a way to narrow, or to widen, that session.
const EnvPermits = "KITBASH_PERMITS"

// Permits is provides.permits: what a Process of this Package may call over
// /mcp, see PLAN.md section 2.3. It is a declaration and not a filter on top
// of a surface the Process would otherwise have: a Process whose Package
// declares no permits gets an empty surface, so the zero value permits
// nothing.
//
// Tools are surface names as tools/list publishes them, with * standing for
// any run of characters inside one name. Paths are absolute Files prefixes,
// each of which covers the path itself and everything below it, with * as one
// whole component.
type Permits struct {
	Tools []string `json:"tools,omitempty"`
	Paths []string `json:"paths,omitempty"`
}

// Permits returns provides.permits as the manifest declares it. A manifest
// with no block returns the zero value, which allows nothing.
func (m *Manifest) Permits() Permits {
	provides, ok := m.Raw["provides"].(map[string]any)
	if !ok {
		return Permits{}
	}
	raw, ok := provides["permits"].(map[string]any)
	if !ok {
		return Permits{}
	}
	return Permits{Tools: stringsOf(raw["tools"]), Paths: stringsOf(raw["paths"])}
}

// stringsOf reads a JSON array of strings, ignoring entries of another type.
// The schema has already refused those; this keeps a hand written block from
// panicking a reader.
func stringsOf(value any) []string {
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, entry := range list {
		if s, ok := entry.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ParsePermits reads a permits block as JSON, which is how it reaches
// kitbash-mcp in KITBASH_PERMITS. An empty document is the empty block, which
// permits nothing; a document this build cannot honour is an error, and the
// caller of a permits block it cannot read has to refuse everything rather
// than fall back to the surface it was meant to narrow.
func ParsePermits(data []byte) (Permits, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return Permits{}, nil
	}
	var p Permits
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return Permits{}, fmt.Errorf("manifest: the permits block is not readable: %w", err)
	}
	if err := p.Validate(); err != nil {
		return Permits{}, err
	}
	return p, nil
}

// JSON is the permits block as kitbashd hands it to a session child. An empty
// block is an object, not null: the child reads the variable as the whole
// declaration, and a null would be one more spelling of the same thing.
func (p Permits) JSON() []byte {
	encoded, err := json.Marshal(struct {
		Tools []string `json:"tools"`
		Paths []string `json:"paths"`
	}{Tools: listOf(p.Tools), Paths: listOf(p.Paths)})
	if err != nil {
		// Two string slices always marshal.
		return []byte(`{"tools":[],"paths":[]}`)
	}
	return encoded
}

// listOf keeps an absent list an empty array rather than null, so a reader
// never has to tell the two apart.
func listOf(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// Validate reports every entry this build will not honour. The schema checks
// the same shapes when a manifest is written; this is what checks a block that
// arrived as an environment variable, and what a test reads.
func (p Permits) Validate() error {
	var bad []string
	if len(p.Tools) > MaxPermittedTools {
		bad = append(bad, fmt.Sprintf("permits.tools names %d tools, over the limit of %d",
			len(p.Tools), MaxPermittedTools))
	}
	if len(p.Paths) > MaxPermittedPaths {
		bad = append(bad, fmt.Sprintf("permits.paths names %d prefixes, over the limit of %d",
			len(p.Paths), MaxPermittedPaths))
	}
	for _, glob := range p.Tools {
		if !validToolGlob(glob) {
			bad = append(bad, fmt.Sprintf("%q is not a tool name glob", glob))
		}
	}
	for _, prefix := range p.Paths {
		if !validPathPrefix(prefix) {
			bad = append(bad, fmt.Sprintf("%q is not an absolute path prefix", prefix))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("manifest: %s", strings.Join(bad, "; "))
}

// Match reports whether one surface tool name is permitted. A Package that
// declares no tools matches none, which is the empty surface of PLAN.md
// section 2.3.
func (p Permits) Match(tool string) bool {
	if tool == "" {
		return false
	}
	for _, glob := range p.Tools {
		if !validToolGlob(glob) {
			// A glob this build cannot read permits nothing. Honouring it as
			// a literal would turn a typo into a rule nobody wrote.
			continue
		}
		if globMatch(glob, tool) {
			return true
		}
	}
	return false
}

// Allows reports whether one absolute Files path is under a permitted prefix.
// A prefix covers the path itself and everything below it, so /org allows
// /org and /org/handbook/README.md alike.
//
// A path that is not absolute, or that carries a dot or a double dot
// component, is allowed by nothing: those are the paths the fs family refuses
// as invalid, and a prefix check that cleaned them first would be answering
// about a different path than the one the caller sent.
func (p Permits) Allows(path string) bool {
	parts, ok := components(path)
	if !ok {
		return false
	}
	for _, prefix := range p.Paths {
		if !validPathPrefix(prefix) {
			continue
		}
		want, ok := components(prefix)
		if !ok || len(want) > len(parts) {
			continue
		}
		matched := true
		for i, component := range want {
			if component == "*" {
				continue
			}
			if component != parts[i] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// AnyPath reports whether this block permits any path at all. A Process with
// tools permitted and no prefix may call nothing that takes a path, so the
// tools that take one are refused before their arguments are read.
func (p Permits) AnyPath() bool {
	for _, prefix := range p.Paths {
		if validPathPrefix(prefix) {
			return true
		}
	}
	return false
}

// components splits an absolute path into its components. A trailing slash is
// dropped; anything else that would not survive a round trip is refused.
func components(path string) ([]string, bool) {
	if !strings.HasPrefix(path, "/") {
		return nil, false
	}
	trimmed := strings.TrimRight(path, "/")
	if trimmed == "" {
		// The root is every path's prefix, which is not a narrowing at all.
		return nil, false
	}
	parts := strings.Split(strings.TrimPrefix(trimmed, "/"), "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, false
		}
	}
	return parts, true
}

// validToolGlob holds a tool glob to the characters MCP allows in a tool name,
// plus the wildcard.
func validToolGlob(glob string) bool {
	if glob == "" {
		return false
	}
	for _, r := range glob {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '*':
		default:
			return false
		}
	}
	return true
}

// validPathPrefix holds a prefix to an absolute path whose wildcards are whole
// components: /home/*/flows is a prefix, /home/al* is not, because a component
// half matched is a rule nobody can read at a glance.
func validPathPrefix(prefix string) bool {
	parts, ok := components(prefix)
	if !ok {
		return false
	}
	for _, part := range parts {
		if strings.Contains(part, "*") && part != "*" {
			return false
		}
	}
	return true
}

// globMatch matches one name against a glob whose * stands for any run of
// characters, the empty run included. It is iterative rather than recursive so
// a pattern of many wildcards costs the caller nothing to check.
func globMatch(pattern, name string) bool {
	var p, n int
	star, mark := -1, 0
	for n < len(name) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, n
			p++
		case p < len(pattern) && pattern[p] == name[n]:
			p++
			n++
		case star >= 0:
			// Give the last wildcard one more character and try again.
			p = star + 1
			mark++
			n = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
