package pkg_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/problem"
)

// A build context that reaches outside the Package folder through a symlink
// looks like a path inside it, so the check walks the components instead of
// comparing strings. Rule 4 of PLAN.md section 2.5: a Package cannot reach
// outside its own tree at build time.
func TestBuildContextCannotEscapeThroughASymlink(t *testing.T) {
	f := newFixture(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "Containerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	folder := f.commit(t, "escape", fs.File{Path: "kitbash.yaml", Content: text(`name: escape
description: A Package whose build context is a link to somewhere else on this host.
deploy:
  units:
    - type: container
      build: ctx
`)})
	if err := os.Symlink(outside, filepath.Join(folder, "ctx")); err != nil {
		t.Skipf("this filesystem does not do symlinks: %v", err)
	}

	_, prob := f.packages.Build(context.Background(), folder)
	if prob == nil {
		t.Fatal("a build context outside the Package folder was accepted")
	}
	if prob.Slug() != problem.SlugInvalidManifest {
		t.Errorf("problem is %s, want invalid-manifest", prob.Slug())
	}
	if !strings.Contains(prob.Detail, "symlink") {
		t.Errorf("detail is %q, want it to name the symlink", prob.Detail)
	}
	if len(f.runner.Builds) != 0 {
		t.Errorf("the runtime was asked to build %+v", f.runner.Builds)
	}
}

// The same rule applies one level down: the Containerfile itself is not read
// through a link out of the folder.
func TestContainerfileIsNotReadThroughASymlink(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(t.TempDir(), "Containerfile")
	if err := os.WriteFile(outside, []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	folder := f.commit(t, "ffmpeg", fs.File{Path: "kitbash.yaml", Content: text(containerManifest)})
	if err := os.Symlink(outside, filepath.Join(folder, "Containerfile")); err != nil {
		t.Skipf("this filesystem does not do symlinks: %v", err)
	}

	_, prob := f.packages.Build(context.Background(), folder)
	if prob == nil {
		t.Fatal("a Containerfile outside the Package folder was read")
	}
	if prob.Slug() != problem.SlugNotFound {
		t.Errorf("problem is %s, want not-found", prob.Slug())
	}
	if len(f.runner.Builds) != 0 {
		t.Errorf("the runtime was asked to build %+v", f.runner.Builds)
	}
}

// A build context of a single dot is the Package folder itself, which is what
// most manifests write.
func TestBuildContextDotIsTheFolder(t *testing.T) {
	f := newFixture(t)
	folder := f.commit(t, "ffmpeg",
		fs.File{Path: "kitbash.yaml", Content: text(containerManifest)},
		fs.File{Path: "Containerfile", Content: text("FROM alpine\n")})

	if _, prob := f.packages.Build(context.Background(), folder); prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if got := f.runner.Builds[0].ContextDir; got != folder {
		t.Errorf("the build context is %q, want the Package folder %q", got, folder)
	}
}
