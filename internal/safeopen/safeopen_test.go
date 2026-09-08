package safeopen_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/safeopen"
)

// secret is what a refused open must never return, whatever route it takes.
const secret = "the crown jewels"

// tree builds a root holding docs/note.md, and a folder outside it holding the
// same names with the secret inside, which is what a swapped link points at.
func tree(t *testing.T) (root, outside string) {
	t.Helper()
	root = t.TempDir()
	outside = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "note.md"), []byte("a note\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "docs", "note.md"), []byte(secret+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return root, outside
}

func TestOpenReadsAFileBelowTheRoot(t *testing.T) {
	root, _ := tree(t)
	f, err := safeopen.Open(root, "docs/note.md", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "a note\n" {
		t.Errorf("read %q, want %q", data, "a note\n")
	}
}

// The hole issue #22 named: the middle of the path, not its last component.
func TestOpenRefusesASymlinkAtAnIntermediateComponent(t *testing.T) {
	root, outside := tree(t)
	if err := os.RemoveAll(filepath.Join(root, "docs")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "docs"), filepath.Join(root, "docs")); err != nil {
		t.Skipf("this filesystem does not do symlinks: %v", err)
	}

	f, err := safeopen.Open(root, "docs/note.md", os.O_RDONLY, 0)
	if err == nil {
		data, _ := io.ReadAll(f)
		f.Close()
		t.Fatalf("the open followed the intermediate link and read %q", data)
	}
	if !errors.Is(err, syscall.ELOOP) {
		t.Errorf("the open failed with %v, want ELOOP", err)
	}
	if _, err := safeopen.Stat(root, "docs/note.md"); !errors.Is(err, syscall.ELOOP) {
		t.Errorf("the stat failed with %v, want ELOOP", err)
	}
}

func TestOpenRefusesASymlinkAtTheFinalComponent(t *testing.T) {
	root, outside := tree(t)
	link := filepath.Join(root, "docs", "link.md")
	if err := os.Symlink(filepath.Join(outside, "docs", "note.md"), link); err != nil {
		t.Skipf("this filesystem does not do symlinks: %v", err)
	}
	if _, err := safeopen.Open(root, "docs/link.md", os.O_RDONLY, 0); !errors.Is(err, syscall.ELOOP) {
		t.Errorf("the open failed with %v, want ELOOP", err)
	}
}

// RESOLVE_BENEATH is the second refusal: a path that resolves out of the root
// is EXDEV, whether it climbs with .. or starts at the top.
func TestOpenRefusesAPathThatLeavesTheRoot(t *testing.T) {
	root, _ := tree(t)
	for _, rel := range []string{"..", "../secret", "docs/../../secret", "/etc/passwd"} {
		if _, err := safeopen.Open(root, rel, os.O_RDONLY, 0); !errors.Is(err, syscall.EXDEV) {
			t.Errorf("Open(%q) failed with %v, want EXDEV", rel, err)
		}
	}
}

// A root that is itself a symlink is refused: the root is the trust boundary,
// and a link stands where the caller thinks the boundary is.
func TestOpenRefusesASymlinkedRoot(t *testing.T) {
	root, outside := tree(t)
	link := filepath.Join(root, "elsewhere")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("this filesystem does not do symlinks: %v", err)
	}
	if _, err := safeopen.Open(link, "docs/note.md", os.O_RDONLY, 0); !errors.Is(err, syscall.ELOOP) {
		t.Errorf("the open of a symlinked root failed with %v, want ELOOP", err)
	}
}

// A missing file is a plain not found, so the refusals above do not swallow
// every other failure.
func TestOpenReportsAMissingFile(t *testing.T) {
	root, _ := tree(t)
	if _, err := safeopen.Open(root, "docs/absent.md", os.O_RDONLY, 0); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the open failed with %v, want not exist", err)
	}
}

// A FIFO must not hold the open, whichever direction it is opened in: a
// caller can plant one anywhere they can write.
func TestOpenDoesNotBlockOnAFifo(t *testing.T) {
	root, _ := tree(t)
	pipe := filepath.Join(root, "docs", "pipe")
	if err := syscall.Mkfifo(pipe, 0o644); err != nil {
		t.Skipf("this filesystem has no FIFOs: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if f, err := safeopen.Open(root, "docs/pipe", os.O_RDONLY, 0); err == nil {
			// A reader of an empty FIFO returns at once with O_NONBLOCK; the
			// point is that the open returned at all.
			f.Close()
		}
		if f, err := safeopen.Open(root, "docs/pipe", os.O_WRONLY, 0); err == nil {
			f.Close()
		}
		if info, err := safeopen.Stat(root, "docs/pipe"); err == nil && info.Mode()&os.ModeNamedPipe == 0 {
			panic("a FIFO was described as something else")
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// The goroutine is stuck in open(2) and cannot be reclaimed, so the run
		// has to stop here rather than carry on with a wedged worker.
		t.Fatal("the open blocked on a FIFO")
	}
}

func TestMkdirAllCreatesOnlyWhatIsMissing(t *testing.T) {
	root, _ := tree(t)
	created, err := safeopen.MkdirAll(root, "docs/deep/deeper", 0o755)
	if err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if len(created) != 2 || created[0] != filepath.Join("docs", "deep") ||
		created[1] != filepath.Join("docs", "deep", "deeper") {
		t.Errorf("MkdirAll created %v, want the two folders that were missing", created)
	}
	again, err := safeopen.MkdirAll(root, "docs/deep/deeper", 0o755)
	if err != nil {
		t.Fatalf("MkdirAll again: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("MkdirAll created %v the second time, want nothing", again)
	}
}

// A folder must not be created outside the root through a link planted on the
// way down, which is the write side of the same window.
func TestMkdirAllRefusesASymlinkedComponent(t *testing.T) {
	root, outside := tree(t)
	if err := os.RemoveAll(filepath.Join(root, "docs")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "docs"), filepath.Join(root, "docs")); err != nil {
		t.Skipf("this filesystem does not do symlinks: %v", err)
	}
	if _, err := safeopen.MkdirAll(root, "docs/deep", 0o755); !errors.Is(err, syscall.ELOOP) {
		t.Errorf("MkdirAll failed with %v, want ELOOP", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "docs", "deep")); err == nil {
		t.Error("MkdirAll created a folder outside the root")
	}
}

// The swap driven while opens run, rather than staged between two calls: no
// ordering of the two is allowed to return the content behind the link.
func TestASwappedFolderNeverLeaksWhileOpensRun(t *testing.T) {
	root, outside := tree(t)
	real := filepath.Join(root, "docs.real")
	folder := filepath.Join(root, "docs")

	stop := make(chan struct{})
	var swapper sync.WaitGroup
	swapper.Add(1)
	go func() {
		defer swapper.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := os.Rename(folder, real); err != nil {
				continue
			}
			if err := os.Symlink(filepath.Join(outside, "docs"), folder); err != nil {
				os.Rename(real, folder)
				continue
			}
			os.Remove(folder)
			os.Rename(real, folder)
		}
	}()

	for i := 0; i < 5000; i++ {
		f, err := safeopen.Open(root, "docs/note.md", os.O_RDONLY, 0)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(f)
		f.Close()
		if string(data) != "a note\n" {
			close(stop)
			swapper.Wait()
			t.Fatalf("the open followed the swapped folder and read %q", data)
		}
	}
	close(stop)
	swapper.Wait()
}
