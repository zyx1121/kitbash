package fs_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/problem"
)

// A multi file write is input like any other, and an import kit is a Package
// like any other, so the size of what it hands back is bounded.
func TestWriteFilesRefusesTooManyFiles(t *testing.T) {
	service, root := tree(t)
	body := "x\n"
	files := make([]fs.File, 0, fs.MaxFiles+1)
	for i := range fs.MaxFiles + 1 {
		files = append(files, fs.File{Path: fmt.Sprintf("file%03d.md", i), Content: &body})
	}

	_, prob := service.WriteFiles(context.Background(), filepath.Join(root, "many"), files, "Import many files")
	if prob == nil {
		t.Fatal("a write of more files than the limit was accepted")
	}
	if prob.Slug() != problem.SlugTooLarge {
		t.Errorf("problem is %s, want too-large", prob.Slug())
	}
	if _, err := os.Stat(filepath.Join(root, "many")); err == nil {
		t.Error("the folder was created even though the write was refused")
	}
}

func TestWriteFilesRefusesTooManyBytes(t *testing.T) {
	service, root := tree(t)
	// Each file is under the single file limit; together they are over the
	// limit for one folder.
	big := strings.Repeat("x", fs.MaxBytes)
	files := make([]fs.File, 0, 16)
	for i := range 16 {
		body := big
		files = append(files, fs.File{Path: fmt.Sprintf("file%02d.bin", i), Content: &body})
	}

	_, prob := service.WriteFiles(context.Background(), filepath.Join(root, "big"), files, "Import a big Package")
	if prob == nil {
		t.Fatal("a write over the folder byte limit was accepted")
	}
	if prob.Slug() != problem.SlugTooLarge {
		t.Errorf("problem is %s, want too-large", prob.Slug())
	}
	if _, err := os.Stat(filepath.Join(root, "big")); err == nil {
		t.Error("the folder was created even though the write was refused")
	}
}

// A path that reaches out of the folder through a symlink is refused, the same
// way one that spells .. is.
func TestWriteFilesRefusesASymlinkedComponent(t *testing.T) {
	service, root := tree(t)
	folder := filepath.Join(root, "linked")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(folder, "out")); err != nil {
		t.Skipf("this filesystem does not do symlinks: %v", err)
	}
	body := "x\n"

	_, prob := service.WriteFiles(context.Background(), folder,
		[]fs.File{{Path: "out/escaped.md", Content: &body}}, "Write through a link")
	if prob == nil {
		t.Fatal("a write through a symlink was accepted")
	}
	if prob.Slug() != problem.SlugInvalidPath {
		t.Errorf("problem is %s, want invalid-path", prob.Slug())
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped.md")); err == nil {
		t.Error("the file was written outside the folder")
	}
}

// The single dot names the folder, which is not a file.
func TestWriteFilesRefusesTheFolderItself(t *testing.T) {
	service, root := tree(t)
	body := "x\n"

	_, prob := service.WriteFiles(context.Background(), filepath.Join(root, "dot"),
		[]fs.File{{Path: ".", Content: &body}}, "Write the folder itself")
	if prob == nil {
		t.Fatal("a file path of . was accepted")
	}
	if prob.Slug() != problem.SlugInvalidPath {
		t.Errorf("problem is %s, want invalid-path", prob.Slug())
	}
}
