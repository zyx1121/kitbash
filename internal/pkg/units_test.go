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
