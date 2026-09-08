package sysusers

import (
	"os"
	"path/filepath"
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
