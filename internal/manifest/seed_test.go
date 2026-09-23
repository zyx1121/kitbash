package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSeededManifestsAreVisible guards the kits the apk seeds into /org: a
// manifest that fails the schema makes its folder invisible on every fresh
// host, which the unit tests of the kits themselves cannot see.
func TestSeededManifestsAreVisible(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("..", "..", "deploy", "org", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 {
		t.Fatal("no seeded folders found under deploy/org")
	}
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
			t.Errorf("%s carries no %s", dir, FileName)
			continue
		}
		m, err := Load(dir)
		if err != nil {
			t.Errorf("%s: %v", dir, err)
			continue
		}
		if m.Name == "" || m.Description == "" {
			t.Errorf("%s: manifest has no name or description", dir)
		}
	}
}

// TestSeededNestedManifestsAreVisible does the same for a manifest below the
// top level, such as each recipe under /org/skills: it begins a description of
// its own, and one that fails the schema hides that recipe while its parent
// stays listed, so nothing else would notice.
func TestSeededNestedManifestsAreVisible(t *testing.T) {
	root := filepath.Join("..", "..", "deploy", "org")
	nested := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Dependencies and test fixtures may carry manifests that are not
		// seeded as folders of their own, some of them invalid on purpose.
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "fixtures") {
			return filepath.SkipDir
		}
		if d.IsDir() || d.Name() != FileName {
			return nil
		}
		dir := filepath.Dir(path)
		if filepath.Dir(dir) == root {
			return nil // top level, TestSeededManifestsAreVisible
		}
		nested++
		m, err := Load(dir)
		if err != nil {
			t.Errorf("%s: %v", dir, err)
			return nil
		}
		if m.Name == "" || m.Description == "" {
			t.Errorf("%s: manifest has no name or description", dir)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if nested == 0 {
		t.Fatal("no nested manifests found under deploy/org, want the recipes under skills")
	}
}
