package proc_test

import (
	"context"
	"strings"
	"testing"
)

// permitsManifest is a kit that says what it needs: two tools and one prefix.
const permitsManifest = `name: workflow
description: Runs a graph whose steps call the tools of the owner's running Processes.
provides:
  permits:
    tools: [fs_read, fs_list, "*_*"]
    paths: [/org, /home/*]
deploy:
  units:
    - type: container
      build: .
      expose: mcp
`

// TestRunRegistersThePermitsOfTheManifest is where the narrowing starts: what
// a Process may call over /mcp is what its Package declared, and kitbashd is
// told at registration rather than reading a manifest later, see PLAN.md 2.3.
func TestRunRegistersThePermitsOfTheManifest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "workflow", permitsManifest)
	f.build(folder, "workflow")

	if _, prob := f.processes.Run(ctx, folder, "", ""); prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	registrations := f.daemon.Registrations()
	if len(registrations) != 1 {
		t.Fatalf("kitbashd holds %d registrations, want 1", len(registrations))
	}
	permits := registrations[0].Permits
	if got := strings.Join(permits.Tools, ","); got != "fs_read,fs_list,*_*" {
		t.Errorf("permits.tools = %q, want the three the manifest declares", got)
	}
	if got := strings.Join(permits.Paths, ","); got != "/org,/home/*" {
		t.Errorf("permits.paths = %q, want the two the manifest declares", got)
	}
	if err := permits.Validate(); err != nil {
		t.Errorf("the registered block does not validate: %v", err)
	}
}

// TestRunRegistersNoPermitsWhenTheManifestDeclaresNone is the default: a
// Package that asks for nothing gets an empty surface, not its owner's.
func TestRunRegistersNoPermitsWhenTheManifestDeclaresNone(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")

	if _, prob := f.processes.Run(ctx, folder, "", ""); prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	registrations := f.daemon.Registrations()
	if len(registrations) != 1 {
		t.Fatalf("kitbashd holds %d registrations, want 1", len(registrations))
	}
	permits := registrations[0].Permits
	if len(permits.Tools) != 0 || len(permits.Paths) != 0 {
		t.Errorf("permits = %+v, want an empty block", permits)
	}
	if permits.Match("fs_read") || permits.AnyPath() {
		t.Error("the empty block permitted something")
	}
}
