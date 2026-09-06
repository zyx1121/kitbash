package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A schema that names a host file used to be resolved by the validator's
// default loader, and the file's contents came back inside the validation
// message. Nothing outside the schema is loaded now.
func TestNestedRefIsRefused(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(outside, []byte(`{"enum":["s3cr3t-token-value"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		ref  string
	}{
		{name: "file", ref: "file://" + outside},
		{name: "http", ref: "http://127.0.0.1:1/schema.json"},
		{name: "relative to the schema", ref: "secret.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			schema := map[string]any{
				"type":       "object",
				"properties": map[string]any{"x": map[string]any{"$ref": tc.ref}},
			}
			err := validateAgainst(schema, []byte(`{"x":"wrong"}`))
			if err == nil {
				t.Fatal("a schema that reaches outside itself was compiled")
			}
			if strings.Contains(err.Error(), "s3cr3t-token-value") {
				t.Fatalf("the file was read and echoed back: %v", err)
			}
			if !strings.Contains(err.Error(), "cannot be compiled") {
				t.Errorf("error is %q, want the compiler refusing the reference", err)
			}
		})
	}
}

// A reference inside the schema document is how a real tool schema factors out
// a repeated shape, and it still resolves.
func TestInternalRefStillResolves(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{"$ref": "#/$defs/name"},
		},
		"$defs": map[string]any{"name": map[string]any{"type": "string"}},
	}
	if err := validateAgainst(schema, []byte(`{"a":"ok"}`)); err != nil {
		t.Errorf("a valid document was refused: %v", err)
	}
	if err := validateAgainst(schema, []byte(`{"a":1}`)); err == nil {
		t.Error("an invalid document was accepted, so the internal reference was not applied")
	}
}
