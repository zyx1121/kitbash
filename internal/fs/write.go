package fs

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/problem"
)

// WriteRequest is the input of fs_write. Exactly one of Content and
// ContentBase64 is set, which the tool's input schema enforces.
type WriteRequest struct {
	Path          string
	Content       *string
	ContentBase64 *string
	Message       string
	ExpectedSha   string
}

// WriteResult is the output of fs_write.
type WriteResult struct {
	Path   string `json:"path"`
	Commit Commit `json:"commit"`
}

// Write answers fs_write: it creates or replaces one file and commits it to
// the enclosing top level repository, attributed to the caller.
func (s *Service) Write(ctx context.Context, req WriteRequest) (*WriteResult, *problem.Problem) {
	clean, prob := s.resolve(req.Path)
	if prob != nil {
		return nil, prob
	}
	if s.isRoot(clean) {
		return nil, problem.InvalidPath(clean, "the path is a root, not a file")
	}
	repo, ok := s.topFolder(clean)
	if !ok || repo == clean {
		return nil, problem.InvalidPath(clean,
			"a file must live inside a top level folder, which is the git repository it is committed to")
	}
	if info, err := os.Stat(clean); err == nil && info.IsDir() {
		return nil, problem.InvalidPath(clean, "the path is a folder, not a file")
	}

	data, prob := payload(clean, req)
	if prob != nil {
		return nil, prob
	}
	if int64(len(data)) > MaxBytes {
		return nil, problem.TooLarge(clean,
			fmt.Sprintf("the content is %d bytes, over the %d byte limit", len(data), MaxBytes),
			"Split the file, or build the content inside a Package instead.")
	}
	if filepath.Base(clean) == manifest.FileName {
		if _, err := manifest.Parse(data); err != nil {
			var invalid *manifest.ErrInvalid
			if errors.As(err, &invalid) {
				return nil, problem.InvalidManifest(clean, invalid.Error())
			}
			return nil, problem.Internal(clean, err.Error(), "")
		}
	}

	rel, err := filepath.Rel(repo, clean)
	if err != nil {
		return nil, problem.Internal(clean, err.Error(), "")
	}
	if prob := s.ensureRepo(ctx, repo); prob != nil {
		return nil, prob
	}

	before, err := s.lastCommit(ctx, repo, rel)
	if err != nil {
		return nil, problem.Internal(clean, err.Error(), "")
	}
	if req.ExpectedSha != "" {
		current := ""
		if before != nil {
			current = before.Sha
		}
		if current != req.ExpectedSha {
			return nil, problem.Conflict(clean, fmt.Sprintf(
				"expectedSha %s does not match %s, the commit that last touched this file",
				req.ExpectedSha, shaOrNone(current)))
		}
	}

	if err := os.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
		return nil, statProblem(clean, err)
	}
	if err := os.WriteFile(clean, data, 0o644); err != nil {
		return nil, statProblem(clean, err)
	}
	if _, err := s.git(ctx, repo, "add", "--", rel); err != nil {
		return nil, problem.Internal(clean, err.Error(), "")
	}
	// An identical write is a no operation, so the call stays idempotent.
	if _, err := s.git(ctx, repo, "diff", "--cached", "--quiet", "--", rel); err == nil && before != nil {
		return &WriteResult{Path: clean, Commit: *before}, nil
	}
	if _, err := s.git(ctx, repo, "commit", "-m", req.Message, "--", rel); err != nil {
		return nil, problem.Internal(clean, err.Error(), "")
	}
	after, err := s.lastCommit(ctx, repo, rel)
	if err != nil || after == nil {
		return nil, problem.Internal(clean, "the commit was not recorded", "")
	}
	return &WriteResult{Path: clean, Commit: *after}, nil
}

// payload decodes the content the caller sent.
func payload(instance string, req WriteRequest) ([]byte, *problem.Problem) {
	switch {
	case req.Content != nil && req.ContentBase64 != nil:
		return nil, problem.BadRequest(instance, "content and contentBase64 are mutually exclusive",
			"Send exactly one of content and contentBase64.")
	case req.Content != nil:
		return []byte(*req.Content), nil
	case req.ContentBase64 != nil:
		data, err := base64.StdEncoding.DecodeString(*req.ContentBase64)
		if err != nil {
			return nil, problem.BadRequest(instance, "contentBase64 is not valid base64: "+err.Error(),
				"Send standard base64, or use content for UTF-8 text.")
		}
		return data, nil
	default:
		return nil, problem.BadRequest(instance, "neither content nor contentBase64 was sent",
			"Send exactly one of content and contentBase64.")
	}
}

// ensureRepo makes the top level folder a git repository, creating it when it
// does not exist yet.
func (s *Service) ensureRepo(ctx context.Context, repo string) *problem.Problem {
	if err := os.MkdirAll(repo, 0o755); err != nil {
		return statProblem(repo, err)
	}
	if isRepo(repo) {
		return nil
	}
	if _, err := s.git(ctx, repo, "init", "--quiet", "--initial-branch=main"); err != nil {
		return problem.Internal(repo, err.Error(), "")
	}
	return nil
}

func shaOrNone(sha string) string {
	if sha == "" {
		return "no commit, the file is not in the repository yet"
	}
	return sha
}
