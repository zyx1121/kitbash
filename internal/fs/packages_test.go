package fs_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/problem"
)

// tree builds a root with one visible Package, one visible folder that is not
// a Package, one invisible folder holding a perfectly good Package, and one
// dot folder. It returns the service and the root.
func tree(t *testing.T) (*fs.Service, string) {
	t.Helper()
	root := t.TempDir()
	mkdir(t, filepath.Join(root, "ffmpeg", "src"))
	mkdir(t, filepath.Join(root, "handbook"))
	mkdir(t, filepath.Join(root, "nomanifest", "buried"))
	mkdir(t, filepath.Join(root, ".hidden"))

	writeFile(t, filepath.Join(root, "ffmpeg", "kitbash.yaml"), `name: ffmpeg
description: Transcode and probe media files. Use for any audio or video conversion.
deploy:
  units:
    - type: container
      build: src
      expose: mcp
`)
	writeFile(t, filepath.Join(root, "ffmpeg", "src", "kitbash.yaml"), `name: src
description: The build context of the ffmpeg Package, holding its Containerfile.
`)
	writeFile(t, filepath.Join(root, "ffmpeg", "src", "Containerfile"), "FROM alpine\n")
	writeFile(t, filepath.Join(root, "handbook", "kitbash.yaml"), `name: handbook
description: How this organization works. Read before writing anything into this root.
`)
	writeFile(t, filepath.Join(root, "nomanifest", "buried", "kitbash.yaml"), `name: buried
description: A Package under a folder that carries no manifest of its own.
deploy:
  units:
    - type: container
      build: .
`)
	writeFile(t, filepath.Join(root, ".hidden", "kitbash.yaml"), `name: hidden
description: A Package inside a folder whose name begins with a dot.
deploy:
  units:
    - type: container
      build: .
`)

	service, err := fs.New("tester", []string{root})
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	return service, root
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func TestPackagesStopsAtInvisibleAndDotFolders(t *testing.T) {
	service, root := tree(t)

	entries, prob := service.Packages(context.Background())
	if prob != nil {
		t.Fatalf("Packages: %s", prob.Detail)
	}
	var paths []string
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	want := []string{filepath.Join(root, "ffmpeg")}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("Packages returned %v, want %v", paths, want)
	}
	if entries[0].Manifest.Name != "ffmpeg" {
		t.Errorf("the manifest name is %q, want ffmpeg", entries[0].Manifest.Name)
	}
}

func TestManifestRefusesAnInvisibleFolder(t *testing.T) {
	service, root := tree(t)

	if _, _, prob := service.Manifest(context.Background(), filepath.Join(root, "nomanifest")); prob == nil {
		t.Fatal("a folder without a manifest was readable")
	} else if prob.Slug() != problem.SlugNotVisible {
		t.Errorf("problem is %s, want not-visible", prob.Slug())
	}

	m, clean, prob := service.Manifest(context.Background(), filepath.Join(root, "ffmpeg"))
	if prob != nil {
		t.Fatalf("Manifest: %s", prob.Detail)
	}
	if clean != filepath.Join(root, "ffmpeg") || m.Name != "ffmpeg" {
		t.Errorf("Manifest returned %q and %q", clean, m.Name)
	}
}

func TestWriteFilesMakesOneCommit(t *testing.T) {
	service, root := tree(t)
	ctx := context.Background()
	folder := filepath.Join(root, "import-mcp")

	body := "# time\n"
	yaml := `name: time
description: A wrapped MCP server that answers what the time is right now.
deploy:
  units:
    - type: container
      build: .
      expose: mcp
`
	out, prob := service.WriteFiles(ctx, folder, []fs.File{
		{Path: "kitbash.yaml", Content: &yaml},
		{Path: "README.md", Content: &body},
		{Path: "src/Containerfile", Content: &body},
	}, "Import npm:time-mcp")
	if prob != nil {
		t.Fatalf("WriteFiles: %s", prob.Detail)
	}
	if out.Commit.Sha == "" || out.Commit.Author != "tester" {
		t.Errorf("commit is %+v, want one attributed to tester", out.Commit)
	}
	for _, name := range []string{"kitbash.yaml", "README.md", filepath.Join("src", "Containerfile")} {
		if _, err := os.Stat(filepath.Join(folder, name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}
	// Three files, one version of Files.
	if got := commitCount(t, folder); got != 1 {
		t.Errorf("the repository has %d commits, want 1", got)
	}
}

func TestWriteFilesRefusesDotComponentsAndEscapes(t *testing.T) {
	service, root := tree(t)
	ctx := context.Background()
	body := "x\n"

	cases := []struct {
		name string
		path string
	}{
		{name: "dot component", path: ".git/config"},
		{name: "parent", path: "../escape.md"},
		{name: "absolute", path: "/etc/passwd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, prob := service.WriteFiles(ctx, filepath.Join(root, "new-package"),
				[]fs.File{{Path: tc.path, Content: &body}}, "Import something")
			if prob == nil {
				t.Fatalf("the path %q was accepted", tc.path)
			}
			if prob.Slug() != problem.SlugInvalidPath {
				t.Errorf("problem is %s, want invalid-path", prob.Slug())
			}
		})
	}
}

// A folder written without a manifest is simply invisible afterwards. fs does
// not require one; pkg_import does, because an invisible Package is useless.
func TestWriteFilesWithoutAManifestIsAllowed(t *testing.T) {
	service, root := tree(t)
	body := "notes\n"

	_, prob := service.WriteFiles(context.Background(), filepath.Join(root, "scratch"),
		[]fs.File{{Path: "README.md", Content: &body}}, "Add scratch notes")
	if prob != nil {
		t.Fatalf("WriteFiles: %s", prob.Detail)
	}
}

func TestWriteFilesValidatesTheManifest(t *testing.T) {
	service, root := tree(t)
	broken := "name: Not Kebab Case\n"

	_, prob := service.WriteFiles(context.Background(), filepath.Join(root, "broken"),
		[]fs.File{{Path: "kitbash.yaml", Content: &broken}}, "Import something broken")
	if prob == nil {
		t.Fatal("an invalid manifest was committed")
	}
	if prob.Slug() != problem.SlugInvalidManifest {
		t.Errorf("problem is %s, want invalid-manifest", prob.Slug())
	}
	if _, err := os.Stat(filepath.Join(root, "broken")); err == nil {
		t.Error("the folder was created even though the write was refused")
	}
}

func TestHeadReportsADirtyTree(t *testing.T) {
	service, root := tree(t)
	ctx := context.Background()
	folder := filepath.Join(root, "import-mcp")
	yaml := `name: time
description: A wrapped MCP server that answers what the time is right now.
`
	if _, prob := service.WriteFiles(ctx, folder, []fs.File{{Path: "kitbash.yaml", Content: &yaml}},
		"Add the time Package"); prob != nil {
		t.Fatalf("WriteFiles: %s", prob.Detail)
	}

	head, prob := service.Head(ctx, folder)
	if prob != nil {
		t.Fatalf("Head: %s", prob.Detail)
	}
	if len(head.Sha) != 40 || !head.Clean {
		t.Fatalf("head is %+v, want a clean 40 character sha", head)
	}

	writeFile(t, filepath.Join(folder, "uncommitted.md"), "not committed\n")
	head, prob = service.Head(ctx, folder)
	if prob != nil {
		t.Fatalf("Head: %s", prob.Detail)
	}
	if head.Clean {
		t.Error("the working tree has an uncommitted file but Head reports it clean")
	}
}

// commitCount counts the commits of the repository a folder belongs to.
func commitCount(t *testing.T, folder string) int {
	t.Helper()
	cmd := exec.Command("git", "-c", "safe.directory="+folder, "rev-list", "--count", "HEAD")
	cmd.Dir = folder
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("counting commits in %s: %v", folder, err)
	}
	count := 0
	for _, c := range strings.TrimSpace(string(out)) {
		count = count*10 + int(c-'0')
	}
	return count
}
