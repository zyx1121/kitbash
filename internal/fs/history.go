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
	info, err := os.Stat(clean)
	if err != nil {
		return nil, statProblem(clean, err)
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
		return nil, problem.Internal(clean, err.Error(), "")
	}
	result.Commits = append(result.Commits, commits...)
	return result, nil
}
