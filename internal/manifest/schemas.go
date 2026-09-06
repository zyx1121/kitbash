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
)

// MaxSchemaBytes caps one schema file. A tool schema is metadata, and the
// bridge reads every one of them when a session starts.
const MaxSchemaBytes = 64 << 10

// ResolveSchemas returns the manifest's tools with every
// {"$ref": "schemas/x.json"} replaced by the contents of that file. The file
// must live inside dir, which is the Package folder: a Package cannot reach
// outside its own tree, the same rule build contexts follow.
func (m *Manifest) ResolveSchemas(dir string) ([]Tool, error) {
	tools := m.Tools()
	out := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		input, err := resolveSchema(dir, tool.Name, "input", tool.Input)
		if err != nil {
			return nil, err
		}
		output, err := resolveSchema(dir, tool.Name, "output", tool.Output)
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
func resolveSchema(dir, tool, field string, schema map[string]any) (map[string]any, error) {
	ref, ok := schema["$ref"].(string)
	if !ok {
		return schema, nil
	}
	where := fmt.Sprintf("tool %s %s schema", tool, field)
	if err := checkRef(dir, ref); err != nil {
		return nil, &ErrInvalid{Messages: []string{where + ": " + err.Error()}}
	}
	path := filepath.Join(dir, ref)
	f, err := os.Open(path)
	if err != nil {
		return nil, &ErrInvalid{Messages: []string{fmt.Sprintf("%s: %s cannot be read", where, ref)}}
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

// checkRef applies the path rules a schema reference lives under. They are the
// fs rules, stated again here because a manifest is data the core reads
// without the fs service in the way.
func checkRef(dir, ref string) error {
	if ref == "" || filepath.IsAbs(ref) {
		return fmt.Errorf("%q is not a relative path inside this folder", ref)
	}
	current := dir
	for _, segment := range strings.Split(filepath.ToSlash(ref), "/") {
		switch {
		case segment == "":
			return fmt.Errorf("%q has an empty path component", ref)
		case segment == "..":
			return fmt.Errorf("%q leaves this folder", ref)
		case strings.HasPrefix(segment, "."):
			return fmt.Errorf("%q has a component beginning with a dot, which is reserved", ref)
		}
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("%q does not exist", ref)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%q passes through a symlink, and kitbash does not follow symlinks", ref)
		}
	}
	return nil
}

// ValidateInput reports whether a JSON document satisfies the tool's input
// schema. A tool with no schema accepts anything, which the manifest schema
// already makes impossible.
func (t Tool) ValidateInput(data []byte) error { return validateAgainst(t.Input, data) }

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

func compile(schema map[string]any) (*jsonschema.Schema, error) {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("the schema is not representable as JSON: %w", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("the schema is not valid JSON: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource(toolSchemaURL, doc); err != nil {
		return nil, fmt.Errorf("the schema cannot be compiled: %w", err)
	}
	compiled, err := c.Compile(toolSchemaURL)
	if err != nil {
		return nil, fmt.Errorf("the schema cannot be compiled: %w", err)
	}
	return compiled, nil
}
