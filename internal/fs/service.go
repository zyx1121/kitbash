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

// Service answers the fs family for one caller. Access control is the
// kernel's: the process already runs as that caller.
type Service struct {
	user  string
	roots []string
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
		s.roots = append(s.roots, filepath.Clean(r))
	}
	return s, nil
}

// NewFromEnv builds the Service the server runs with: /org and the current
// user's home, unless KITBASH_ROOTS says otherwise.
func NewFromEnv() (*Service, error) {
	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("fs: reading the current user: %w", err)
	}
	name := u.Username
	if env := os.Getenv(RootsEnv); env != "" && os.Getenv(SSHEnv) == "" {
		return New(name, strings.Split(env, ":"))
	}
	home := u.HomeDir
	if home == "" {
		home = filepath.Join("/home", name)
	}
	return New(name, []string{OrgRoot, home})
}

// User is the Linux user every commit is attributed to.
func (s *Service) User() string { return s.user }

// Roots are the folders the caller may reach.
func (s *Service) Roots() []string { return append([]string(nil), s.roots...) }

// resolve validates one caller supplied path and returns it cleaned. It is the
// only way a path enters the surface, so every rule that keeps a caller inside
// the roots lives here.
//
// M1 does not follow symlinks at all. A link anywhere under a root could point
// outside it, and checking the target after the fact races the filesystem, so
// any symlink in the path is refused instead.
func (s *Service) resolve(p string) (string, *problem.Problem) {
	if p == "" {
		return "", problem.InvalidPath(p, "the path is empty")
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
// beginning with a dot are reserved, and no component may be a symlink.
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
			return problem.InvalidPathFix(clean,
				fmt.Sprintf("%s is a symlink, and kitbash does not follow symlinks", current),
				"Write the file itself instead of a link to it.")
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

// folderManifest returns the manifest of a visible folder. A folder is visible
// only when it and every ancestor below its root carry a valid manifest:
// progressive disclosure is not defeated by naming a folder deep inside an
// invisible one. Roots carry no manifest and are always readable.
func (s *Service) folderManifest(dir string) (*manifest.Manifest, *problem.Problem) {
	if s.isRoot(dir) {
		return nil, nil
	}
	m, ok := manifest.Visible(dir)
	if !ok {
		return nil, notVisible(dir, "")
	}
	if prob := s.ancestorsVisible(dir); prob != nil {
		return nil, prob
	}
	return m, nil
}

// ancestorsVisible checks every folder between dir and its root, dir excluded.
func (s *Service) ancestorsVisible(dir string) *problem.Problem {
	root, ok := s.rootOf(dir)
	if !ok {
		return problem.InvalidPath(dir, "the path is outside its root")
	}
	for current := filepath.Dir(dir); within(current, root) && current != root; current = filepath.Dir(current) {
		if _, ok := manifest.Visible(current); !ok {
			return notVisible(current, "")
		}
	}
	return nil
}

// notVisible builds the problem every tool returns for a folder outside the
// surface.
func notVisible(dir, fix string) *problem.Problem {
	return problem.NotVisible(dir,
		"the folder carries no kitbash.yaml with a name and a description, so it is not part of the surface",
		fix)
}

// hidden reports whether a directory entry is kept out of the surface.
func hidden(name string) bool {
	return name == ".git" || strings.HasPrefix(name, ".")
}
