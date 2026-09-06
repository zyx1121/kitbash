package pkg_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/pkg"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
)

// containerManifest is a Package that builds its own folder.
const containerManifest = `name: ffmpeg
description: Transcode and probe media files. Use for any audio or video conversion.
deploy:
  units:
    - type: container
      build: .
      expose: mcp
`

// imageManifest is a Package that names an existing image instead of a build
// context. Version 1 refuses to build it.
const imageManifest = `name: alpine
description: An existing image pinned by digest, which version 1 cannot build.
deploy:
  units:
    - type: container
      image: docker.io/library/alpine@sha256:0000000000000000000000000000000000000000000000000000000000000000
`

type fixture struct {
	files    *fs.Service
	runner   *podman.Fake
	packages *pkg.Service
	root     string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	files, err := fs.New("tester", []string{root})
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	runner := podman.NewFake()
	return &fixture{
		files:    files,
		runner:   runner,
		packages: pkg.New(files, runner, nil),
		root:     root,
	}
}

// commit writes a folder through fs, so it is committed the way an agent's
// writes are.
func (f *fixture) commit(t *testing.T, name string, files ...fs.File) string {
	t.Helper()
	folder := filepath.Join(f.root, name)
	if _, prob := f.files.WriteFiles(context.Background(), folder, files, "Add "+name); prob != nil {
		t.Fatalf("writing %s: %s", name, prob.Detail)
	}
	return folder
}

func text(s string) *string { return &s }

func TestBuildStampsTheImageAndTagsTheCommit(t *testing.T) {
	f := newFixture(t)
	folder := f.commit(t, "ffmpeg",
		fs.File{Path: "kitbash.yaml", Content: text(containerManifest)},
		fs.File{Path: "Containerfile", Content: text("FROM alpine\n")})

	out, prob := f.packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if !strings.HasPrefix(out.Digest, "sha256:") || len(out.Digest) != len("sha256:")+64 {
		t.Errorf("digest is %q, want sha256 and 64 hex characters", out.Digest)
	}
	if len(out.Commit) != 40 {
		t.Errorf("commit is %q, want a 40 character sha", out.Commit)
	}
	if out.Log == "" {
		t.Error("the build returned no log tail")
	}

	if len(f.runner.Builds) != 1 {
		t.Fatalf("the runtime was asked for %d builds, want 1", len(f.runner.Builds))
	}
	build := f.runner.Builds[0]
	if build.ContextDir != folder {
		t.Errorf("the build context is %q, want %q", build.ContextDir, folder)
	}
	if build.Containerfile != filepath.Join(folder, "Containerfile") {
		t.Errorf("the containerfile is %q, want the one in the context", build.Containerfile)
	}
	wantTag := pkg.TagPrefix + "ffmpeg:" + out.Commit[:12]
	if build.Tag != wantTag {
		t.Errorf("the tag is %q, want %q", build.Tag, wantTag)
	}
	want := map[string]string{
		podman.LabelPath:   folder,
		podman.LabelName:   "ffmpeg",
		podman.LabelCommit: out.Commit,
		podman.LabelUser:   "tester",
	}
	for k, v := range want {
		if build.Labels[k] != v {
			t.Errorf("label %s is %q, want %q", k, build.Labels[k], v)
		}
	}
}

func TestBuildPrefersContainerfileOverDockerfile(t *testing.T) {
	f := newFixture(t)
	folder := f.commit(t, "ffmpeg",
		fs.File{Path: "kitbash.yaml", Content: text(containerManifest)},
		fs.File{Path: "Containerfile", Content: text("FROM alpine\n")},
		fs.File{Path: "Dockerfile", Content: text("FROM debian\n")})

	if _, prob := f.packages.Build(context.Background(), folder); prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if got := filepath.Base(f.runner.Builds[0].Containerfile); got != "Containerfile" {
		t.Errorf("the build read %s, want Containerfile", got)
	}
}

func TestBuildRefusesADirtyTree(t *testing.T) {
	f := newFixture(t)
	folder := f.commit(t, "ffmpeg",
		fs.File{Path: "kitbash.yaml", Content: text(containerManifest)},
		fs.File{Path: "Containerfile", Content: text("FROM alpine\n")})
	if err := os.WriteFile(filepath.Join(folder, "notes.md"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, prob := f.packages.Build(context.Background(), folder)
	if prob == nil {
		t.Fatal("a dirty tree was built")
	}
	if prob.Slug() != problem.SlugConflict {
		t.Errorf("problem is %s, want conflict", prob.Slug())
	}
	if !strings.Contains(prob.Detail, "kitbash builds from a commit") {
		t.Errorf("detail is %q, want it to say kitbash builds from a commit", prob.Detail)
	}
	if len(f.runner.Builds) != 0 {
		t.Error("the runtime was asked to build a dirty tree")
	}
}

func TestBuildRefusesAnImageUnit(t *testing.T) {
	f := newFixture(t)
	folder := f.commit(t, "alpine", fs.File{Path: "kitbash.yaml", Content: text(imageManifest)})

	_, prob := f.packages.Build(context.Background(), folder)
	if prob == nil {
		t.Fatal("an image unit was built")
	}
	if prob.Slug() != problem.SlugInvalidManifest {
		t.Errorf("problem is %s, want invalid-manifest", prob.Slug())
	}
	if !strings.Contains(prob.Detail, "version 1 builds container units with a build context") {
		t.Errorf("detail is %q, want the version 1 sentence", prob.Detail)
	}
}

// A build that ran and failed is the caller's to fix, so the tail of the log
// comes back with the error.
func TestBuildFailureReturnsTheLogTail(t *testing.T) {
	f := newFixture(t)
	folder := f.commit(t, "ffmpeg",
		fs.File{Path: "kitbash.yaml", Content: text(containerManifest)},
		fs.File{Path: "Containerfile", Content: text("FROM alpine\nRUN exit 1\n")})
	f.runner.Log = "STEP 1: FROM alpine\nSTEP 2: RUN exit 1\nerror: exit status 1\n"
	f.runner.BuildErr = fmt.Errorf("podman build: exit status 1: %w", podman.ErrBuildFailed)

	_, prob := f.packages.Build(context.Background(), folder)
	if prob == nil {
		t.Fatal("a failed build was reported as a success")
	}
	if prob.Slug() != problem.SlugBadRequest {
		t.Fatalf("problem is %s, want bad-request", prob.Slug())
	}
	if prob.Status != 400 {
		t.Errorf("status is %d, want 400", prob.Status)
	}
	if prob.Instance != folder {
		t.Errorf("instance is %q, want %q", prob.Instance, folder)
	}
	if !strings.Contains(prob.Detail, "RUN exit 1") {
		t.Errorf("detail is %q, want the tail of the build log", prob.Detail)
	}
	if prob.Fix != "Fix the build context and call pkg_build again." {
		t.Errorf("fix is %q, want the build context advice", prob.Fix)
	}
}

// The build log is bounded the same way whether the build worked or not.
func TestBuildFailureLogTailIsBounded(t *testing.T) {
	f := newFixture(t)
	folder := f.commit(t, "ffmpeg",
		fs.File{Path: "kitbash.yaml", Content: text(containerManifest)},
		fs.File{Path: "Containerfile", Content: text("FROM alpine\n")})
	var log strings.Builder
	for i := range 200 {
		fmt.Fprintf(&log, "step %d\n", i)
	}
	f.runner.Log = log.String()
	f.runner.BuildErr = fmt.Errorf("podman build: exit status 1: %w", podman.ErrBuildFailed)

	_, prob := f.packages.Build(context.Background(), folder)
	if prob == nil {
		t.Fatal("a failed build was reported as a success")
	}
	if lines := strings.Count(prob.Detail, "\n") + 1; lines > pkg.LogTailLines {
		t.Errorf("the detail carries %d lines, want at most %d", lines, pkg.LogTailLines)
	}
	if len(prob.Detail) > pkg.LogTailBytes {
		t.Errorf("the detail is %d bytes, want at most %d", len(prob.Detail), pkg.LogTailBytes)
	}
	if !strings.Contains(prob.Detail, "step 199") {
		t.Errorf("detail is %q, want the end of the log", prob.Detail)
	}
}

// A runtime that will not run at all is not the caller's fault, so the cause
// stays in the server log.
func TestBuildWithoutARuntimeIsInternal(t *testing.T) {
	f := newFixture(t)
	folder := f.commit(t, "ffmpeg",
		fs.File{Path: "kitbash.yaml", Content: text(containerManifest)},
		fs.File{Path: "Containerfile", Content: text("FROM alpine\n")})
	f.runner.BuildErr = errors.New(`exec: "podman": executable file not found in $PATH`)

	_, prob := f.packages.Build(context.Background(), folder)
	if prob == nil {
		t.Fatal("a missing runtime was reported as a success")
	}
	if prob.Slug() != problem.SlugInternal {
		t.Errorf("problem is %s, want internal", prob.Slug())
	}
	if strings.Contains(prob.Detail, "podman") {
		t.Errorf("detail is %q, want the runtime's own words kept out of it", prob.Detail)
	}
}

func TestBuildRefusesAContextWithoutAContainerfile(t *testing.T) {
	f := newFixture(t)
	folder := f.commit(t, "ffmpeg", fs.File{Path: "kitbash.yaml", Content: text(containerManifest)})

	_, prob := f.packages.Build(context.Background(), folder)
	if prob == nil {
		t.Fatal("a context without a Containerfile was built")
	}
	if prob.Slug() != problem.SlugNotFound {
		t.Errorf("problem is %s, want not-found", prob.Slug())
	}
	if prob.Fix == "" {
		t.Error("the problem carries no fix")
	}
}

func TestBuildRefusesAContextOutsideTheFolder(t *testing.T) {
	f := newFixture(t)
	folder := f.commit(t, "escape", fs.File{Path: "kitbash.yaml", Content: text(`name: escape
description: A Package whose build context tries to reach outside its own tree.
deploy:
  units:
    - type: container
      build: ../elsewhere
`)})

	_, prob := f.packages.Build(context.Background(), folder)
	if prob == nil {
		t.Fatal("a build context outside the Package folder was accepted")
	}
	if prob.Slug() != problem.SlugInvalidManifest {
		t.Errorf("problem is %s, want invalid-manifest", prob.Slug())
	}
}

func TestListJoinsPackagesWithTheirNewestBuild(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.commit(t, "ffmpeg",
		fs.File{Path: "kitbash.yaml", Content: text(containerManifest)},
		fs.File{Path: "Containerfile", Content: text("FROM alpine\n")})
	f.commit(t, "handbook", fs.File{Path: "kitbash.yaml", Content: text(`name: handbook
description: How this organization works, which is knowledge and not a Package.
`)})

	first, prob := f.packages.Build(ctx, folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	second, prob := f.packages.Build(ctx, folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if first.Digest == second.Digest {
		t.Fatal("two builds returned the same digest, so newest first cannot be asserted")
	}
	f.runner.AddContainer(podman.Container{
		Name:  "kitbash-ffmpeg-ffmpeg",
		State: podman.StateRunning,
		Labels: map[string]string{
			podman.LabelUser:    "tester",
			podman.LabelPackage: folder,
		},
	})

	out, prob := f.packages.List(ctx)
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	if len(out.Packages) != 1 {
		t.Fatalf("List returned %+v, want the one Package", out.Packages)
	}
	entry := out.Packages[0]
	if entry.Name != "ffmpeg" || entry.Path != folder {
		t.Errorf("entry is %+v, want ffmpeg at %s", entry, folder)
	}
	if entry.Digest != second.Digest {
		t.Errorf("digest is %s, want the newest build %s", entry.Digest, second.Digest)
	}
	if entry.BuiltAt == "" {
		t.Error("the entry carries no build time")
	}
	if entry.Running != 1 {
		t.Errorf("running is %d, want 1", entry.Running)
	}
}

func TestInspectReturnsBuildsNewestFirst(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.commit(t, "ffmpeg",
		fs.File{Path: "kitbash.yaml", Content: text(containerManifest)},
		fs.File{Path: "Containerfile", Content: text("FROM alpine\n")})

	first, _ := f.packages.Build(ctx, folder)
	second, _ := f.packages.Build(ctx, folder)

	out, prob := f.packages.Inspect(ctx, folder)
	if prob != nil {
		t.Fatalf("Inspect: %s", prob.Detail)
	}
	if out.Manifest["name"] != "ffmpeg" {
		t.Errorf("the manifest is %v, want the one at the folder", out.Manifest)
	}
	if len(out.Builds) != 2 {
		t.Fatalf("Inspect returned %d builds, want 2", len(out.Builds))
	}
	if out.Builds[0].Digest != second.Digest || out.Builds[1].Digest != first.Digest {
		t.Errorf("builds are %+v, want the newest first", out.Builds)
	}
	if out.Builds[0].Commit != second.Commit {
		t.Errorf("the build records commit %s, want %s", out.Builds[0].Commit, second.Commit)
	}
}

func TestInspectRefusesAnInvisibleFolder(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Join(f.root, "nomanifest"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, prob := f.packages.Inspect(context.Background(), filepath.Join(f.root, "nomanifest"))
	if prob == nil {
		t.Fatal("a folder outside the surface was inspected")
	}
	if prob.Slug() != problem.SlugNotVisible {
		t.Errorf("problem is %s, want not-visible", prob.Slug())
	}
}
