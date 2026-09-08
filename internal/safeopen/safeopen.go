// Package safeopen opens a file below a root without following a symlink at
// any component of the path, the middle ones included.
//
// A lexical check plus a walk with Lstat answers where a path points now, not
// where it will point when the open runs. A caller who can write a folder can
// swap an intermediate directory for a symlink in that window, and an open
// with O_NOFOLLOW still follows it, because O_NOFOLLOW covers the final
// component only. The kernel closes the window: openat2(2) resolves the whole
// path in one syscall, RESOLVE_NO_SYMLINKS refuses every symlink on the way
// and RESOLVE_BENEATH refuses every resolution that leaves the root, which is
// what spec/mcp-surface.yaml promises for every open below a root.
//
// The root itself is the trust boundary and is opened by path: it is a folder
// the host configured, not a path a caller sent. It must not be a symlink,
// which is checked before it is opened.
package safeopen

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

// resolveFlags are the three refusals every open below a root is made with.
// RESOLVE_NO_MAGICLINKS is the third: a /proc/self/fd entry is not a symlink
// on disk that the other two flags would see, and it is a way out of the tree.
const resolveFlags = unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS

// maxRetries bounds the retry loop. openat2 answers EAGAIN when the path was
// renamed underneath the resolution, which is a race worth retrying, and EINTR
// when a signal arrived. Neither is worth retrying forever.
const maxRetries = 16

// Open opens rel below root and returns the open file. rel is relative to
// root, and "." names root itself.
//
// O_CLOEXEC is always added, so a descriptor never leaks into a container the
// caller starts, and O_NONBLOCK is added to every open that reads or writes.
// The second is not optional: open(2) on a FIFO blocks until the other end
// appears, and a caller can put a FIFO anywhere they can write, so without it
// a planted pipe hangs the session. The caller still has to judge what it
// opened: this function refuses symlinks and escapes, not FIFOs, sockets or
// devices.
//
// perm is used only with O_CREATE, which is what openat2 itself insists on.
func Open(root, rel string, flags int, perm os.FileMode) (*os.File, error) {
	rel, err := clean(root, rel)
	if err != nil {
		return nil, err
	}
	full := filepath.Join(root, rel)
	dir, err := openRoot(root)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dir)

	flags |= unix.O_CLOEXEC
	if flags&unix.O_PATH == 0 {
		// openat2 accepts O_CLOEXEC, O_DIRECTORY and O_NOFOLLOW beside O_PATH
		// and refuses every other flag, and an O_PATH open reads nothing, so
		// it cannot block on a FIFO in the first place.
		flags |= unix.O_NONBLOCK
	}
	if available() {
		fd, err := openat2(dir, rel, flags, perm)
		if err == nil {
			return os.NewFile(uintptr(fd), full), nil
		}
		if !errors.Is(err, unix.ENOSYS) {
			return nil, &os.PathError{Op: "openat2", Path: full, Err: err}
		}
		// A kernel before 5.6 has no openat2 at all. The walk below is what
		// kitbash did before, so an old kernel keeps working with the weaker
		// guarantee it always had.
		unavailable()
	}
	return openWalk(root, rel, full, flags, perm)
}

// Stat is Lstat below a root: it says what rel is without following a symlink
// anywhere on the way, and a symlink at rel itself is refused rather than
// described, because kitbash never acts on the target of a link.
//
// O_PATH opens the file without reading it, so a stat needs no permission on
// the file itself and a FIFO is described rather than waited on.
func Stat(root, rel string) (os.FileInfo, error) {
	f, err := Open(root, rel, unix.O_PATH, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}

// MkdirAll creates rel below root and every missing folder above it, and
// returns the folders it created, relative to root and outermost first.
//
// It descends one component at a time from a descriptor on the root, so no
// component is ever named by a path the kernel resolves on our behalf: a
// directory swapped for a symlink mid call cannot make a folder appear outside
// the root. A component that already exists is entered, whatever created it; a
// component that exists and is not a folder is ENOTDIR, the same answer a
// plain MkdirAll gives.
func MkdirAll(root, rel string, perm os.FileMode) ([]string, error) {
	rel, err := clean(root, rel)
	if err != nil {
		return nil, err
	}
	dir, err := openRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { unix.Close(dir) }()

	var created []string
	current := "."
	for _, segment := range segments(rel) {
		current = filepath.Join(current, segment)
		next, err := descend(dir, segment)
		if errors.Is(err, unix.ENOENT) {
			if err := unix.Mkdirat(dir, segment, uint32(perm.Perm())); err != nil && !errors.Is(err, unix.EEXIST) {
				return created, &os.PathError{Op: "mkdirat", Path: filepath.Join(root, current), Err: err}
			}
			created = append(created, current)
			next, err = descend(dir, segment)
		}
		if err != nil {
			return created, &os.PathError{Op: "openat2", Path: filepath.Join(root, current), Err: err}
		}
		unix.Close(dir)
		dir = next
	}
	return created, nil
}

// descend opens one child folder of an open directory. A single component
// cannot be resolved through a symlink planted higher up, because the parent is
// already an open descriptor, and RESOLVE_NO_SYMLINKS refuses the component
// itself being a link.
func descend(dir int, segment string) (int, error) {
	if available() {
		fd, err := openat2(dir, segment, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err == nil {
			return fd, nil
		}
		if !errors.Is(err, unix.ENOSYS) {
			return -1, err
		}
		unavailable()
	}
	return unix.Openat(dir, segment,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
}

// openat2 is the one syscall this package exists for, with the retries its
// manual page asks for.
func openat2(dir int, rel string, flags int, perm os.FileMode) (int, error) {
	how := unix.OpenHow{Flags: uint64(uint32(flags)), Resolve: resolveFlags}
	if flags&os.O_CREATE != 0 {
		// openat2 refuses a mode on an open that creates nothing, so the mode
		// is carried only when it means something.
		how.Mode = uint64(perm.Perm())
	}
	for attempt := 0; ; attempt++ {
		fd, err := unix.Openat2(dir, rel, &how)
		if err == nil {
			return fd, nil
		}
		if attempt >= maxRetries || (!errors.Is(err, unix.EINTR) && !errors.Is(err, unix.EAGAIN)) {
			return -1, err
		}
	}
}

// openRoot opens the root as the descriptor every other resolution starts
// from. O_PATH is enough: nothing is read through it, it only names the folder
// the resolution may not leave.
func openRoot(root string) (int, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return -1, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return -1, &os.PathError{Op: "open", Path: root, Err: syscall.ELOOP}
	}
	if !info.IsDir() {
		return -1, &os.PathError{Op: "open", Path: root, Err: syscall.ENOTDIR}
	}
	fd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: root, Err: err}
	}
	return fd, nil
}

// clean applies the lexical rules, which stay the first belt. openat2 answers
// EXDEV for both of these on its own; refusing them here means the path in the
// error names what the caller asked for rather than what filepath.Join made of
// it.
func clean(root, rel string) (string, error) {
	if rel == "" {
		return ".", nil
	}
	if filepath.IsAbs(rel) {
		return "", &os.PathError{Op: "open", Path: rel, Err: syscall.EXDEV}
	}
	for _, segment := range segments(rel) {
		if segment == ".." {
			return "", &os.PathError{Op: "open", Path: filepath.Join(root, filepath.Clean(rel)), Err: syscall.EXDEV}
		}
	}
	return rel, nil
}

// segments are the names a relative path is made of, empty ones and dots
// dropped.
func segments(rel string) []string {
	var out []string
	for _, segment := range strings.Split(filepath.ToSlash(rel), "/") {
		if segment == "" || segment == "." {
			continue
		}
		out = append(out, segment)
	}
	return out
}

// openWalk is what a kernel without openat2 gets: the per component Lstat walk
// plus O_NOFOLLOW on the open. It is the guarantee kitbash had before, a check
// that can race, which is why it is the fallback and not the implementation.
func openWalk(root, rel, full string, flags int, perm os.FileMode) (*os.File, error) {
	current := root
	for _, segment := range segments(rel) {
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err != nil {
			// The rest of the path does not exist yet, so it holds no links,
			// and the open below reports whatever is missing.
			break
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, &os.PathError{Op: "open", Path: current, Err: syscall.ELOOP}
		}
	}
	return os.OpenFile(full, flags|syscall.O_NOFOLLOW, perm)
}

// missing records that this kernel has no openat2, so the fallback is taken
// straight away rather than after a failed syscall on every open.
var missing atomic.Bool

var warnOnce sync.Once

func available() bool { return !missing.Load() }

func unavailable() {
	missing.Store(true)
	warnOnce.Do(func() {
		log.New(os.Stderr, "kitbash: ", log.LstdFlags).Print(
			"this kernel has no openat2, so a path below a root is resolved with a per component walk " +
				"instead; a symlink swapped in during a call is then refused by a check that can race, " +
				"see spec/mcp-surface.yaml. Run kitbash on Linux 5.6 or newer.")
	})
}
