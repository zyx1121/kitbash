package pkg_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/pkg"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/proc"
)

// stubKits stands in for the MCP bridge: a set of running import kits, the one
// kit a manifest named, and whatever the kit that is called returns.
type stubKits struct {
	kits   []proc.Kit
	at     map[string]proc.Kit
	result string
	called map[string]any
	tool   string
}

func (s *stubKits) Kits(context.Context) ([]proc.Kit, *problem.Problem) {
	return s.kits, nil
}

// KitAt answers the way the bridge does: the kit running at that path, or the
// not-found that tells the caller to run it.
func (s *stubKits) KitAt(_ context.Context, path, hook, _ string) (*proc.Kit, *problem.Problem) {
	if kit, running := s.at[path]; running {
		return &kit, nil
	}
	return nil, problem.NotFoundFix(path,
		"the manifest names the "+hook+" kit at "+path+", and no Process of it is running",
		"Run the kit first with proc_run on "+path+", then call this tool again.")
}

func (s *stubKits) CallTool(_ context.Context, _ *proc.Process, tool string, args map[string]any) (json.RawMessage, *problem.Problem) {
	s.called = args
	s.tool = tool
	return json.RawMessage(s.result), nil
}

// kit builds one running import kit whose import tool accepts sources matching
// pattern.
func kit(path, pattern string) proc.Kit {
	return proc.Kit{
		Process: &proc.Process{ID: path, Package: path, State: proc.StateRunning, Expose: manifest.ExposeMCP},
		Tool: manifest.Tool{
			Name: manifest.ToolImport,
			Input: map[string]any{
				"type":     "object",
				"required": []any{"source"},
				"properties": map[string]any{
					"source": map[string]any{"type": "string", "pattern": pattern},
				},
			},
			Output: map[string]any{"type": "object"},
		},
	}
}

const importedManifest = `name: time
description: A wrapped MCP server that answers what the time is right now.
deploy:
  units:
    - type: container
      build: .
      expose: mcp
`

func newImporter(t *testing.T, kits *stubKits) (*pkg.Service, string) {
	t.Helper()
	f := newFixture(t)
	return pkg.New(f.files, f.runner, kits), f.root
}

func TestImportWritesTheKitsFilesAsOneCommit(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"files": []map[string]any{
		{"path": "kitbash.yaml", "content": importedManifest},
		{"path": "README.md", "content": "# time\n"},
		{"path": "src/Containerfile", "content": "FROM node:22-alpine\n"},
	}})
	kits := &stubKits{kits: []proc.Kit{kit("/org/import-mcp", "^npm:")}, result: string(body)}
	packages, root := newImporter(t, kits)
	target := filepath.Join(root, "time")

	out, prob := packages.Import(context.Background(), pkg.ImportRequest{Path: target, Source: "npm:time-mcp@1.0.0"})
	if prob != nil {
		t.Fatalf("Import: %s", prob.Detail)
	}
	if out.Path != target || len(out.Commit.Sha) != 40 {
		t.Errorf("Import returned %+v, want the folder and its commit", out)
	}
	if out.Commit.Message != "Import npm:time-mcp@1.0.0" {
		t.Errorf("the commit message is %q, want it to name the source", out.Commit.Message)
	}
	if kits.called["source"] != "npm:time-mcp@1.0.0" {
		t.Errorf("the kit was called with %v, want the source", kits.called)
	}
	for _, name := range []string{"kitbash.yaml", "README.md", filepath.Join("src", "Containerfile")} {
		if _, err := os.Stat(filepath.Join(target, name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}
}

func TestImportWithNoAcceptingKit(t *testing.T) {
	kits := &stubKits{kits: []proc.Kit{kit("/org/import-mcp", "^npm:")}}
	packages, root := newImporter(t, kits)

	_, prob := packages.Import(context.Background(), pkg.ImportRequest{Path: filepath.Join(root, "alpine"), Source: "oci://alpine@sha256:0"})
	if prob == nil {
		t.Fatal("a source no kit accepts was imported")
	}
	if prob.Slug() != problem.SlugNotFound {
		t.Errorf("problem is %s, want not-found", prob.Slug())
	}
	if !strings.Contains(prob.Fix, "import-mcp") {
		t.Errorf("fix is %q, want it to name an example kit", prob.Fix)
	}
}

func TestImportWithTwoAcceptingKits(t *testing.T) {
	kits := &stubKits{kits: []proc.Kit{
		kit("/org/import-mcp", "^npm:"),
		kit("/org/import-npm", "^npm:"),
	}}
	packages, root := newImporter(t, kits)

	_, prob := packages.Import(context.Background(), pkg.ImportRequest{Path: filepath.Join(root, "time"), Source: "npm:time-mcp@1.0.0"})
	if prob == nil {
		t.Fatal("two kits accepting the same source was not a conflict")
	}
	if prob.Slug() != problem.SlugConflict {
		t.Errorf("problem is %s, want conflict", prob.Slug())
	}
	if !strings.Contains(prob.Fix, "proc_stop") {
		t.Errorf("fix is %q, want it to say to stop one of them", prob.Fix)
	}
}

func TestImportRefusesAnExistingFolder(t *testing.T) {
	kits := &stubKits{kits: []proc.Kit{kit("/org/import-mcp", "^npm:")}}
	packages, root := newImporter(t, kits)
	target := filepath.Join(root, "time")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	_, prob := packages.Import(context.Background(), pkg.ImportRequest{Path: target, Source: "npm:time-mcp@1.0.0"})
	if prob == nil {
		t.Fatal("an existing folder was imported into")
	}
	if prob.Slug() != problem.SlugConflict {
		t.Errorf("problem is %s, want conflict", prob.Slug())
	}
}

func TestImportRequiresAManifestAmongTheFiles(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"files": []map[string]any{
		{"path": "README.md", "content": "# time\n"},
	}})
	kits := &stubKits{kits: []proc.Kit{kit("/org/import-mcp", "^npm:")}, result: string(body)}
	packages, root := newImporter(t, kits)
	target := filepath.Join(root, "time")

	_, prob := packages.Import(context.Background(), pkg.ImportRequest{Path: target, Source: "npm:time-mcp@1.0.0"})
	if prob == nil {
		t.Fatal("a folder without a manifest was imported")
	}
	if prob.Slug() != problem.SlugInvalidManifest {
		t.Errorf("problem is %s, want invalid-manifest", prob.Slug())
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("the folder was created even though the import was refused")
	}
}

func TestImportRefusesAKitThatEscapesTheFolder(t *testing.T) {
	cases := []struct{ name, path string }{
		{name: "parent", path: "../escape.md"},
		{name: "dot component", path: ".git/config"},
		{name: "absolute", path: "/etc/passwd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"files": []map[string]any{
				{"path": "kitbash.yaml", "content": importedManifest},
				{"path": tc.path, "content": "x\n"},
			}})
			kits := &stubKits{kits: []proc.Kit{kit("/org/import-mcp", "^npm:")}, result: string(body)}
			packages, root := newImporter(t, kits)

			_, prob := packages.Import(context.Background(), pkg.ImportRequest{Path: filepath.Join(root, "time"), Source: "npm:time-mcp@1.0.0"})
			if prob == nil {
				t.Fatalf("the kit path %q was accepted", tc.path)
			}
			if prob.Slug() != problem.SlugInvalidPath {
				t.Errorf("problem is %s, want invalid-path", prob.Slug())
			}
		})
	}
}
