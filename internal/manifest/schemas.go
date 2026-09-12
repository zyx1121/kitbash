package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/zyx1121/kitbash/internal/safeopen"
	"github.com/zyx1121/kitbash/internal/safepath"
)

// MaxSchemaBytes caps one schema file. A tool schema is metadata, and the
// bridge reads every one of them when a session starts.
const MaxSchemaBytes = 64 << 10

// ResolveSchemas returns the manifest's tools with every
// {"$ref": "schemas/x.json"} replaced by the contents of that file. The file
// must live inside dir, which is the Package folder: a Package cannot reach
// outside its own tree, the same rule build contexts follow.
//
// dir is its own root here, so a symlink above it is followed: that part of
// the path is whatever the caller resolved. ResolveSchemasBelow is the form
// that closes it, and it is what the bridge uses.
func (m *Manifest) ResolveSchemas(dir string) ([]Tool, error) {
	return m.ResolveSchemasBelow(dir, ".")
}

// ResolveSchemasBelow is ResolveSchemas for a Package folder named below a
// root: every component from the root down, the reference included, is
// resolved in one openat2, so no part of the path may be a symlink.
func (m *Manifest) ResolveSchemasBelow(root, dir string) ([]Tool, error) {
	tools := m.Tools()
	out := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		input, err := resolveSchema(root, dir, tool.Name, "input", tool.Input)
		if err != nil {
			return nil, err
		}
		output, err := resolveSchema(root, dir, tool.Name, "output", tool.Output)
		if err != nil {
			return nil, err
		}
		tool.Input = input
		tool.Output = output
		out = append(out, tool)
	}
	return out, nil
}

// resolveSchema inlines one schema reference, or returns the inline schema
// unchanged.
func resolveSchema(root, dir, tool, field string, schema map[string]any) (map[string]any, error) {
	ref, ok := schema["$ref"].(string)
	if !ok {
		return schema, nil
	}
	where := fmt.Sprintf("tool %s %s schema", tool, field)
	f, err := openSchema(root, dir, ref)
	if err != nil {
		return nil, &ErrInvalid{Messages: []string{where + ": " + err.Error()}}
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxSchemaBytes+1))
	if err != nil {
		return nil, &ErrInvalid{Messages: []string{fmt.Sprintf("%s: %s cannot be read", where, ref)}}
	}
	if len(data) > MaxSchemaBytes {
		return nil, &ErrInvalid{Messages: []string{fmt.Sprintf(
			"%s: %s is over the %d byte limit", where, ref, MaxSchemaBytes)}}
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, &ErrInvalid{Messages: []string{fmt.Sprintf("%s: %s is not valid JSON", where, ref)}}
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, &ErrInvalid{Messages: []string{fmt.Sprintf("%s: %s is not a JSON object", where, ref)}}
	}
	return object, nil
}

// openSchema opens one schema file inside the Package folder. The reference
// obeys the same path rules as every other path the surface takes: safepath
// applies the lexical ones and names what is wrong, safeopen resolves the file
// itself so no component of the reference may be a symlink at the moment of
// the open. The file has to be a regular file opened without blocking: a named
// pipe called schemas/x.json would otherwise hold a session open forever.
func openSchema(root, dir, ref string) (*os.File, error) {
	if ref == "." {
		return nil, fmt.Errorf("%q is not a schema file", ref)
	}
	if _, err := safepath.Inside(dir, ref); err != nil {
		return nil, err
	}
	f, err := safeopen.Open(root, filepath.Join(dir, ref), os.O_RDONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%q does not exist", ref)
		}
		return nil, fmt.Errorf("%q cannot be read", ref)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%q is not a regular file", ref)
	}
	return f, nil
}

// ValidateInput reports whether a JSON document satisfies the tool's input
// schema. A tool with no schema accepts anything, which the manifest schema
// already makes impossible.
func (t Tool) ValidateInput(data []byte) error { return validateAgainst(t.Input, data) }

// AcceptsArgs reports whether this tool's input schema accepts one set of hook
// arguments, and says why not when it does not. Dispatch to a kit binds on
// schemas and never on names, so a tool whose schema refuses what its hook is
// called with is a Package that does not implement that hook, see PLAN.md
// section 3.
func (t Tool) AcceptsArgs(args map[string]any) error {
	body, err := json.Marshal(args)
	if err != nil {
		return err
	}
	return t.ValidateInput(body)
}

// ValidateOutput reports whether a JSON document satisfies the tool's output
// schema.
func (t Tool) ValidateOutput(data []byte) error { return validateAgainst(t.Output, data) }

// validateAgainst compiles one schema and validates one document against it.
// Both sides are round tripped through the validator's own decoder so numbers
// carry the type it expects.
func validateAgainst(schema map[string]any, data []byte) error {
	if len(schema) == 0 {
		return nil
	}
	compiled, err := compile(schema)
	if err != nil {
		return err
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("the document is not valid JSON: %w", err)
	}
	if err := compiled.Validate(value); err != nil {
		return fmt.Errorf("%s", strings.Join(validationMessages(err), "; "))
	}
	return nil
}

// toolSchemaURL is the base every tool schema is compiled under. Schemas are
// self contained, so the identifier only has to be stable.
const toolSchemaURL = "https://kitbash.zyx.tw/tool-schema.json"

// noLoader refuses every reference a schema makes to somewhere else. The
// validator's default loader reads file:// and http:// URLs, so a tool schema
// could name a host file and have its contents echoed back in a validation
// message. A tool schema is self contained: the only references kitbash
// resolves are the {"$ref": "schemas/x.json"} in the manifest, which
// ResolveSchemas reads itself under the folder rules.
type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("this schema refers to %s, and kitbash resolves no references outside the schema itself", url)
}

// NewCompiler is the only compiler kitbash builds: 2020-12, and no reach off
// the page.
func NewCompiler() *jsonschema.Compiler {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(noLoader{})
	return c
}

func compile(schema map[string]any) (*jsonschema.Schema, error) {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("the schema is not representable as JSON: %w", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("the schema is not valid JSON: %w", err)
	}
	c := NewCompiler()
	if err := c.AddResource(toolSchemaURL, doc); err != nil {
		return nil, fmt.Errorf("the schema cannot be compiled: %w", err)
	}
	compiled, err := c.Compile(toolSchemaURL)
	if err != nil {
		return nil, fmt.Errorf("the schema cannot be compiled: %w", err)
	}
	return compiled, nil
}
