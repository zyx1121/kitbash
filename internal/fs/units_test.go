package fs_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/problem"
)

// A rule about the units together is read where every other manifest rule is,
// at the write that commits it, so the member who wrote a Package of two units
// is told which rule they missed rather than finding an invisible folder
// later, see PLAN.md section 5.6.
func TestWriteFilesNamesTheUnitRuleItRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		manifest string
		says     string
	}{
		"a unit with no name": {`name: board
description: A counter and the cache it keeps its count in, with a unit that has no name.
deploy:
  units:
    - name: web
      type: container
      build: .
      expose: http
      port: 8080
    - type: container
      build: cache
`, "names every unit"},
		"no unit is the face": {`name: board
description: A counter and the cache it keeps its count in, with nothing exposed at all.
deploy:
  units:
    - name: web
      type: container
      build: .
    - name: cache
      type: container
      build: cache
`, "exactly one unit declares expose"},
	} {
		t.Run(name, func(t *testing.T) {
			service, root := tree(t)
			body := tc.manifest
			_, prob := service.WriteFiles(context.Background(), fs.WriteFilesRequest{
				Path:    filepath.Join(root, "board"),
				Files:   []fs.File{{Path: "kitbash.yaml", Content: &body}},
				Message: "Add a Package of two units"})
			if prob == nil {
				t.Fatal("the manifest was committed")
			}
			if prob.Slug() != problem.SlugInvalidManifest {
				t.Fatalf("problem is %s, want invalid-manifest", prob.Slug())
			}
			if !strings.Contains(prob.Detail, tc.says) {
				t.Errorf("the refusal says %q, want it to say %q", prob.Detail, tc.says)
			}
		})
	}
}
