package fs_test

import (
	"context"
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
