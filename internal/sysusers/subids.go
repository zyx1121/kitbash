package sysusers

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// The subordinate id layout rootless podman needs, see PLAN.md section 4.2.
// Every member gets one block of 65536 ids, the first at SubIDBase, and blocks
// never overlap: two members sharing a subuid range share container file
// ownership, which is not isolation at all.
const (
	SubIDBase  = 100000
	SubIDCount = 65536
	SubIDMode  = 0o644
)

// The cross process lock of the allocation. The lock is on a file of its own
// rather than on /etc/subuid, because an allocation replaces /etc/subuid by
// renaming a new file over it: a lock held on the old inode would guard a file
// nobody reads any more.
//
// The name is kitbash's own, under /run, and never /etc/subuid.lock: that name
// belongs to shadow-utils, which writes a PID into it and refuses to run when
// it finds one without, so a lock file kitbash left there stops useradd on the
// whole host, see issue #83.
//
// kitbash-adduser takes the same lock, so an admin creating a member through
// kitbashd and an operator running the console script cannot hand out the same
// range.
const (
	DefaultSubIDLock = "/run/kitbash/subids.lock"
	SubIDLockDirMode = 0o755
	SubIDLockMode    = 0o600
)

// LegacySubIDLock is where kitbash took that lock before issue #83, which is
// shadow's own lock name. An empty file there is one an older kitbash left
// behind and is removed once at start, see RemoveLegacySubIDLock.
const LegacySubIDLock = "/etc/subuid.lock"

// SubIDAttempts is how often an allocation is tried before it gives up. One
// attempt is enough under the lock; the retry is what makes the allocation
// converge anyway if a lock is ever weaker than it looks, because the file is
// read back and a block shared with another name is dropped and asked for
// again.
const SubIDAttempts = 5

// lockSubIDs takes the cross process lock and returns the release. The lock is
// advisory, which is enough: the two writers of these files are kitbashd and
// kitbash-adduser, and both take it. The directory holding it is created
// first, because /run is a tmpfs on a kitbash host and comes back empty at
// every boot.
func lockSubIDs(path string) (func(), error) {
	if path == "" {
		return nil, fmt.Errorf("sysusers: no subordinate id lock file is configured")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, SubIDLockDirMode); err != nil {
		return nil, fmt.Errorf("sysusers: lock directory %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, SubIDLockMode)
	if err != nil {
		return nil, fmt.Errorf("sysusers: open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("sysusers: lock %s: %w", path, err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// RemoveLegacySubIDLock clears the lock file an older kitbash left at shadow's
// own lock name and reports whether it removed one. Only an empty regular file
// is removed: shadow writes its PID into that file while useradd runs, so a
// file with anything in it belongs to a tool that is running now and deleting
// it would break the protocol this is fixing. A symbolic link there is not
// kitbash's either and is left alone.
//
// kitbashd calls this at start and kitbash-adduser does the same on the
// console, so a host upgraded from a version that took the wrong lock recovers
// without an operator deleting the file by hand.
func RemoveLegacySubIDLock(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sysusers: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() != 0 {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("sysusers: remove the stale lock %s: %w", path, err)
	}
	return true, nil
}

// nextSubID reads a subordinate id file and answers the first free block above
// the highest one it holds. A file that does not exist yet answers SubIDBase,
// which is what a fresh host has.
func nextSubID(path string) (int, error) {
	lines, err := readLines(path)
	if err != nil {
		return 0, err
	}
	next := SubIDBase
	for _, line := range lines {
		_, start, count, ok := parseSubID(line)
		if !ok {
			continue
		}
		if end := start + count; end > next {
			next = end
		}
	}
	return next, nil
}

// allocate gives one name a block in one file, then reads the file back: the
// name has to appear exactly once and its block has to be nobody else's. An
// allocation that lost a race is removed and asked for again rather than left
// overlapping somebody, which is what sharing a subordinate range means for
// two members' container files.
func allocate(path, name string) error {
	for range SubIDAttempts {
		has, err := hasSubID(path, name)
		if err != nil {
			return err
		}
		if has {
			return nil
		}
		start, err := nextSubID(path)
		if err != nil {
			return err
		}
		if err := appendSubID(path, name, start, SubIDCount); err != nil {
			return err
		}
		ours, err := blockIsOurs(path, name, start)
		if err != nil {
			return err
		}
		if ours {
			return nil
		}
		if _, err := removeSubID(path, name); err != nil {
			return err
		}
	}
	return fmt.Errorf("sysusers: could not allocate a subordinate id block for %s in %s", name, path)
}

// blockIsOurs reports whether the file gives this name one block and gives
// that block to nobody else.
func blockIsOurs(path, name string, start int) (bool, error) {
	lines, err := readLines(path)
	if err != nil {
		return false, err
	}
	named, sharing := 0, 0
	for _, line := range lines {
		owner, at, _, ok := parseSubID(line)
		if !ok {
			continue
		}
		if owner == name {
			named++
		}
		if at == start {
			sharing++
		}
	}
	return named == 1 && sharing == 1, nil
}

// hasSubID reports whether the file already carries a block for this name.
func hasSubID(path, name string) (bool, error) {
	lines, err := readLines(path)
	if err != nil {
		return false, err
	}
	for _, line := range lines {
		if owner, _, _, ok := parseSubID(line); ok && owner == name {
			return true, nil
		}
	}
	return false, nil
}

// appendSubID adds one block to a subordinate id file. The file is rewritten
// through a temporary file in the same directory and renamed over the
// original, so a reader never sees half a file and a crash never leaves
// /etc/subuid truncated.
func appendSubID(path, name string, start, count int) error {
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	lines = append(lines, fmt.Sprintf("%s:%d:%d", name, start, count))
	return writeLines(path, lines)
}

// removeSubID drops every block belonging to one name and reports whether it
// removed any. Leaving the block behind would hand the next member with the
// same name the same ids.
func removeSubID(path, name string) (bool, error) {
	lines, err := readLines(path)
	if err != nil {
		return false, err
	}
	kept := make([]string, 0, len(lines))
	removed := false
	for _, line := range lines {
		if owner, _, _, ok := parseSubID(line); ok && owner == name {
			removed = true
			continue
		}
		kept = append(kept, line)
	}
	if !removed {
		return false, nil
	}
	return true, writeLines(path, kept)
}

// parseSubID reads one name:start:count line.
func parseSubID(line string) (name string, start, count int, ok bool) {
	fields := strings.Split(strings.TrimSpace(line), ":")
	if len(fields) != 3 {
		return "", 0, 0, false
	}
	start, err := strconv.Atoi(fields[1])
	if err != nil {
		return "", 0, 0, false
	}
	count, err = strconv.Atoi(fields[2])
	if err != nil {
		return "", 0, 0, false
	}
	return fields[0], start, count, true
}

// readLines reads a subordinate id file, answering nothing for a file that
// does not exist. Blank lines are dropped so a rewrite does not grow them.
func readLines(path string) ([]string, error) {
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sysusers: read %s: %w", path, err)
	}
	var out []string
	for _, line := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, strings.TrimRight(line, "\r"))
		}
	}
	return out, nil
}

// writeLines replaces a subordinate id file atomically, keeping the mode
// shadow-utils expects. The content is on the disk before the rename, so a
// host that loses power during an allocation comes back with the old file or
// the new one and never with an empty one.
func writeLines(path string, lines []string) error {
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".kitbash-subid-")
	if err != nil {
		return fmt.Errorf("sysusers: write %s: %w", path, err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return fmt.Errorf("sysusers: write %s: %w", path, err)
	}
	if err := tmp.Chmod(SubIDMode); err != nil {
		tmp.Close()
		return fmt.Errorf("sysusers: write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sysusers: write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("sysusers: write %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("sysusers: write %s: %w", path, err)
	}
	return nil
}
