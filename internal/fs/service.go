// Package fs implements the fs tool family from spec/mcp-surface.yaml: the
// Files object, backed by the filesystem and git. It holds no MCP types so
// both kitbash-mcp and kitbashd can call it.
package fs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/problem"
)

// MaxBytes is the largest payload one fs_read or fs_write call carries.
const MaxBytes = 1 << 20

// RootsEnv overrides the roots with a colon separated list. It exists for
// tests and for running the server outside a kitbash host, so it is honoured
// only when the process is not serving an SSH session.
const RootsEnv = "KITBASH_ROOTS"

// SSHEnv is set by sshd on every session. Its presence means the caller is a
// member connecting through ForceCommand, who must not choose the roots.
const SSHEnv = "SSH_CONNECTION"

// OrgRoot is the shared root every member can read.
const OrgRoot = "/org"

// SharedEnv overrides which root a member's write is queued under. Like
// RootsEnv it exists for tests and for running the server outside a kitbash
// host, so it is honoured only when the process is not serving an SSH session.
const SharedEnv = "KITBASH_SHARED"

// Service answers the fs family for one caller. Access control is the
// kernel's: the process already runs as that caller.
type Service struct {
	user  string
	roots []string

	// shared is the root a member's write is queued under, /org on a kitbash
	// host, and approvals is the queue itself. Both are optional: without
	// them every write is attempted and the kernel has the last word.
	shared    string
	approvals Approvals
}

// New builds a Service for a named user over the given roots.
func New(username string, roots []string) (*Service, error) {
	if username == "" {
		return nil, errors.New("fs: empty user name")
	}
	if len(roots) == 0 {
		return nil, errors.New("fs: no roots")
	}
	s := &Service{user: username}
	for _, r := range roots {
		if r == "" {
			continue
		}
		if !filepath.IsAbs(r) {
			return nil, fmt.Errorf("fs: root %q is not absolute", r)
		}
		root := filepath.Clean(r)
		if root == OrgRoot {
			s.shared = root
		}
		s.roots = append(s.roots, root)
	}
	return s, nil
}

// cleanRoot normalises a root path. An empty one leaves the service without a
// shared root, which is a host where nothing is queued.
func cleanRoot(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Clean(root)
}

// NewFromEnv builds the Service the server runs with: /org and the current
// user's home, unless KITBASH_ROOTS says otherwise.
func NewFromEnv() (*Service, error) {
	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("fs: reading the current user: %w", err)
	}
	name := u.Username
	home := u.HomeDir
	if home == "" {
		home = filepath.Join("/home", name)
	}
	roots := []string{OrgRoot, home}
	if env := os.Getenv(RootsEnv); env != "" && os.Getenv(SSHEnv) == "" {
		roots = strings.Split(env, ":")
	}
	s, err := New(name, roots)
	if err != nil {
		return nil, err
	}
	if env := os.Getenv(SharedEnv); env != "" && os.Getenv(SSHEnv) == "" {
		s.SetShared(env)
	}
	return s, nil
}

// User is the Linux user every commit is attributed to.
func (s *Service) User() string { return s.user }

// Roots are the folders the caller may reach.
func (s *Service) Roots() []string { return append([]string(nil), s.roots...) }

// resolve validates one caller supplied path and returns it cleaned. It is the
// only way a path enters the surface, so every rule that keeps a caller inside
// the roots lives here.
//
// kitbash follows no symlinks at all. There is no same root exception: a link
// whose target stays inside the same root is refused exactly like a link that
// escapes, because the target of a link is not the file the caller named and
// checking a target after the fact races the filesystem.
//
// The walk below is the first belt: it names the component that is a link, and
// it applies the rules a kernel knows nothing about, such as a name beginning
// with a dot. It is not the guarantee. The guarantee is the second belt, the
// open itself: every open below a root goes through safeopen, which resolves
// the whole path in one openat2 with RESOLVE_NO_SYMLINKS and RESOLVE_BENEATH,
// so a directory swapped for a link between this walk and the open is refused
// by the kernel rather than followed.
func (s *Service) resolve(p string) (string, *problem.Problem) {
	if p == "" {
		return "", problem.InvalidPath(p, "the path is empty")
	}
	if strings.ContainsRune(p, 0) {
		// A NUL byte truncates the path inside the C library, so the kernel
		// would see a shorter path than the one that was validated here.
		return "", problem.InvalidPath(p, "the path contains a NUL byte")
	}
	if !filepath.IsAbs(p) {
		return "", problem.InvalidPath(p, "the path is not absolute")
	}
	for _, seg := range strings.Split(p, string(filepath.Separator)) {
		if seg == ".." {
			return "", problem.InvalidPath(p, "the path contains a .. segment")
		}
	}
	clean := filepath.Clean(p)
	root, ok := s.rootOf(clean)
	if !ok {
		return "", problem.InvalidPath(p, fmt.Sprintf("the path is outside %s", strings.Join(s.roots, " and ")))
	}
	if prob := checkSegments(root, clean); prob != nil {
		return "", prob
	}
	return clean, nil
}

// checkSegments walks the path one component at a time below the root. Names
// beginning with a dot are reserved, and no component may be a symlink,
// wherever that link points.
func checkSegments(root, clean string) *problem.Problem {
	rel, err := filepath.Rel(root, clean)
	if err != nil {
		return problem.InvalidPath(clean, "the path is outside its root")
	}
	if rel == "." {
		return nil
	}
	current := root
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if strings.HasPrefix(seg, ".") {
			return problem.InvalidPathFix(clean,
				fmt.Sprintf("the path component %q begins with a dot", seg),
				"Names beginning with a dot are reserved.")
		}
		current = filepath.Join(current, seg)
		info, err := os.Lstat(current)
		if err != nil {
			// The rest of the path does not exist yet, so it holds no links.
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return symlinkRefused(clean, current)
		}
	}
	return nil
}

// rootOf returns the root a cleaned path belongs to.
func (s *Service) rootOf(clean string) (string, bool) {
	for _, r := range s.roots {
		if within(clean, r) {
			return r, true
		}
	}
	return "", false
}

// isRoot reports whether a cleaned path is one of the roots itself.
func (s *Service) isRoot(clean string) bool {
	for _, r := range s.roots {
		if clean == r {
			return true
		}
	}
	return false
}

// topFolder returns the direct child of a root that encloses a path. Every top
// level folder is a git repository.
func (s *Service) topFolder(clean string) (string, bool) {
	root, ok := s.rootOf(clean)
	if !ok || clean == root {
		return "", false
	}
	rel, err := filepath.Rel(root, clean)
	if err != nil {
		return "", false
	}
	first := strings.Split(rel, string(filepath.Separator))[0]
	if first == "" || first == "." {
		return "", false
	}
	return filepath.Join(root, first), true
}

// within reports whether p is dir or lives under it.
func within(p, dir string) bool {
	if p == dir {
		return true
	}
	return strings.HasPrefix(p, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}

// statProblem maps a filesystem error onto the surface's error classes.
func statProblem(path string, err error) *problem.Problem {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return problem.NotFound(path, "no such file or folder")
	case errors.Is(err, fs.ErrPermission):
		return problem.NotPermitted(path, "the operating system refused access", "")
	default:
		return problem.Internal(path, err.Error(), "")
	}
}

// openProblem maps a failed open or stat of a caller path. openat2 reports
// ELOOP for a symlink at any component, which is the same refusal resolve
// gives, not an internal failure, and EXDEV for a resolution that left the
// root, which is the caller's mistake as well. ENOTDIR is a path naming a file
// where a folder must be.
func openProblem(path string, err error) *problem.Problem {
	switch {
	case errors.Is(err, syscall.ELOOP):
		return symlinkRefused(path, path)
	case errors.Is(err, syscall.EXDEV):
		return problem.InvalidPath(path, "the path leaves the root it started in")
	case errors.Is(err, syscall.ENOTDIR):
		return problem.InvalidPath(path, "a component of the path is a file, not a folder")
	}
	return statProblem(path, err)
}

// symlinkRefused is the single answer to a symlink on a caller path. kitbash
// follows no symlinks at all, inside the same root as well as outside it.
func symlinkRefused(instance, link string) *problem.Problem {
	return problem.InvalidPathFix(instance,
		fmt.Sprintf("%s is a symlink, and kitbash does not follow symlinks", link),
		"Use the file itself instead of a link to it.")
}

// folderManifest returns the manifest that speaks for a visible folder. A
// folder is visible when it, or a folder between it and its root, carries a
// valid manifest, and the nearest one is what describes it: the folders inside
// a Package need none of their own, see PLAN.md section 2.1. Progressive
// disclosure is not defeated by naming a folder deep inside one that has no
// manifest anywhere above it. Roots carry no manifest and are always readable.
func (s *Service) folderManifest(dir string) (*manifest.Manifest, *problem.Problem) {
	m, _, prob := s.visibleFolder(dir, nil)
	return m, prob
}

// visibleFolder is folderManifest with the manifests this call is about to
// write counted as present, keyed by absolute folder path. One fs_write holds
// the manifest of a Package and the files beneath it, and the folder those
// files are in is visible because of a manifest in the same call.
//
// It answers the folder the manifest is in as well. The manifest itself is nil
// when the folder is a root, and when what made it visible is a manifest this
// call has not written yet.
func (s *Service) visibleFolder(dir string, pending map[string]bool) (*manifest.Manifest, string, *problem.Problem) {
	if s.isRoot(dir) {
		return nil, dir, nil
	}
	root, rel, err := s.relative(dir)
	if err != nil {
		return nil, "", problem.InvalidPath(dir, "the path is outside its root")
	}
	var carries func(string) bool
	if len(pending) > 0 {
		carries = func(rel string) bool { return pending[filepath.Join(root, rel)] }
	}
	m, folder, ok := manifest.VisibleChainWith(root, rel, carries)
	if !ok {
		return nil, "", notVisible(filepath.Join(root, folder), "")
	}
	return m, filepath.Join(root, folder), nil
}

// ancestorsVisible checks the folder dir sits in, dir excluded. It is the same
// rule as folderManifest one folder higher, which is what a write that creates
// a folder asks: the folder does not exist yet, so what has to be visible is
// what is above it.
func (s *Service) ancestorsVisible(dir string) *problem.Problem {
	if _, ok := s.rootOf(dir); !ok {
		return problem.InvalidPath(dir, "the path is outside its root")
	}
	_, _, prob := s.visibleFolder(filepath.Dir(dir), nil)
	return prob
}

// notVisible builds the problem every tool returns for a folder outside the
// surface. The folder it names is the top level one the chain ended at, so the
// fix names the folder a manifest has to be written into rather than the
// folder that was asked for.
func notVisible(dir, fix string) *problem.Problem {
	return problem.NotVisible(dir,
		"this folder carries no kitbash.yaml with a name and a description, and neither does any folder above it, so it is not part of the surface",
		fix)
}

// hidden reports whether a directory entry is kept out of the surface.
func hidden(name string) bool {
	return name == ".git" || strings.HasPrefix(name, ".")
}
