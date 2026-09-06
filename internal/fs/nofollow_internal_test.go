package fs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/zyx1121/kitbash/internal/problem"
)

// resolve walks the path with Lstat, which leaves a window: a caller can swap a
// symlink in for the regular file between that walk and the open. This drives
// the swap deterministically, so the open is the only thing standing between
// the agent and the target of the link.
func TestOpenNoFollowRefusesALinkSwappedInAfterTheWalk(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "docs")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := filepath.Join(folder, "secret.md")
	if err := os.WriteFile(target, []byte("the crown jewels\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	path := filepath.Join(folder, "note.md")
	if err := os.WriteFile(path, []byte("a note\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	service, err := New("tester", []string{root})
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	clean, prob := service.resolve(path)
	if prob != nil {
		t.Fatalf("resolve of a regular file: %s", prob.Detail)
	}

	// The swap: the walk above saw a regular file, the open below sees a link.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	f, err := openNoFollow(clean)
	if err == nil {
		defer f.Close()
		data := make([]byte, 64)
		n, _ := f.Read(data)
		t.Fatalf("the open followed the link and read %q", data[:n])
	}
	if !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("the open failed with %v, want ELOOP", err)
	}

	mapped := openProblem(clean, err)
	if mapped.Slug() != problem.SlugInvalidPath {
		t.Errorf("the failure is %s, want %s", mapped.Slug(), problem.SlugInvalidPath)
	}
	if !strings.Contains(mapped.Detail, "symlink") {
		t.Errorf("the detail is %q, want it to say the path is a symlink", mapped.Detail)
	}
	if strings.Contains(mapped.JSON(), "crown jewels") {
		t.Error("the problem carries the content of the link target")
	}
}

// A missing file is still a plain not found, so the O_NOFOLLOW mapping does not
// turn every failed open into an invalid path.
func TestOpenProblemKeepsOtherFailuresIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.md")
	_, err := openNoFollow(path)
	if err == nil {
		t.Fatal("opening a file that does not exist succeeded")
	}
	if got := openProblem(path, err).Slug(); got != problem.SlugNotFound {
		t.Errorf("a missing file is %s, want %s", got, problem.SlugNotFound)
	}
}
