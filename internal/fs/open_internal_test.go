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

// planted builds a root with a folder, a file in it and a secret next to it,
// and returns the Service, the folder and the file.
func planted(t *testing.T) (*Service, string, string) {
	t.Helper()
	root := t.TempDir()
	folder := filepath.Join(root, "docs")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(folder, "secret.md"), []byte("the crown jewels\n"), 0o600); err != nil {
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
	return service, folder, path
}

// resolve walks the path with Lstat, which leaves a window: a caller can swap a
// symlink in for the regular file between that walk and the open. This drives
// the swap deterministically, so the open is the only thing standing between
// the agent and the target of the link.
func TestOpenRefusesALinkSwappedInAfterTheWalk(t *testing.T) {
	service, folder, path := planted(t)
	clean, prob := service.resolve(path)
	if prob != nil {
		t.Fatalf("resolve of a regular file: %s", prob.Detail)
	}

	// The swap: the walk above saw a regular file, the open below sees a link.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(filepath.Join(folder, "secret.md"), path); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	f, err := service.read(clean)
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

// The hole this closes, issue #22: O_NOFOLLOW protects the final component
// only, so a directory in the middle of the path swapped for a link after the
// walk used to be followed. openat2 resolves the whole path with
// RESOLVE_NO_SYMLINKS, so the component that changed is refused wherever it
// sits.
func TestOpenRefusesALinkSwappedInAtAnIntermediateComponent(t *testing.T) {
	service, folder, path := planted(t)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "docs", "note.md"), []byte("the crown jewels\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	clean, prob := service.resolve(path)
	if prob != nil {
		t.Fatalf("resolve of a regular file: %s", prob.Detail)
	}

	// The swap: docs was a folder while resolve walked the path, and is a link
	// to a folder outside the root by the time the open runs.
	if err := os.RemoveAll(folder); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "docs"), folder); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	f, err := service.read(clean)
	if err == nil {
		defer f.Close()
		data := make([]byte, 64)
		n, _ := f.Read(data)
		t.Fatalf("the open followed the intermediate link and read %q", data[:n])
	}
	if !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("the open failed with %v, want ELOOP", err)
	}
	if got := openProblem(clean, err).Slug(); got != problem.SlugInvalidPath {
		t.Errorf("the failure is %s, want %s", got, problem.SlugInvalidPath)
	}

	// The stat below a root answers the same way, so nothing learns about the
	// folder the link points at either.
	if _, err := service.stat(clean); !errors.Is(err, syscall.ELOOP) {
		t.Errorf("the stat failed with %v, want ELOOP", err)
	}
}

// The write side of the same hole: a folder swapped for a link must not make
// the file land outside the root.
func TestWriteFileRefusesALinkSwappedInAtAnIntermediateComponent(t *testing.T) {
	service, folder, path := planted(t)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.RemoveAll(folder); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "docs"), folder); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := service.writeFile(path, []byte("planted\n")); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("the write failed with %v, want ELOOP", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "docs", "note.md")); err == nil {
		t.Error("the write landed outside the root")
	}
	if got := writeProblem(path, &os.PathError{Op: "openat2", Path: path, Err: syscall.ELOOP}).Slug(); got != problem.SlugInvalidPath {
		t.Errorf("the failure is %s, want %s", got, problem.SlugInvalidPath)
	}
}

// git resolves its own paths, so the working directory it is handed is a
// descriptor rather than a name. This asserts the descriptor is really what it
// gets on a host with /proc, and that the descriptor names the folder that was
// resolved.
func TestRepoDirNamesADescriptor(t *testing.T) {
	service, folder, _ := planted(t)
	dir, release, err := service.repoDir(folder)
	if err != nil {
		t.Fatalf("repoDir: %v", err)
	}
	defer release()
	if !strings.HasPrefix(dir, "/proc/self/fd/") {
		t.Fatalf("the working directory is %q, want a descriptor", dir)
	}
	target, err := os.Readlink(dir)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != folder {
		t.Errorf("the descriptor holds %q, want %q", target, folder)
	}
}

// A repository that has become a symlink gets no working directory at all.
// Falling back to the name here is what let git init create a .git outside the
// root, because the name is what the link redirects.
func TestRepoDirRefusesASymlinkedRepo(t *testing.T) {
	service, folder, _ := planted(t)
	outside := t.TempDir()
	if err := os.RemoveAll(folder); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(outside, folder); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	dir, release, err := service.repoDir(folder)
	release()
	if err == nil {
		t.Fatalf("a symlinked repository was given %q as a working directory", dir)
	}
	if !errors.Is(err, syscall.ELOOP) {
		t.Errorf("repoDir failed with %v, want ELOOP", err)
	}
	if got := gitProblem(folder, err).Slug(); got != problem.SlugInvalidPath {
		t.Errorf("the failure is %s, want %s", got, problem.SlugInvalidPath)
	}
}

// A missing file is still a plain not found, so the openat2 mapping does not
// turn every failed open into an invalid path.
func TestOpenProblemKeepsOtherFailuresIntact(t *testing.T) {
	service, folder, _ := planted(t)
	path := filepath.Join(folder, "absent.md")
	_, err := service.read(path)
	if err == nil {
		t.Fatal("opening a file that does not exist succeeded")
	}
	if got := openProblem(path, err).Slug(); got != problem.SlugNotFound {
		t.Errorf("a missing file is %s, want %s", got, problem.SlugNotFound)
	}
}
