package pkg_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/pkg"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/proc"
)

// builderManifest is a Package whose container unit names the kit that builds
// it. The build context is still declared, because the kit is handed it: a
// Package cannot reach outside its own tree whoever does the building.
const builderManifest = `name: ffmpeg
description: Transcode and probe media files, built somewhere other than this host.
deploy:
  units:
    - type: container
      build: .
      builder: /org/nix-build
      expose: mcp
`

// buildDigest is what the fake build kit answers with.
const buildDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// buildKit is one running build kit whose build tool accepts the hook's
// arguments. The schema is the contract: dispatch validates against it before
// anything is called.
func buildKit(path string) proc.Kit {
	return proc.Kit{
		Process: &proc.Process{ID: path, Package: path, State: proc.StateRunning, Expose: manifest.ExposeMCP},
		Tool: manifest.Tool{
			Name: manifest.ToolBuild,
			Input: map[string]any{
				"type":     "object",
				"required": []any{"path", "context"},
				"properties": map[string]any{
					"path":    map[string]any{"type": "string"},
					"context": map[string]any{"type": "string"},
				},
			},
			Output: map[string]any{"type": "object"},
		},
	}
}

// buildFixture is a Package that names a builder, with the kits the session
// can reach.
func buildFixture(t *testing.T, kits *stubKits) (*pkg.Service, string) {
	t.Helper()
	f := newFixture(t)
	packages := pkg.New(f.files, f.runner, kits)
	folder := f.commit(t, "ffmpeg",
		fs.File{Path: "kitbash.yaml", Content: text(builderManifest)},
		fs.File{Path: "Containerfile", Content: text("FROM alpine\n")})
	return packages, folder
}

func TestBuildDispatchesToTheBuilderKit(t *testing.T) {
	answer, _ := json.Marshal(map[string]any{"digest": buildDigest, "log": "built by nix\n"})
	kits := &stubKits{
		at:     map[string]proc.Kit{"/org/nix-build": buildKit("/org/nix-build")},
		result: string(answer),
	}
	packages, folder := buildFixture(t, kits)

	out, prob := packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if out.Digest != buildDigest {
		t.Errorf("the digest is %q, want the one the kit answered", out.Digest)
	}
	if out.Log != "built by nix" {
		t.Errorf("the log is %q, want the tail of what the kit answered", out.Log)
	}
	if len(out.Commit) != 40 {
		t.Errorf("the commit is %q, want the commit the Package was built from", out.Commit)
	}
	if kits.tool != manifest.ToolBuild {
		t.Errorf("the kit was called on %q, want its build tool", kits.tool)
	}
	if kits.called["path"] != folder || kits.called["context"] != folder {
		t.Errorf("the kit was called with %v, want the Package folder and its build context", kits.called)
	}
}

func TestBuildWithABuilderDoesNotRunTheBuiltInPath(t *testing.T) {
	answer, _ := json.Marshal(map[string]any{"digest": buildDigest})
	kits := &stubKits{
		at:     map[string]proc.Kit{"/org/nix-build": buildKit("/org/nix-build")},
		result: string(answer),
	}
	f := newFixture(t)
	packages := pkg.New(f.files, f.runner, kits)
	folder := f.commit(t, "ffmpeg",
		fs.File{Path: "kitbash.yaml", Content: text(builderManifest)},
		fs.File{Path: "Containerfile", Content: text("FROM alpine\n")})

	if _, prob := packages.Build(context.Background(), folder); prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if len(f.runner.Builds) != 0 {
		t.Errorf("the container runtime was asked for %d builds, want none: the kit builds this Package",
			len(f.runner.Builds))
	}
}

func TestBuildWithoutABuilderStaysOnTheBuiltInPath(t *testing.T) {
	kits := &stubKits{at: map[string]proc.Kit{}}
	f := newFixture(t)
	packages := pkg.New(f.files, f.runner, kits)
	folder := f.commit(t, "ffmpeg",
		fs.File{Path: "kitbash.yaml", Content: text(containerManifest)},
		fs.File{Path: "Containerfile", Content: text("FROM alpine\n")})

	out, prob := packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if len(f.runner.Builds) != 1 {
		t.Fatalf("the container runtime was asked for %d builds, want the one the built in path makes",
			len(f.runner.Builds))
	}
	if kits.called != nil {
		t.Errorf("a kit was called with %v for a Package that names no builder", kits.called)
	}
	if out.Digest == "" {
		t.Error("the built in path returned no digest")
	}
}

func TestBuildWithABuilderThatIsNotRunning(t *testing.T) {
	packages, folder := buildFixture(t, &stubKits{at: map[string]proc.Kit{}})

	_, prob := packages.Build(context.Background(), folder)
	if prob == nil {
		t.Fatal("a Package whose builder is not running was built")
	}
	if prob.Slug() != problem.SlugNotFound {
		t.Errorf("problem is %s, want not-found", prob.Slug())
	}
	if !strings.Contains(prob.Fix, "proc_run") {
		t.Errorf("fix is %q, want it to say to run the kit first", prob.Fix)
	}
}

func TestBuildWithABuilderWhoseSchemaRefusesTheHook(t *testing.T) {
	kit := buildKit("/org/nix-build")
	// A build tool that takes a source is an import kit wearing the build
	// hook: it does not implement what the manifest named it for.
	kit.Tool.Input = map[string]any{
		"type":                 "object",
		"required":             []any{"source"},
		"additionalProperties": false,
		"properties":           map[string]any{"source": map[string]any{"type": "string"}},
	}
	packages, folder := buildFixture(t, &stubKits{at: map[string]proc.Kit{"/org/nix-build": kit}})

	_, prob := packages.Build(context.Background(), folder)
	if prob == nil {
		t.Fatal("a kit whose build tool refuses the hook was called")
	}
	if prob.Slug() != problem.SlugInvalidManifest {
		t.Errorf("problem is %s, want invalid-manifest", prob.Slug())
	}
}

func TestBuildWithABuilderThatAnswersNoDigest(t *testing.T) {
	answer, _ := json.Marshal(map[string]any{"digest": "not-an-image", "log": "done"})
	packages, folder := buildFixture(t, &stubKits{
		at:     map[string]proc.Kit{"/org/nix-build": buildKit("/org/nix-build")},
		result: string(answer),
	})

	_, prob := packages.Build(context.Background(), folder)
	if prob == nil {
		t.Fatal("a kit that answered with something that is not an image id was believed")
	}
	if prob.Slug() != problem.SlugInternal {
		t.Errorf("problem is %s, want internal", prob.Slug())
	}
	if !strings.Contains(prob.Fix, "author") {
		t.Errorf("fix is %q, want it to name the kit's author", prob.Fix)
	}
}

func TestBuildWithABuilderRefusesAContextOutsideTheFolder(t *testing.T) {
	const escaping = `name: ffmpeg
description: A Package whose build context reaches outside its own folder.
deploy:
  units:
    - type: container
      build: ../elsewhere
      builder: /org/nix-build
      expose: mcp
`
	answer, _ := json.Marshal(map[string]any{"digest": buildDigest})
	kits := &stubKits{
		at:     map[string]proc.Kit{"/org/nix-build": buildKit("/org/nix-build")},
		result: string(answer),
	}
	f := newFixture(t)
	packages := pkg.New(f.files, f.runner, kits)
	folder := f.commit(t, "ffmpeg", fs.File{Path: "kitbash.yaml", Content: text(escaping)})

	_, prob := packages.Build(context.Background(), filepath.Clean(folder))
	if prob == nil {
		t.Fatal("a build context outside the Package folder was handed to a kit")
	}
	if prob.Slug() != problem.SlugInvalidManifest {
		t.Errorf("problem is %s, want invalid-manifest", prob.Slug())
	}
	if kits.called != nil {
		t.Errorf("the kit was called with %v before the context was checked", kits.called)
	}
}
