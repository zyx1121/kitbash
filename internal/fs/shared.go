package fs

import (
	"os"
	"path/filepath"

	"github.com/zyx1121/kitbash/internal/safeopen"
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
//
// The folders are made one component at a time from a descriptor on the root,
// which is what keeps a folder from being created outside the root through a
// symlink swapped in while the call runs, see safeopen.
func (s *Service) makeDir(dir string) error {
	root, rel, err := s.relative(dir)
	if err != nil {
		return err
	}
	mode := homeDirMode
	if s.sharedPath(dir) {
		mode = sharedDirMode
	}
	created, err := safeopen.MkdirAll(root, rel, mode)
	if err != nil {
		return err
	}
	if !s.sharedPath(dir) {
		return nil
	}
	// Only the folders this call created are given the shared mode. A folder
	// that was already there belongs to whoever made it, and changing its mode
	// is not this write's business.
	for _, folder := range created {
		full := filepath.Join(root, folder)
		if !s.sharedPath(full) || full == s.shared {
			continue
		}
		// mkdir(2) applies the umask and drops the setgid bit, so the mode is
		// set again here.
		if err := s.chmod(full, sharedDirMode); err != nil {
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
	info, err := s.stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o060 == 0o060 {
		return nil
	}
	return s.chmod(path, sharedFileMode)
}
