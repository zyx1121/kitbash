package fs

import (
	"os"
	"path/filepath"
)

// The modes of what an admin writes under the shared root. /org is 2775 and
// owned by kitbash-admin, so a file one admin writes has to stay writable by
// the group or the next admin cannot replace it, and a folder has to keep the
// setgid bit or what is written inside it leaves the group. The process umask
// is 022 on a normal host, which is why these are set rather than left to the
// open and the mkdir, see PLAN.md section 2.1.
const (
	sharedFileMode = os.FileMode(0o664)
	sharedDirMode  = os.ModeSetgid | os.FileMode(0o775)
)

// The default modes outside the shared root. A member's home is theirs alone,
// so nothing widens it.
const (
	homeFileMode = os.FileMode(0o644)
	homeDirMode  = os.FileMode(0o755)
)

// sharedPath reports whether a resolved path lies under the shared root.
func (s *Service) sharedPath(clean string) bool {
	return s.shared != "" && within(clean, s.shared)
}

// makeDir creates a folder and every missing folder above it. Under the shared
// root each folder it creates is made setgid and group writable; elsewhere the
// default mode is the caller's own.
func (s *Service) makeDir(dir string) error {
	if !s.sharedPath(dir) {
		return os.MkdirAll(dir, homeDirMode)
	}
	// Only the folders this call creates are given the shared mode. A folder
	// that was already there belongs to whoever made it, and changing its mode
	// is not this write's business.
	var created []string
	for current := dir; s.sharedPath(current) && current != s.shared; current = filepath.Dir(current) {
		if _, err := os.Lstat(current); err == nil {
			break
		}
		created = append(created, current)
	}
	if err := os.MkdirAll(dir, sharedDirMode); err != nil {
		return err
	}
	for _, folder := range created {
		// mkdir(2) applies the umask and drops the setgid bit, so the mode is
		// set again here.
		if err := os.Chmod(folder, sharedDirMode); err != nil {
			return err
		}
	}
	return nil
}

// share makes a file under the shared root group writable, so the next admin
// can replace what this one wrote.
//
// A file that is already group writable is left alone: chmod belongs to the
// owner, and under /org the owner may be another admin whose file this call
// only rewrote through the group permission.
func (s *Service) share(path string) error {
	if !s.sharedPath(path) {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o060 == 0o060 {
		return nil
	}
	return os.Chmod(path, sharedFileMode)
}
