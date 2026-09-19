// Package manifest parses and validates kitbash.yaml, the one manifest defined
// by spec/manifest.schema.json.
package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"gopkg.in/yaml.v3"

	"github.com/zyx1121/kitbash/internal/safeopen"
	"github.com/zyx1121/kitbash/spec"
)

// FileName is the manifest file every visible folder carries.
const FileName = "kitbash.yaml"

// MaxBytes caps kitbash.yaml. A manifest is metadata, and reading an arbitrarily
// large file on every visibility check is a denial of service.
const MaxBytes = 64 << 10

// Manifest is a parsed kitbash.yaml. Fields the core reads are typed; the whole
// document is kept in Raw so the MCP surface can return it verbatim.
type Manifest struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Tags        []string       `json:"tags,omitempty"`
	Raw         map[string]any `json:"-"`
}

// IsPackage reports whether the manifest carries a deploy block.
func (m *Manifest) IsPackage() bool {
	_, ok := m.Raw["deploy"]
	return ok
}

var (
	compileOnce sync.Once
	compiled    *jsonschema.Schema
	compileErr  error
)

func schema() (*jsonschema.Schema, error) {
	compileOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(spec.ManifestSchema))
		if err != nil {
			compileErr = fmt.Errorf("reading embedded manifest schema: %w", err)
			return
		}
		c := NewCompiler()
		if err := c.AddResource(spec.ManifestSchemaURL, doc); err != nil {
			compileErr = fmt.Errorf("adding embedded manifest schema: %w", err)
			return
		}
		compiled, compileErr = c.Compile(spec.ManifestSchemaURL)
	})
	return compiled, compileErr
}

// ErrInvalid is returned by Parse when the document does not satisfy
// spec/manifest.schema.json. Its message lists the validator's complaints.
type ErrInvalid struct {
	Messages []string
}

func (e *ErrInvalid) Error() string {
	return strings.Join(e.Messages, "; ")
}

// Parse decodes and validates one kitbash.yaml document.
func Parse(data []byte) (*Manifest, error) {
	if len(data) > MaxBytes {
		return nil, &ErrInvalid{Messages: []string{
			fmt.Sprintf("kitbash.yaml is %d bytes, over the %d byte limit", len(data), MaxBytes)}}
	}
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, &ErrInvalid{Messages: []string{"kitbash.yaml is not valid YAML: " + err.Error()}}
	}
	if doc == nil {
		return nil, &ErrInvalid{Messages: []string{"kitbash.yaml is empty"}}
	}
	// Round trip through JSON so the validator sees only JSON types.
	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, &ErrInvalid{Messages: []string{"kitbash.yaml is not representable as JSON: " + err.Error()}}
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, &ErrInvalid{Messages: []string{"kitbash.yaml is not representable as JSON: " + err.Error()}}
	}

	s, err := schema()
	if err != nil {
		return nil, err
	}
	if err := s.Validate(value); err != nil {
		return nil, &ErrInvalid{Messages: validationMessages(err)}
	}

	raw, ok := value.(map[string]any)
	if !ok {
		return nil, &ErrInvalid{Messages: []string{"kitbash.yaml must be a mapping"}}
	}
	// The rules the schema cannot express are read here, so a manifest is
	// invalid wherever it is parsed rather than only where a Process is run:
	// fs_write validates before it commits, and a name a unit may not declare
	// is refused there too, see checkSecrets.
	if messages := checkSecrets(raw); len(messages) > 0 {
		return nil, &ErrInvalid{Messages: messages}
	}
	m := &Manifest{Raw: raw}
	m.Name, _ = raw["name"].(string)
	m.Description, _ = raw["description"].(string)
	if tags, ok := raw["tags"].([]any); ok {
		for _, t := range tags {
			if s, ok := t.(string); ok {
				m.Tags = append(m.Tags, s)
			}
		}
	}
	return m, nil
}

// englishPrinter renders validator complaints in English, matching the rest of
// the surface.
var englishPrinter = message.NewPrinter(language.English)

// validationMessages flattens a jsonschema error into caller readable lines.
func validationMessages(err error) []string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []string{err.Error()}
	}
	seen := map[string]bool{}
	var out []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			loc := e.InstanceLocation
			line := e.ErrorKind.LocalizedString(englishPrinter)
			msg := line
			if len(loc) > 0 {
				msg = "/" + strings.Join(loc, "/") + ": " + line
			}
			if !seen[msg] {
				seen[msg] = true
				out = append(out, msg)
			}
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	sort.Strings(out)
	if len(out) == 0 {
		out = []string{ve.Error()}
	}
	return out
}

// ErrNotRegular is returned by Load when kitbash.yaml is not a regular file: a
// symlink, a FIFO, a device. kitbash follows no symlinks, so a folder whose
// manifest is a link to a manifest elsewhere is not visible, and a manifest
// that would block the read is refused rather than waited on.
var ErrNotRegular = errors.New("kitbash.yaml is not a regular file")

// Load reads and validates the manifest of one folder, refusing to read more
// than MaxBytes of it. The folder is its own root, which is all a caller
// outside the fs family can say about it.
func Load(dir string) (*Manifest, error) { return LoadBelow(dir, ".") }

// LoadBelow reads the manifest of the folder rel below root. Everything from
// root down is resolved by safeopen, so no component of rel, and not the
// manifest itself, may be a symlink.
//
// Refusing a symlinked manifest is not a detail: it keeps a manifest from
// lending its name and description to a folder that carries none, which would
// put that folder on the surface describing contents the caller never named.
// Refusing a FIFO keeps a manifest from hanging the open, which every
// visibility check would then wait on.
func LoadBelow(root, rel string) (*Manifest, error) {
	path := filepath.Join(root, rel, FileName)
	f, err := safeopen.Open(root, filepath.Join(rel, FileName), os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w", path, ErrNotRegular)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Visible reports whether a folder is visible: it carries a kitbash.yaml that
// parses, validates, and has both name and description. The manifest is
// returned when it is visible.
func Visible(dir string) (*Manifest, bool) { return VisibleBelow(dir, ".") }

// VisibleChain is the whole visibility rule: a folder is on the surface when
// it, or any folder between it and its root, carries a manifest with a name
// and a description, and the nearest of those manifests is the one that speaks
// for it. The folders inside a Package, src, public, deploy, are part of what
// the Package's manifest describes and need none of their own; a nested
// kitbash.yaml only begins a new description where one is wanted. A folder
// with no manifest anywhere above it does not exist as far as the surface is
// concerned, see PLAN.md section 2.1.
//
// It answers the nearest manifest and the folder that carries it, relative to
// the root, so a caller knows which folder is speaking. When there is none it
// answers the top level folder the walk ended at instead, because that is the
// folder a manifest has to be written into for anything below it to be
// readable.
//
// The root itself carries no manifest and is not part of the chain: a root is
// a folder the host configured, not one a caller wrote. rel of "." therefore
// names no folder at all, which is not visible and has nothing to blame.
//
// It is one function because it is one rule: the fs family reads by it and a
// Process is given a folder by it, and two implementations of it would be two
// rules, see internal/mounts.
func VisibleChain(root, rel string) (m *Manifest, folder string, ok bool) {
	return VisibleChainWith(root, rel, nil)
}

// VisibleChainWith is VisibleChain with the manifests a call is about to write
// counted as present. carries reports whether a folder, named relative to the
// root, gets a manifest from this call; a nil carries is the rule read against
// the filesystem alone.
//
// It exists because one fs_write may hold the manifest of a Package and the
// files beneath it, and the folder those files are in is visible because of a
// manifest in the same call, see internal/fs. It is the same walk rather than
// a second rule.
func VisibleChainWith(root, rel string, carries func(rel string) bool) (m *Manifest, folder string, ok bool) {
	rel = filepath.Clean(rel)
	if rel == "." || rel == "" || rel == string(filepath.Separator) {
		return nil, ".", false
	}
	current := rel
	for {
		if carries != nil && carries(current) {
			// The manifest is not on disk yet, so there is none to read; what
			// this says is that the folder is visible and which folder says so.
			return nil, current, true
		}
		if m, held := VisibleBelow(root, current); held {
			return m, current, true
		}
		parent := filepath.Dir(current)
		if parent == "." || parent == string(filepath.Separator) {
			// current is the top level folder, which is where a manifest has
			// to go for anything below it to be on the surface.
			return nil, current, false
		}
		current = parent
	}
}

// VisibleBelow is Visible for a folder named below a root, which is how the fs
// family asks: the root is the folder the caller may not resolve out of.
func VisibleBelow(root, rel string) (*Manifest, bool) {
	m, err := LoadBelow(root, rel)
	if err != nil {
		return nil, false
	}
	if m.Name == "" || m.Description == "" {
		return nil, false
	}
	return m, true
}
