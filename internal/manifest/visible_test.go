package manifest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zyx1121/kitbash/internal/manifest"
)

// chainTree is a root holding a Package with folders of its own, a folder with
// no manifest anywhere above it, and a manifest below that one.
func chainTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("app/kitbash.yaml", "name: app\ndescription: A web application, served over http.\n")
	write("app/public/index.html", "<!doctype html>\n")
	write("app/src/deep/main.go", "package main\n")
	write("app/deploy/kitbash.yaml", "name: deploy\ndescription: How this application is deployed, one file per target.\n")
	write("nomanifest/notes.md", "invisible\n")
	if err := os.MkdirAll(filepath.Join(root, "app", "public"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestVisibleChainStopsAtTheNearestManifest is the reading model of PLAN.md
// section 2.1: the manifest of a Package speaks for everything beneath it, and
// the nearest one is the one that speaks.
func TestVisibleChainStopsAtTheNearestManifest(t *testing.T) {
	root := chainTree(t)

	cases := []struct {
		rel    string
		name   string
		folder string
	}{
		{rel: "app", name: "app", folder: "app"},
		{rel: "app/public", name: "app", folder: "app"},
		{rel: "app/src/deep", name: "app", folder: "app"},
		{rel: "app/deploy", name: "deploy", folder: "app/deploy"},
	}
	for _, tc := range cases {
		t.Run(tc.rel, func(t *testing.T) {
			m, folder, ok := manifest.VisibleChain(root, tc.rel)
			if !ok {
				t.Fatalf("%s is not visible, and the manifest of its Package speaks for it", tc.rel)
			}
			if m == nil || m.Name != tc.name {
				t.Errorf("%s is described by %v, want the manifest of %s", tc.rel, m, tc.name)
			}
			if folder != tc.folder {
				t.Errorf("%s is spoken for by %q, want %s", tc.rel, folder, tc.folder)
			}
		})
	}
}

// And the other half: no manifest anywhere above it is no folder at all, and
// the folder named is the top level one a manifest has to go into.
func TestVisibleChainRefusesAFolderWithNoManifestAboveIt(t *testing.T) {
	root := chainTree(t)

	for _, rel := range []string{"nomanifest", "nomanifest/deeper", "."} {
		m, folder, ok := manifest.VisibleChain(root, rel)
		if ok {
			t.Errorf("%s is visible as %v", rel, m)
		}
		if rel != "." && folder != "nomanifest" {
			t.Errorf("%s names %q, want the top level folder nomanifest", rel, folder)
		}
	}
}

// VisibleChainWith is the same walk with the manifests one write is about to
// make counted as present, which is what lets one fs_write carry a Package.
func TestVisibleChainWithCountsAManifestThisCallWrites(t *testing.T) {
	root := t.TempDir()
	pending := func(rel string) bool { return rel == "app" }

	if _, _, ok := manifest.VisibleChain(root, "app/public"); ok {
		t.Fatal("a folder under a Package that does not exist yet is visible")
	}
	m, folder, ok := manifest.VisibleChainWith(root, "app/public", pending)
	if !ok {
		t.Fatal("a folder under a manifest this call writes is not visible")
	}
	if m != nil {
		t.Errorf("the manifest is %v, and it is not on disk yet", m)
	}
	if folder != "app" {
		t.Errorf("the folder is %q, want app", folder)
	}
	if _, _, ok := manifest.VisibleChainWith(root, "elsewhere/public", pending); ok {
		t.Error("a folder the call writes no manifest above is visible")
	}
}
