package fs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/problem"
)

// folderFix is the advice a caller who named a folder can act on, which is the
// whole point of the fix member: the path was absolute, under a root and free
// of parent references, so the rule the default names was never broken, see
// issue #153.
const folderFix = "Call fs_list on it, or fs_read one of its files."

// writeFolderFix is the same thing for the tool that would have created a
// file: naming the folder is the one mistake, and the fix names what to send
// instead rather than the rule about absolute paths.
const writeFolderFix = "A path names a file; to write into this folder, name the file."

func TestReadingAFolderSaysToListIt(t *testing.T) {
	service, _, folder := hardened(t)

	_, prob := service.Read(context.Background(), folder, fs.ReadOptions{})
	wantSlug(t, prob, "fs_read", problem.SlugInvalidPath)
	if prob.Fix != folderFix {
		t.Errorf("the fix is %q, want %q", prob.Fix, folderFix)
	}
	if strings.Contains(prob.Fix, "absolute path") {
		t.Error("the fix is the default advice about the shape of the path, which this path had")
	}
	if !strings.Contains(prob.Detail, "folder") {
		t.Errorf("the detail is %q, want it to say the path is a folder", prob.Detail)
	}
}

// The other tool a folder path reaches answers it rather than refusing it, so
// there is no fix to word: a folder's history is the commits that touched
// anything in it.
func TestHistoryOfAFolderIsAnAnswerAndNotAProblem(t *testing.T) {
	service, _, folder := hardened(t)

	result, prob := service.History(context.Background(), folder, 0)
	if prob != nil {
		t.Fatalf("fs_history of a folder returned %s: %s (fix: %s)", prob.Slug(), prob.Detail, prob.Fix)
	}
	if result.Path != folder {
		t.Errorf("the history is of %q, want %q", result.Path, folder)
	}
}

// fs_write of a folder answered the advice about the shape of a path too, in
// both of its forms: one file and a list are one refusal written twice, so
// both are held to the same fix here.
func TestWritingToAFolderSaysToNameTheFile(t *testing.T) {
	service, _, top := hardened(t)
	// A folder inside the repository rather than the top of one: the top of a
	// repository is refused a step earlier, by the rule that a file lives
	// inside a top level folder.
	folder := filepath.Join(top, "policies")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "planted\n"

	_, prob := service.Write(context.Background(), fs.WriteRequest{
		Path: folder, Content: &content, Message: "write a folder"})
	wantSlug(t, prob, "fs_write", problem.SlugInvalidPath)
	if prob.Fix != writeFolderFix {
		t.Errorf("the fix is %q, want %q", prob.Fix, writeFolderFix)
	}
	if !strings.Contains(prob.Detail, "folder") {
		t.Errorf("the detail is %q, want it to say the path is a folder", prob.Detail)
	}

	_, prob = service.WriteAll(context.Background(), fs.WriteAllRequest{
		Files:   []fs.WriteFileEntry{{Path: folder, Content: &content}},
		Message: "write a folder in a list",
	})
	wantSlug(t, prob, "fs_write files", problem.SlugInvalidPath)
	if prob.Fix != writeFolderFix {
		t.Errorf("the fix of the list form is %q, want %q", prob.Fix, writeFolderFix)
	}
}
