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
