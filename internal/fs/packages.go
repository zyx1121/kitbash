package fs

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/problem"
)

// MaxWalkDepth caps how deep Packages descends below a root. Progressive
// disclosure already stops the walk at the first folder without a manifest;
// the depth cap is the second belt, so a deep tree cannot turn one pkg_list
// into an unbounded amount of work.
const MaxWalkDepth = 8

// PackageEntry is one visible folder whose manifest carries a deploy block.
type PackageEntry struct {
	Path     string
	Manifest *manifest.Manifest
}

// Manifest returns the manifest that speaks for the visible folder at path,
// together with the folder that carries it. It is the read the pkg and proc
// families start from: they operate on a folder the caller can see, and
// nothing else.
//
// The folder is the one the manifest is in rather than the one that was named,
// because the nearest manifest speaks for everything below it: pkg_build of a
// folder inside a Package builds that Package, which is the same answer
// fs_list gives about the same folder, see PLAN.md section 2.1.
func (s *Service) Manifest(_ context.Context, path string) (*manifest.Manifest, string, *problem.Problem) {
	clean, prob := s.resolve(path)
	if prob != nil {
		return nil, "", prob
	}
	if s.isRoot(clean) {
		return nil, "", problem.InvalidPath(clean, "the path is a root, not a folder with a manifest")
	}
	info, err := s.stat(clean)
	if err != nil {
		return nil, "", openProblem(clean, err)
	}
	if !info.IsDir() {
		return nil, "", problem.InvalidPath(clean, "the path is a file, not a folder")
	}
	m, folder, prob := s.visibleFolder(clean, nil)
	if prob != nil {
		return nil, "", prob
	}
	return m, folder, nil
}

// Packages walks every visible folder under the roots and returns those whose
// manifest carries a deploy block. The walk follows progressive disclosure: a
// folder without a manifest is not entered, so nothing below an invisible
// folder is ever reported.
func (s *Service) Packages(_ context.Context) ([]PackageEntry, *problem.Problem) {
	var found []PackageEntry
	for _, root := range s.roots {
		s.walk(root, 0, &found)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Path < found[j].Path })
	return found, nil
}

// walk descends one folder. It never follows a symlink and never enters a
// folder whose name begins with a dot. The folder is listed through a
// descriptor opened below its root, so the entries belong to the folder the
// walk meant to read.
func (s *Service) walk(dir string, depth int, found *[]PackageEntry) {
	if depth > MaxWalkDepth {
		return
	}
	f, err := s.read(dir)
	if err != nil {
		// A folder the kernel will not open contributes nothing.
		return
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || hidden(e.Name()) {
			continue
		}
		child := filepath.Join(dir, e.Name())
		info, err := s.stat(child)
		if err != nil || !info.IsDir() {
			continue
		}
		m, ok := s.visible(child)
		if !ok {
			continue
		}
		if m.IsPackage() {
			*found = append(*found, PackageEntry{Path: child, Manifest: m})
		}
		s.walk(child, depth+1, found)
	}
}

// Head is the state of the repository a Package is built from.
type Head struct {
	// Sha is the HEAD commit of the enclosing top level repository.
	Sha string
	// Clean reports whether the working tree under the path matches HEAD.
	Clean bool
}

// Head reads the commit a path is at and whether anything under it is
// uncommitted. A build records the commit it came from, so it must refuse to
// build a tree that is not that commit.
func (s *Service) Head(ctx context.Context, path string) (*Head, *problem.Problem) {
	clean, prob := s.resolve(path)
	if prob != nil {
		return nil, prob
	}
	repo, ok := s.topFolder(clean)
	if !ok {
		return nil, problem.InvalidPath(clean,
			"the path is not inside a top level folder, which is the git repository it belongs to")
	}
	if !s.isRepo(repo) {
		return nil, problem.NotFoundFix(clean, "this folder is not in a git repository yet",
			"Write a file with fs_write first, which creates the repository and the first commit.")
	}
	sha, err := s.git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		if strings.Contains(err.Error(), "does not have any commits yet") ||
			strings.Contains(err.Error(), "unknown revision") {
			return nil, problem.NotFoundFix(clean, "this repository has no commit yet",
				"Write a file with fs_write first, which makes the first commit.")
		}
		return nil, gitProblem(clean, err)
	}
	rel, err := filepath.Rel(repo, clean)
	if err != nil {
		return nil, problem.Internal(clean, err.Error(), "")
	}
	args := []string{"status", "--porcelain"}
	if rel != "." {
		args = append(args, "--", rel)
	}
	status, err := s.git(ctx, repo, args...)
	if err != nil {
		return nil, gitProblem(clean, err)
	}
	return &Head{
		Sha:   strings.TrimSpace(sha),
		Clean: strings.TrimSpace(status) == "",
	}, nil
}
