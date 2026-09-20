package sysusers

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The archive step of a removal moves a member's home under /org/.archive and
// gives it to root. A home carries whatever its member put in it, an immutable
// file and a mode nothing else on the host has included, and the account it
// belonged to is gone by the time this runs: a step that stopped on one file
// would leave a deleted account, a half moved home and an admin holding an
// error about a file, see issue #152.

// stageChown replaces the chown the archive uses for the length of one test,
// which is how a name this daemon may not take is staged: nothing inside a
// temporary directory can be made immutable, and a test does not run as root.
func stageChown(t *testing.T, refuse string) *int {
	t.Helper()
	taken := 0
	held := lchown
	lchown = func(path string, _, _ int) error {
		if refuse != "" && strings.HasSuffix(path, refuse) {
			return syscall.EPERM
		}
		taken++
		return nil
	}
	t.Cleanup(func() { lchown = held })
	return &taken
}

// homeWith builds a home with one file in a folder, and answers the host that
// archives it.
func homeWith(t *testing.T, names ...string) (*Host, Member) {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home", "alice")
	if err := os.MkdirAll(filepath.Join(home, "notes"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(home, "notes", name), []byte("what alice wrote"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	return &Host{Archive: filepath.Join(dir, "archive")}, Member{Name: "alice", UID: 1005, Home: home}
}

// TestTheArchiveToleratesAFileItCannotTake is the guard: one file the daemon
// may not chown does not stop the home from being archived and does not make
// the step a failure.
func TestTheArchiveToleratesAFileItCannotTake(t *testing.T) {
	h, m := homeWith(t, "immutable.txt", "ordinary.txt")
	taken := stageChown(t, "immutable.txt")

	target, err := h.archiveHome(m)
	if err != nil {
		t.Fatalf("archiveHome: %v, want the home archived", err)
	}
	if target != filepath.Join(h.Archive, "alice") {
		t.Errorf("the home went to %q, want it under the archive", target)
	}
	// The whole home moved, the file it could not take included.
	for _, name := range []string{"immutable.txt", "ordinary.txt"} {
		if _, err := os.Lstat(filepath.Join(target, "notes", name)); err != nil {
			t.Errorf("%s is not in the archive: %v", name, err)
		}
	}
	if _, err := os.Lstat(m.Home); !os.IsNotExist(err) {
		t.Errorf("the home is still where it was: %v", err)
	}
	// And the rest of the tree was taken, so tolerating one name is not
	// giving up on the walk.
	if *taken < 3 {
		t.Errorf("%d names were taken, want the archive, the home and everything under it bar one", *taken)
	}
	// The archive is narrow whatever it could not chown.
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != ArchiveMode {
		t.Errorf("the archived home is %v, want %v", info.Mode().Perm(), os.FileMode(ArchiveMode))
	}
}

// A home with nothing the daemon cannot take is archived the same way, which
// is the path the tolerance has to leave alone.
func TestTheArchiveTakesAHomeItCanTake(t *testing.T) {
	h, m := homeWith(t, "ordinary.txt")
	taken := stageChown(t, "")

	if _, err := h.archiveHome(m); err != nil {
		t.Fatalf("archiveHome: %v", err)
	}
	if *taken < 4 {
		t.Errorf("%d names were taken, want the archive, the home, the folder and the file", *taken)
	}
}

// chownTree answers how many names it could not take, which is what the
// caller says once rather than once per name.
func TestChownTreeCountsWhatItCouldNotTake(t *testing.T) {
	_, m := homeWith(t, "immutable.txt", "ordinary.txt")
	stageChown(t, "immutable.txt")
	if left := chownTree(m.Home, 0, 0); left != 1 {
		t.Errorf("chownTree left %d names, want the one it may not take", left)
	}
}
