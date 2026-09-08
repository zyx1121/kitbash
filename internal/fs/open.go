package fs

import (
	"os"
	"path/filepath"
	"syscall"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/safeopen"
)

// Every touch of a caller path goes through one of the four helpers below, so
// the root a path belongs to is looked up once and the open is made relative to
// it. resolve says the path is inside a root; these say the kernel agrees at
// the moment of the syscall, which is the part a check cannot promise on its
// own, see safeopen.

// RootOf splits a resolved path into the root it belongs to and its path below
// that root. It is what a caller outside this package needs to open something
// below a Package folder with the same guarantee the fs family gets: the root
// has to be a root the host configured, because RESOLVE_BENEATH says nothing
// about the path to the root it starts at.
func (s *Service) RootOf(clean string) (root, rel string, ok bool) {
	root, rel, err := s.relative(clean)
	if err != nil {
		return "", "", false
	}
	return root, rel, true
}

// relative splits a resolved path into the root it belongs to and its path
// below that root. A path that belongs to no root never reaches the
// filesystem: resolve refuses it first, and this is the belt under that.
func (s *Service) relative(clean string) (root, rel string, err error) {
	root, ok := s.rootOf(clean)
	if !ok {
		return "", "", &os.PathError{Op: "open", Path: clean, Err: syscall.EXDEV}
	}
	rel, err = filepath.Rel(root, clean)
	if err != nil {
		return "", "", &os.PathError{Op: "open", Path: clean, Err: syscall.EXDEV}
	}
	return root, rel, nil
}

// open opens a resolved caller path below its own root.
func (s *Service) open(clean string, flags int, perm os.FileMode) (*os.File, error) {
	root, rel, err := s.relative(clean)
	if err != nil {
		return nil, err
	}
	return safeopen.Open(root, rel, flags, perm)
}

// read opens a resolved caller path for reading. The descriptor is the whole
// answer to the call: what it reads is what the resolution checked, because
// nothing looks the path up a second time.
func (s *Service) read(clean string) (*os.File, error) {
	return s.open(clean, os.O_RDONLY, 0)
}

// stat is Lstat on a resolved caller path: a symlink is refused with ELOOP
// rather than described, which is the same answer resolve gives, and no
// component on the way is followed.
func (s *Service) stat(clean string) (os.FileInfo, error) {
	root, rel, err := s.relative(clean)
	if err != nil {
		return nil, err
	}
	return safeopen.Stat(root, rel)
}

// visible reports whether a folder below a root carries a usable manifest. It
// is manifest.Visible with the root passed in, so the manifest is opened
// through the same resolution as everything else: a folder swapped for a link
// mid call cannot lend its name and description to the surface.
func (s *Service) visible(dir string) (*manifest.Manifest, bool) {
	root, rel, err := s.relative(dir)
	if err != nil {
		return nil, false
	}
	return manifest.VisibleBelow(root, rel)
}

// chmod sets the mode of a resolved caller path through a descriptor rather
// than by name, so the mode lands on the file that was opened. chmod(2) by
// path follows symlinks, which is the one call that would undo the rest of
// this.
func (s *Service) chmod(clean string, mode os.FileMode) error {
	f, err := s.open(clean, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Chmod(mode)
}
