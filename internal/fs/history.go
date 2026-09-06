package fs

import (
	"context"
	"os"
	"path/filepath"

	"github.com/zyx1121/kitbash/internal/problem"
)

// DefaultHistoryLimit matches the default in spec/mcp-surface.yaml.
const DefaultHistoryLimit = 20

// HistoryResult is the output of fs_history.
type HistoryResult struct {
	Path    string   `json:"path"`
	Commits []Commit `json:"commits"`
}

// History answers fs_history: the commits that touched a path, newest first.
func (s *Service) History(ctx context.Context, path string, limit int) (*HistoryResult, *problem.Problem) {
	clean, prob := s.resolve(path)
	if prob != nil {
		return nil, prob
	}
	if limit <= 0 {
		limit = DefaultHistoryLimit
	}
	// Lstat, not Stat: a symlink swapped in after resolve is refused here
	// rather than resolved to whatever it points at.
	info, err := os.Lstat(clean)
	if err != nil {
		return nil, statProblem(clean, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, symlinkRefused(clean, clean)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		// A FIFO or a device is not something the surface has a history for.
		return nil, problem.InvalidPath(clean, "the path is not a regular file")
	}
	folder := clean
	if !info.IsDir() {
		folder = filepath.Dir(clean)
	}
	if _, prob := s.folderManifest(folder); prob != nil {
		return nil, prob
	}

	result := &HistoryResult{Path: clean, Commits: []Commit{}}
	repo, ok := s.topFolder(clean)
	if !ok || !isRepo(repo) {
		return result, nil
	}
	rel, err := filepath.Rel(repo, clean)
	if err != nil {
		return nil, problem.Internal(clean, err.Error(), "")
	}
	commits, err := s.history(ctx, repo, rel, limit)
	if err != nil {
		// gitProblem, not a raw detail: git's own output never reaches the
		// agent, it goes to the server log through problem.Internal.
		return nil, gitProblem(clean, err)
	}
	result.Commits = append(result.Commits, commits...)
	return result, nil
}
