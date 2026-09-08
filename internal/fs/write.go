package fs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
)

// WriteRequest is the input of fs_write. Exactly one of Content and
// ContentBase64 is set, which the tool's input schema enforces.
//
// Author and ApprovedBy are empty on a call an agent made. They are set when
// an admin's session executes an approved write: the commit is authored by the
// member who asked for it and carries an Approved-by trailer naming the admin,
// while the committer stays the admin whose session ran it.
type WriteRequest struct {
	Path          string
	Content       *string
	ContentBase64 *string
	Message       string
	ExpectedSha   string
	Author        string
	ApprovedBy    string
}

// input is this request as the fs_write input of spec/mcp-surface.yaml, which
// is what an approval stores. The tool's input has no property this struct
// does not hold and refuses every other one, so the queued call is the call
// that was made.
func (req WriteRequest) input() (json.RawMessage, *problem.Problem) {
	body, err := json.Marshal(struct {
		Path          string  `json:"path"`
		Content       *string `json:"content,omitempty"`
		ContentBase64 *string `json:"contentBase64,omitempty"`
		Message       string  `json:"message"`
		ExpectedSha   string  `json:"expectedSha,omitempty"`
	}{
		Path:          req.Path,
		Content:       req.Content,
		ContentBase64: req.ContentBase64,
		Message:       req.Message,
		ExpectedSha:   req.ExpectedSha,
	})
	if err != nil {
		return nil, problem.Internal(req.Path, err.Error(), "")
	}
	return body, nil
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
	by := authorship{Author: req.Author, ApprovedBy: req.ApprovedBy}
	if prob := by.check(clean); prob != nil {
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
	// Lstat, not Stat: the target is judged as it is on disk, never through a
	// link. writeNoFollow refuses the link itself further down.
	if info, err := os.Lstat(clean); err == nil && info.IsDir() {
		return nil, problem.InvalidPath(clean, "the path is a folder, not a file")
	}

	folder := filepath.Dir(clean)
	if prob := s.writeVisible(folder, filepath.Base(clean)); prob != nil {
		return nil, prob
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

	// Everything above says whether this write is a write kitbash would do at
	// all. Only now is it offered to the approval queue, so a member's bad
	// path or invalid manifest is refused here and an admin never reads it.
	// Nothing has touched the filesystem yet, which is what makes a queued
	// call a call that did not happen.
	input, prob := req.input()
	if prob != nil {
		return nil, prob
	}
	if prob := s.Queue(ctx, clean, telemetry.ToolFSWrite, input); prob != nil {
		return nil, prob
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
		return nil, gitProblem(clean, err)
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

	if err := os.MkdirAll(folder, 0o755); err != nil {
		return nil, writeProblem(clean, err)
	}
	if err := writeNoFollow(clean, data); err != nil {
		return nil, writeProblem(clean, err)
	}
	rels := []string{rel}
	if err := s.stage(ctx, repo, rels); err != nil {
		return nil, gitProblem(clean, err)
	}
	// An identical write is a no operation, so the call stays idempotent.
	if !s.staged(ctx, repo, rels) && before != nil {
		return &WriteResult{Path: clean, Commit: *before}, nil
	}
	after, err := s.commitPaths(ctx, repo, req.Message, rels, by)
	if err != nil {
		return nil, gitProblem(clean, err)
	}
	return &WriteResult{Path: clean, Commit: *after}, nil
}

// stage adds repository relative paths to the index.
func (s *Service) stage(ctx context.Context, repo string, rels []string) error {
	_, err := s.git(ctx, repo, append([]string{"add", "--"}, rels...)...)
	return err
}

// staged reports whether the index differs from HEAD for these paths.
func (s *Service) staged(ctx context.Context, repo string, rels []string) bool {
	_, err := s.git(ctx, repo, append([]string{"diff", "--cached", "--quiet", "--"}, rels...)...)
	return err != nil
}

// commitPaths commits the staged paths as one commit and reads it back. Both
// fs_write and the multi file write share it, so a folder written in one call
// lands as one version.
func (s *Service) commitPaths(ctx context.Context, repo, message string, rels []string, by authorship) (*Commit, error) {
	args := append([]string{"commit", "-m", by.message(message)}, by.args()...)
	args = append(args, "--")
	args = append(args, rels...)
	if _, err := s.git(ctx, repo, args...); err != nil {
		return nil, err
	}
	commit, err := s.lastCommit(ctx, repo, rels[0])
	if err != nil {
		return nil, err
	}
	if commit == nil {
		return nil, errors.New("the commit was not recorded")
	}
	return commit, nil
}

// writeVisible applies the visibility rules to a write. Every ancestor of the
// target folder must be visible. The folder itself must be visible too, unless
// the caller is writing the manifest that brings it into the surface.
func (s *Service) writeVisible(folder, name string) *problem.Problem {
	if prob := s.ancestorsVisible(folder); prob != nil {
		return prob
	}
	if s.isRoot(folder) || name == manifest.FileName {
		return nil
	}
	if _, ok := manifest.Visible(folder); !ok {
		return notVisible(folder, "Write kitbash.yaml with name and description first")
	}
	return nil
}

// errNotRegular is the refusal of a target that exists but is not a plain
// file: a FIFO, a socket, a device.
var errNotRegular = errors.New("the path is not a regular file")

// writeNoFollow replaces a file without ever following a symlink at the final
// component, which a caller could have planted between the check and the write.
//
// O_NONBLOCK is not optional: open(2) on a FIFO for writing blocks until a
// reader appears, so a FIFO planted by the caller would hang the session. With
// it the open fails or returns at once, and the type of the descriptor is
// checked before anything is written.
func writeNoFollow(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return errNotRegular
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// writeProblem maps a failed write. A member writing into /org is refused by
// the kernel, which is the whole permission model in version 1.
func writeProblem(path string, err error) *problem.Problem {
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS) {
		return sharedReadOnly(path)
	}
	if errors.Is(err, syscall.ELOOP) {
		return symlinkRefused(path, path)
	}
	// ENXIO is a FIFO opened for writing with no reader on the other end.
	if errors.Is(err, errNotRegular) || errors.Is(err, syscall.ENXIO) {
		return problem.InvalidPathFix(path, "the path is not a regular file",
			"fs_write replaces plain files. Choose a path that is a file or does not exist yet.")
	}
	return statProblem(path, err)
}

// gitProblem maps a failed git invocation. It is the only mapper for an error
// out of Service.git, because that error carries git's standard error, which
// names host paths and internals the caller has no business seeing. Everything
// that is not a plain refusal by the operating system goes to problem.Internal,
// which writes the cause to the server log and returns a generic detail.
func gitProblem(path string, err error) *problem.Problem {
	if isPermissionDenied(err) {
		return sharedReadOnly(path)
	}
	return problem.Internal(path, err.Error(), "")
}

func sharedReadOnly(path string) *problem.Problem {
	return problem.NotPermitted(path, "the operating system refused this write",
		"This folder is shared and read only for members. A member's write to it is queued for an admin "+
			"when kitbashd is running; ask an administrator.")
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
		return gitProblem(repo, err)
	}
	return nil
}

func shaOrNone(sha string) string {
	if sha == "" {
		return "no commit, the file is not in the repository yet"
	}
	return sha
}
