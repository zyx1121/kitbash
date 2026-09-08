package sysusers

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// testKey builds one well formed public key line of a type. The internal test
// cannot reach the one in keys_test.go, which is in the external package.
func testKey(keyType string) string {
	body := make([]byte, 0, 48)
	body = binary.BigEndian.AppendUint32(body, uint32(len(keyType)))
	body = append(body, keyType...)
	body = binary.BigEndian.AppendUint32(body, 32)
	for i := range 32 {
		body = append(body, byte(i)+byte(len(keyType)))
	}
	return keyType + " " + base64.StdEncoding.EncodeToString(body)
}

// hostFor is a Host whose files all live in a temporary directory, and one
// member whose home is inside it owned by whoever runs the test. Nothing here
// needs root: the checks that matter are about which name a descriptor ends up
// on, not about which uid holds it.
func hostFor(t *testing.T) (*Host, Member) {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home", "alice")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	h := &Host{
		Passwd:  filepath.Join(dir, "passwd"),
		Group:   filepath.Join(dir, "group"),
		SubUID:  filepath.Join(dir, "subuid"),
		SubGID:  filepath.Join(dir, "subgid"),
		RunUser: filepath.Join(dir, "run"),
		Archive: filepath.Join(dir, "archive"),
		Proc:    filepath.Join(dir, "proc"),
	}
	m := Member{
		Name:   "alice",
		UID:    os.Getuid(),
		GID:    os.Getgid(),
		Home:   home,
		Groups: []string{"alice", UsersGroup},
	}
	return h, m
}

func TestWriteKeyCreatesTheShapeSSHDWants(t *testing.T) {
	h, m := hostFor(t)
	line := testKey("ssh-ed25519")

	n, err := h.writeKey(m, line)
	if err != nil || n != 1 {
		t.Fatalf("writeKey = %d, %v, want one key", n, err)
	}
	// The same key again is not a second key.
	if n, err := h.writeKey(m, line); err != nil || n != 1 {
		t.Fatalf("writeKey again = %d, %v, want one key", n, err)
	}
	if n, err := h.writeKey(m, testKey("ssh-rsa")); err != nil || n != 2 {
		t.Fatalf("writeKey of a second key = %d, %v, want two", n, err)
	}

	dir, err := os.Lstat(filepath.Join(m.Home, ".ssh"))
	if err != nil {
		t.Fatalf("lstat .ssh: %v", err)
	}
	if got := dir.Mode().Perm(); got != SSHDirMode {
		t.Errorf(".ssh mode = %o, want %o", got, SSHDirMode)
	}
	keys := filepath.Join(m.Home, ".ssh", "authorized_keys")
	info, err := os.Lstat(keys)
	if err != nil {
		t.Fatalf("lstat authorized_keys: %v", err)
	}
	if got := info.Mode().Perm(); got != KeysMode {
		t.Errorf("authorized_keys mode = %o, want %o", got, KeysMode)
	}
	content, err := os.ReadFile(keys)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if lines := keyLines(string(content)); len(lines) != 2 || lines[0] != line {
		t.Errorf("authorized_keys =\n%s\nwant the two keys, the first one first", content)
	}
	if count, err := h.countKeys(m); err != nil || count != 2 {
		t.Errorf("countKeys = %d, %v, want 2", count, err)
	}
}

// TestWriteKeyRefusesAMemberControlledName is the rule this whole path exists
// for. kitbashd writes authorized_keys as root inside a directory the member
// owns, so every name under the home is a name the member can replace with a
// link to somewhere else. None of them may be followed out of the home.
func TestWriteKeyRefusesAMemberControlledName(t *testing.T) {
	line := testKey("ssh-ed25519")

	t.Run("a .ssh that is a symbolic link", func(t *testing.T) {
		h, m := hostFor(t)
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(m.Home, ".ssh")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		before := modeOf(t, outside)

		if _, err := h.writeKey(m, line); !errors.Is(err, ErrHomeShape) {
			t.Fatalf("writeKey = %v, want ErrHomeShape", err)
		}
		if after := modeOf(t, outside); after != before {
			t.Errorf("the directory the link named changed mode from %o to %o", before, after)
		}
		if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
			t.Errorf("the directory the link named holds %v, want nothing written into it", entries)
		}
	})

	t.Run("an authorized_keys that is a symbolic link out of the home", func(t *testing.T) {
		h, m := hostFor(t)
		outside := filepath.Join(t.TempDir(), "shadow")
		if err := os.WriteFile(outside, []byte("root:!::0:::::\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.Mkdir(filepath.Join(m.Home, ".ssh"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(m.Home, ".ssh", "authorized_keys")); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		if _, err := h.writeKey(m, line); err == nil {
			t.Fatal("writeKey followed a symbolic link out of the home")
		}
		content, err := os.ReadFile(outside)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.Contains(string(content), "ssh-ed25519") {
			t.Errorf("the file the link named was written: %s", content)
		}
	})

	t.Run("an authorized_keys that is a hard link", func(t *testing.T) {
		h, m := hostFor(t)
		outside := filepath.Join(t.TempDir(), "shadow")
		if err := os.WriteFile(outside, []byte("root:!::0:::::\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.Mkdir(filepath.Join(m.Home, ".ssh"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Link(outside, filepath.Join(m.Home, ".ssh", "authorized_keys")); err != nil {
			t.Skipf("this filesystem does not do hard links: %v", err)
		}

		if _, err := h.writeKey(m, line); !errors.Is(err, ErrHomeShape) {
			t.Fatalf("writeKey = %v, want ErrHomeShape", err)
		}
		content, err := os.ReadFile(outside)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.Contains(string(content), "ssh-ed25519") {
			t.Errorf("the file the hard link named was written: %s", content)
		}
	})

	t.Run("an authorized_keys that is a directory", func(t *testing.T) {
		h, m := hostFor(t)
		if err := os.MkdirAll(filepath.Join(m.Home, ".ssh", "authorized_keys"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if _, err := h.writeKey(m, line); err == nil {
			t.Fatal("writeKey wrote into something that is not a file")
		}
	})

	t.Run("a home that is a symbolic link", func(t *testing.T) {
		h, m := hostFor(t)
		outside := t.TempDir()
		m.Home = filepath.Join(filepath.Dir(m.Home), "linked")
		if err := os.Symlink(outside, m.Home); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if _, err := h.writeKey(m, line); !errors.Is(err, ErrHomeShape) {
			t.Fatalf("writeKey = %v, want ErrHomeShape", err)
		}
	})
}

// TestCountKeysDoesNotFailAListing keeps one broken home from making
// users_list unanswerable.
func TestCountKeysDoesNotFailAListing(t *testing.T) {
	h, m := hostFor(t)
	if err := os.Symlink("/etc", filepath.Join(m.Home, ".ssh")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	count, err := h.countKeys(m)
	if err != nil || count != 0 {
		t.Errorf("countKeys = %d, %v, want no keys and no error", count, err)
	}
}

// TestCountKeysDoesNotWrite keeps a listing a listing: users_list counts every
// member's keys, and counting must not narrow a home or create a .ssh.
func TestCountKeysDoesNotWrite(t *testing.T) {
	h, m := hostFor(t)
	if err := os.Chmod(m.Home, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	count, err := h.countKeys(m)
	if err != nil || count != 0 {
		t.Fatalf("countKeys = %d, %v, want none", count, err)
	}
	if got := modeOf(t, m.Home); got != 0o755 {
		t.Errorf("the home is %o after a count, want it left at 755", got)
	}
	if _, err := os.Lstat(filepath.Join(m.Home, ".ssh")); !os.IsNotExist(err) {
		t.Errorf("counting the keys created a .ssh (%v)", err)
	}
}

// TestSubIDsAreAllocatedUnderOneLock is what stops two members getting the
// same range: the read of the highest block and the append of the next are one
// step, whichever goroutine or process asks.
func TestSubIDsAreAllocatedUnderOneLock(t *testing.T) {
	h, _ := hostFor(t)
	const members = 8

	var wg sync.WaitGroup
	errs := make(chan error, members)
	for i := range members {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.ensureSubIDs("member" + strconv.Itoa(i)); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("ensureSubIDs: %v", err)
	}

	for _, path := range []string{h.SubUID, h.SubGID} {
		lines, err := readLines(path)
		if err != nil {
			t.Fatalf("readLines: %v", err)
		}
		if len(lines) != members {
			t.Fatalf("%s holds %d blocks, want %d:\n%s", path, len(lines), members, strings.Join(lines, "\n"))
		}
		starts := map[int]string{}
		for _, line := range lines {
			name, start, count, ok := parseSubID(line)
			if !ok || count != SubIDCount {
				t.Fatalf("%q is not a block of %d", line, SubIDCount)
			}
			if other, held := starts[start]; held {
				t.Errorf("%s and %s were both given %d", other, name, start)
			}
			starts[start] = name
		}
	}
}

// TestRunningReadsTheProcessFilesystem is what removing a member waits on.
func TestRunningReadsTheProcessFilesystem(t *testing.T) {
	h, m := hostFor(t)
	if err := os.MkdirAll(filepath.Join(h.Proc, "42"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(h.Proc, "self"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	running, err := h.running(m)
	if err != nil || running != 1 {
		t.Errorf("running = %d, %v, want the one process of this uid", running, err)
	}
	// An entry that is not a number is not a process, and another uid's
	// processes are not this member's.
	other := m
	other.UID = m.UID + 4242
	if running, err := h.running(other); err != nil || running != 0 {
		t.Errorf("running for another uid = %d, %v, want none", running, err)
	}
}

// modeOf is the permission bits of one path.
func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return info.Mode().Perm()
}
