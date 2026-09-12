package sysusers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSubIDsAllocateWithoutOverlap is the rule rootless podman rests on: two
// members never share a subordinate id block, because members sharing one
// share the ownership of each other's container files.
func TestSubIDsAllocateWithoutOverlap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subuid")

	// A host that has never had a member has no file at all.
	start, err := nextSubID(path)
	if err != nil {
		t.Fatalf("nextSubID: %v", err)
	}
	if start != SubIDBase {
		t.Errorf("first block = %d, want %d", start, SubIDBase)
	}
	if err := appendSubID(path, "alice", start, SubIDCount); err != nil {
		t.Fatalf("appendSubID: %v", err)
	}

	start, err = nextSubID(path)
	if err != nil {
		t.Fatalf("nextSubID: %v", err)
	}
	if want := SubIDBase + SubIDCount; start != want {
		t.Errorf("second block = %d, want %d", start, want)
	}
	if err := appendSubID(path, "bob", start, SubIDCount); err != nil {
		t.Fatalf("appendSubID: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "alice:100000:65536\nbob:165536:65536\n"
	if string(content) != want {
		t.Errorf("file =\n%q\nwant\n%q", content, want)
	}
}

// TestSubIDsSkipAHandEditedBlock is what makes this safe on a host an operator
// has touched: the next block is above the highest one in the file, whatever
// order the lines are in and whatever else they carry.
func TestSubIDsSkipAHandEditedBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subgid")
	if err := os.WriteFile(path, []byte("bob:300000:65536\nalice:100000:65536\n# a comment\n\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	start, err := nextSubID(path)
	if err != nil {
		t.Fatalf("nextSubID: %v", err)
	}
	if want := 300000 + SubIDCount; start != want {
		t.Errorf("next block = %d, want %d", start, want)
	}
}

func TestSubIDsHasAndRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subuid")
	if err := appendSubID(path, "alice", SubIDBase, SubIDCount); err != nil {
		t.Fatalf("appendSubID: %v", err)
	}
	if err := appendSubID(path, "bob", SubIDBase+SubIDCount, SubIDCount); err != nil {
		t.Fatalf("appendSubID: %v", err)
	}

	has, err := hasSubID(path, "alice")
	if err != nil || !has {
		t.Fatalf("hasSubID(alice) = %t, %v, want true", has, err)
	}
	has, err = hasSubID(path, "carol")
	if err != nil || has {
		t.Fatalf("hasSubID(carol) = %t, %v, want false", has, err)
	}

	removed, err := removeSubID(path, "alice")
	if err != nil || !removed {
		t.Fatalf("removeSubID(alice) = %t, %v, want true", removed, err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(content) != "bob:165536:65536\n" {
		t.Errorf("file = %q, want bob alone", content)
	}

	// Removing a name that is not there rewrites nothing.
	removed, err = removeSubID(path, "alice")
	if err != nil || removed {
		t.Errorf("removeSubID(alice) again = %t, %v, want false", removed, err)
	}
}

// TestSubIDsKeepTheirMode is what shadow-utils and podman both expect of these
// two files. A rewrite through a temporary file must not narrow them.
func TestSubIDsKeepTheirMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subuid")
	if err := appendSubID(path, "alice", SubIDBase, SubIDCount); err != nil {
		t.Fatalf("appendSubID: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != SubIDMode {
		t.Errorf("mode = %o, want %o", got, SubIDMode)
	}
}

// TestTheSubIDLockIsKitbashsOwnFile is the whole of issue #83: shadow-utils
// owns the name /etc/subuid.lock, writes a PID into it and refuses to run when
// it finds one without, so a lock kitbash leaves there stops useradd on the
// host. The lock kitbash takes is its own file under /run.
func TestTheSubIDLockIsKitbashsOwnFile(t *testing.T) {
	if DefaultSubIDLock != "/run/kitbash/subids.lock" {
		t.Errorf("the lock is %s, want /run/kitbash/subids.lock", DefaultSubIDLock)
	}
	if DefaultSubIDLock == LegacySubIDLock {
		t.Errorf("the lock is shadow's own %s", LegacySubIDLock)
	}
	if strings.HasPrefix(DefaultSubIDLock, "/etc/") {
		t.Errorf("the lock %s is in /etc, where shadow-utils keeps its own locks", DefaultSubIDLock)
	}
	if DefaultSubIDLock == DefaultSubUIDFile+".lock" || DefaultSubIDLock == DefaultSubGIDFile+".lock" {
		t.Errorf("the lock %s is the lock name of a subordinate id file", DefaultSubIDLock)
	}

	// kitbash-adduser allocates from the same two files and has to take the
	// same lock, so the script is read here too: the two are one protocol, and
	// a script left on the old name would break useradd exactly as before.
	script, err := os.ReadFile(filepath.Join("..", "..", "deploy", "kitbash-adduser"))
	if err != nil {
		t.Fatalf("reading kitbash-adduser: %v", err)
	}
	body := string(script)
	if !strings.Contains(body, "/subids.lock") || !strings.Contains(body, "/run/kitbash") {
		t.Errorf("kitbash-adduser does not lock %s", DefaultSubIDLock)
	}
	if strings.Contains(body, "lockfile=/etc/subuid.lock") {
		t.Errorf("kitbash-adduser still locks shadow's own %s", LegacySubIDLock)
	}
}

// TestLockSubIDsCreatesItsDirectory is what a kitbash host needs at every
// boot: /run is a tmpfs and comes back empty, so the first allocation makes
// the directory rather than failing.
func TestLockSubIDsCreatesItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kitbash", "subids.lock")
	unlock, err := lockSubIDs(path)
	if err != nil {
		t.Fatalf("lockSubIDs: %v", err)
	}
	defer unlock()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the lock: %v", err)
	}
	if got := info.Mode().Perm(); got != SubIDLockMode {
		t.Errorf("the lock is %o, want %o", got, SubIDLockMode)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat the lock directory: %v", err)
	}
	if got := dir.Mode().Perm(); got != SubIDLockDirMode {
		t.Errorf("the lock directory is %o, want %o", got, SubIDLockDirMode)
	}

	// A path that was never configured is a lock nobody holds, which would
	// hand two members the same range silently.
	if _, err := lockSubIDs(""); err == nil {
		t.Error("lockSubIDs of no path returned a lock")
	}
}

// TestRemoveLegacySubIDLockOnlyTakesAnEmptyFile is how an upgraded host
// recovers without deleting a file an operator's useradd is using: the empty
// file is one an older kitbash left, and anything with a PID in it belongs to
// a shadow tool that is running now.
func TestRemoveLegacySubIDLockOnlyTakesAnEmptyFile(t *testing.T) {
	dir := t.TempDir()

	// A host that never ran the old kitbash has no file at all.
	missing := filepath.Join(dir, "missing.lock")
	removed, err := RemoveLegacySubIDLock(missing)
	if err != nil || removed {
		t.Errorf("removing a lock that is not there = %t, %v, want false", removed, err)
	}

	stale := filepath.Join(dir, "stale.lock")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	removed, err = RemoveLegacySubIDLock(stale)
	if err != nil || !removed {
		t.Fatalf("removing the empty lock = %t, %v, want true", removed, err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Errorf("the empty lock is still there (%v)", err)
	}

	// A file with a PID in it is shadow's, being held by a useradd that is
	// running now. Deleting it is the bug, not the fix.
	held := filepath.Join(dir, "held.lock")
	if err := os.WriteFile(held, []byte("4242\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	removed, err = RemoveLegacySubIDLock(held)
	if err != nil || removed {
		t.Errorf("removing a lock holding a PID = %t, %v, want false", removed, err)
	}
	if _, err := os.Stat(held); err != nil {
		t.Errorf("the lock holding a PID was removed: %v", err)
	}

	// Neither a directory nor a symbolic link at that name is kitbash's.
	asDir := filepath.Join(dir, "dir.lock")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	removed, err = RemoveLegacySubIDLock(asDir)
	if err != nil || removed {
		t.Errorf("removing a directory = %t, %v, want false", removed, err)
	}

	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(dir, "link.lock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	removed, err = RemoveLegacySubIDLock(link)
	if err != nil || removed {
		t.Errorf("removing a symbolic link = %t, %v, want false", removed, err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the symbolic link was removed: %v", err)
	}
}
