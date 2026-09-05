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
// tests and for running the server outside a kitbash host.
const RootsEnv = "KITBASH_ROOTS"

// OrgRoot is the shared root every member can read.
const OrgRoot = "/org"

// Service answers the fs family for one caller. Access control is the
// kernel's: the process already runs as that caller.
type Service struct {
	user  string
	roots []string
	// resolved holds each root with symlinks evaluated, so a path the caller
	// spells through a symlinked root still matches.
	resolved []string
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
		clean := filepath.Clean(r)
		s.roots = append(s.roots, clean)
		res, err := filepath.EvalSymlinks(clean)
		if err != nil {
			res = clean
		}
		s.resolved = append(s.resolved, res)
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
	if env := os.Getenv(RootsEnv); env != "" {
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

// resolve validates one caller supplied path and returns it cleaned.
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
	if _, ok := s.rootOf(clean); ok {
		return clean, nil
	}
	// The caller may have spelled a root that is itself a symlink.
	if res, err := filepath.EvalSymlinks(clean); err == nil {
		for _, r := range s.resolved {
			if within(res, r) {
				return clean, nil
			}
		}
	}
	return "", problem.InvalidPath(p, fmt.Sprintf("the path is outside %s", strings.Join(s.roots, " and ")))
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

// folderManifest returns the manifest of a visible folder. Roots carry no
// manifest and are always readable.
func (s *Service) folderManifest(dir string) (*manifest.Manifest, *problem.Problem) {
	if s.isRoot(dir) {
		return nil, nil
	}
	m, ok := manifest.Visible(dir)
	if !ok {
		return nil, problem.NotVisible(dir, "the folder carries no kitbash.yaml with a name and a description, so it is not part of the surface")
	}
	return m, nil
}

// hidden reports whether a directory entry is kept out of the surface.
func hidden(name string) bool {
	return name == ".git" || strings.HasPrefix(name, ".")
}
