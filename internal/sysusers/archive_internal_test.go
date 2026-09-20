package sysusers

import (
	"context"
	"errors"
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

// A removal deletes the account and then moves the home, so a daemon that
// stopped between the two left an account that is gone and a home that is
// still there. The archive step is asked for on its own to take that up: with
// no account there is nothing to read a home path off, so the name's own place
// under the homes root is what is looked at.
func TestArchiveHomeTakesUpAHomeADeletedAccountLeft(t *testing.T) {
	dir := t.TempDir()
	homes := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(homes, "alice", "notes"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(homes, "alice", "notes", "plan.md"),
		[]byte("what alice wrote"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// The account is gone: the passwd file this host reads has no alice.
	passwd := filepath.Join(dir, "passwd")
	if err := os.WriteFile(passwd, []byte("root:x:0:0::/root:/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	h := &Host{Archive: filepath.Join(dir, "archive"), Homes: homes, Passwd: passwd,
		Group: filepath.Join(dir, "group")}
	stageChown(t, "")

	target, err := h.ArchiveHome(context.Background(), "alice")
	if err != nil {
		t.Fatalf("ArchiveHome: %v", err)
	}
	if target != filepath.Join(h.Archive, "alice") {
		t.Errorf("the home went to %q, want it under the archive", target)
	}
	if _, err := os.Lstat(filepath.Join(target, "notes", "plan.md")); err != nil {
		t.Errorf("what the deleted account left is not in the archive: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(homes, "alice")); !os.IsNotExist(err) {
		t.Errorf("the home is still where the deleted account left it: %v", err)
	}

	// And running it again is no work rather than a failure, which is what
	// makes the whole job safe to take up twice.
	if _, err := h.ArchiveHome(context.Background(), "alice"); err != nil {
		t.Errorf("the second ArchiveHome: %v, want it to find nothing to move", err)
	}
}

// A name that is not a member name never reaches the filesystem: this is the
// one call that builds a home path from a name rather than reading one off an
// account.
func TestArchiveHomeRefusesAName(t *testing.T) {
	h := &Host{Archive: t.TempDir(), Homes: t.TempDir()}
	if _, err := h.ArchiveHome(context.Background(), "../root"); !errors.Is(err, ErrName) {
		t.Errorf("ArchiveHome of a path = %v, want ErrName", err)
	}
}
