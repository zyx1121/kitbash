package safeopen

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// Every descriptor this package holds is close on exec. kitbash starts
// containers while these are open, and a descriptor on a folder below a root
// is exactly the thing that must not be inherited by one.
func TestEveryDescriptorIsCloseOnExec(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "f"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	dir, err := openRoot(root)
	if err != nil {
		t.Fatalf("openRoot: %v", err)
	}
	defer unix.Close(dir)
	assertCloexec(t, "the root descriptor", dir)

	child, err := descend(dir, "a")
	if err != nil {
		t.Fatalf("descend: %v", err)
	}
	defer unix.Close(child)
	assertCloexec(t, "a descended folder", child)

	f, err := Open(root, "a/f", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	assertCloexec(t, "an opened file", int(f.Fd()))
}

func assertCloexec(t *testing.T, what string, fd int) {
	t.Helper()
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatalf("F_GETFD on %s: %v", what, err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Errorf("%s is fd %d without FD_CLOEXEC (flags %#x)", what, fd, flags)
	}
}
