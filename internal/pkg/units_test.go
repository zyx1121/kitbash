package pkg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/podman"
)

// A Package of more than one unit has one image per unit, because a pod runs a
// container per unit, see PLAN.md section 5.6. pkg_build builds every one of
// them and answers the face unit's digest, which is the one digest the surface
// has.

const twoUnitManifest = `name: board
description: A counter and the cache it keeps its count in, two units of one Package.
deploy:
  units:
    - name: web
      type: container
      build: .
      expose: http
      port: 8080
    - name: cache
      type: container
      image: docker.io/library/redis@sha256:1111111111111111111111111111111111111111111111111111111111111111
`

func TestBuildBuildsEveryUnitAndAnswersTheFace(t *testing.T) {
	f := newFixture(t)
	folder := f.commit(t, "board",
		fs.File{Path: "kitbash.yaml", Content: text(twoUnitManifest)},
		fs.File{Path: "Containerfile", Content: text("FROM alpine\n")})

	out, prob := f.packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if len(f.runner.Builds) != 2 {
		t.Fatalf("the runtime was asked for %d builds, want one per unit", len(f.runner.Builds))
	}
	byUnit := map[string]string{}
	for _, build := range f.runner.Builds {
		unit := build.Labels[podman.LabelUnit]
		if unit == "" {
			t.Fatalf("a build of a composed Package carries no unit label: %+v", build.Labels)
		}
		byUnit[unit] = build.Tag
		if build.Labels[podman.LabelPath] != folder || build.Labels[podman.LabelCommit] != out.Commit {
			t.Errorf("the %s build is labelled %+v, want this Package at this commit", unit, build.Labels)
		}
	}
	if len(byUnit) != 2 {
		t.Fatalf("the builds cover %d units, want web and cache", len(byUnit))
	}
	// Two units of one Package are two tags, so a member reading podman
	// images sees which image is which.
	if byUnit["web"] == byUnit["cache"] {
		t.Errorf("both units were tagged %s", byUnit["web"])
	}
	if !strings.Contains(byUnit["web"], "board-web:") {
		t.Errorf("the web unit is tagged %q, want the unit in the tag", byUnit["web"])
	}
	// The sidecar is an image unit, so its build is the generated one line
	// Containerfile, which is how kitbash pulls an image pinned by digest.
	cache := ""
	for _, build := range f.runner.Builds {
		if build.Labels[podman.LabelUnit] == "cache" {
			cache = build.Content
		}
	}
	if !strings.HasPrefix(cache, "FROM docker.io/library/redis@sha256:") {
		t.Errorf("the sidecar was built from %q, want one FROM line naming its digest", cache)
	}
	if out.Digest == "" {
		t.Fatal("the build answered no digest")
	}
}

// A Package of one unit is built exactly as it was: one build, no unit label,
// and the tag it has always had.
func TestBuildOfOneUnitIsUnchangedByPods(t *testing.T) {
	f := newFixture(t)
	folder := f.commit(t, "ffmpeg",
		fs.File{Path: "kitbash.yaml", Content: text(containerManifest)},
		fs.File{Path: "Containerfile", Content: text("FROM alpine\n")})

	out, prob := f.packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if len(f.runner.Builds) != 1 {
		t.Fatalf("the runtime was asked for %d builds, want one", len(f.runner.Builds))
	}
	build := f.runner.Builds[0]
	if _, labelled := build.Labels[podman.LabelUnit]; labelled {
		t.Errorf("a Package of one unit was labelled with a unit: %+v", build.Labels)
	}
	if !strings.HasSuffix(build.Tag, "ffmpeg:"+out.Commit[:12]) {
		t.Errorf("the tag is %q, want the one a Package of one unit has always had", build.Tag)
	}
}

// pkg_inspect says what a Package is made of: every unit the manifest
// declares, what each one runs, the image the last build left of it, and the
// one unit that is the Process's face. It is what an agent reads before it
// asks proc_logs for a unit by name, see PLAN.md section 5.6.
func TestInspectListsTheUnitsAndTheirImages(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.commit(t, "board",
		fs.File{Path: "kitbash.yaml", Content: text(twoUnitManifest)},
		fs.File{Path: "Containerfile", Content: text("FROM alpine\n")})
	if _, prob := f.packages.Build(ctx, folder); prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}

	out, prob := f.packages.Inspect(ctx, folder)
	if prob != nil {
		t.Fatalf("Inspect: %s", prob.Detail)
	}
	if len(out.Units) != 2 {
		t.Fatalf("Inspect answered %d units, want the two the manifest declares: %+v", len(out.Units), out.Units)
	}
	// The order is the manifest's, so a reader sees the Package as it is
	// written rather than sorted into another shape.
	if out.Units[0].Name != "web" || out.Units[1].Name != "cache" {
		t.Errorf("the units are %+v, want web then cache", out.Units)
	}
	web, cache := out.Units[0], out.Units[1]
	if web.Expose != "http" {
		t.Errorf("the face declares expose %q, want http", web.Expose)
	}
	if web.Build != "." || web.Image != "" {
		t.Errorf("the face is %+v, want the build context it declares", web)
	}
	if !strings.HasPrefix(cache.Image, "docker.io/library/redis@sha256:") || cache.Build != "" {
		t.Errorf("the sidecar is %+v, want the image it is pinned to", cache)
	}
	// Exactly one unit is the face. A Package where two declare one, or none
	// does, is refused before it is ever inspected, and this is what says so
	// on the answer a reader acts on.
	faces := 0
	for _, unit := range out.Units {
		if unit.Expose != "" {
			faces++
		}
	}
	if faces != 1 {
		t.Errorf("%d units carry an exposure, want exactly one face: %+v", faces, out.Units)
	}
	// One image per unit, each the one that unit was built to.
	if web.Digest == "" || cache.Digest == "" {
		t.Fatalf("the units answer the digests %q and %q, want the image of each", web.Digest, cache.Digest)
	}
	if web.Digest == cache.Digest {
		t.Errorf("both units answer %s, want one image per unit", web.Digest)
	}
}

// A Package of one unit answers one entry named after the Package, because a
// single unit is the Package itself, and its digest is the latest build.
func TestInspectOfOneUnitNamesThePackage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.commit(t, "ffmpeg",
		fs.File{Path: "kitbash.yaml", Content: text(containerManifest)},
		fs.File{Path: "Containerfile", Content: text("FROM alpine\n")})
	built, prob := f.packages.Build(ctx, folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}

	out, prob := f.packages.Inspect(ctx, folder)
	if prob != nil {
		t.Fatalf("Inspect: %s", prob.Detail)
	}
	if len(out.Units) != 1 {
		t.Fatalf("Inspect answered %d units, want one: %+v", len(out.Units), out.Units)
	}
	unit := out.Units[0]
	if unit.Name != "ffmpeg" {
		t.Errorf("the unit is called %q, want the Package's own name", unit.Name)
	}
	if unit.Expose != "mcp" {
		t.Errorf("the unit declares expose %q, want mcp", unit.Expose)
	}
	if unit.Digest != built.Digest {
		t.Errorf("the unit answers %s, want the latest build %s", unit.Digest, built.Digest)
	}
}

// A Package nobody has built here is described all the same: what it declares
// is the manifest's, and only the digest is this host's.
func TestInspectAnswersUnitsOfAPackageNothingHasBuilt(t *testing.T) {
	f := newFixture(t)
	folder := f.commit(t, "board",
		fs.File{Path: "kitbash.yaml", Content: text(twoUnitManifest)},
		fs.File{Path: "Containerfile", Content: text("FROM alpine\n")})

	out, prob := f.packages.Inspect(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Inspect: %s", prob.Detail)
	}
	if len(out.Units) != 2 {
		t.Fatalf("Inspect answered %d units, want two: %+v", len(out.Units), out.Units)
	}
	for _, unit := range out.Units {
		if unit.Digest != "" {
			t.Errorf("the unit %s answers the digest %s, want none before anything is built", unit.Name, unit.Digest)
		}
	}
}
