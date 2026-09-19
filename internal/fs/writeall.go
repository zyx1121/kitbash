package fs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
)

// MaxWriteFiles and MaxWriteTotalBytes bound one fs_write that carries a list.
// A Package is a handful of files that belong together, not a tree, and the
// caller writing a tree is writing it one Package at a time.
//
// They are not the import limits of WriteFiles: an import kit hands back what
// an npm package holds, which is many small files, while this list is written
// by an agent that has each file in its context already.
const (
	MaxWriteFiles      = 64
	MaxWriteTotalBytes = 16 << 20
)

// WriteFileEntry is one file of a list write: where it goes and what is in it.
// Exactly one of Content and ContentBase64 is set, which the tool's input
// schema enforces.
type WriteFileEntry struct {
	Path          string
	Content       *string
	ContentBase64 *string
}

// WriteAllRequest is the list form of fs_write: several files, one message,
// one commit. Every file lands in the same top level folder, which is the git
// repository the commit is made in.
//
// ExpectedSha is the optimistic lock, and for a list it is read against the
// repository's head rather than against one file: what this call replaces is a
// state of the repository.
//
// Author and ApprovedBy are what they are on WriteRequest: empty on a call an
// agent made, set when an admin's session executes an approved write.
type WriteAllRequest struct {
	Files       []WriteFileEntry
	Message     string
	ExpectedSha string
	Author      string
	ApprovedBy  string
}

// input is this request as the fs_write input of spec/mcp-surface.yaml, which
// is what an approval stores. A queued list is one approval an admin approves
// once, not one per file.
func (req WriteAllRequest) input(instance string) (json.RawMessage, *problem.Problem) {
	type file struct {
		Path          string  `json:"path"`
		Content       *string `json:"content,omitempty"`
		ContentBase64 *string `json:"contentBase64,omitempty"`
	}
	files := make([]file, 0, len(req.Files))
	for _, f := range req.Files {
		files = append(files, file{Path: f.Path, Content: f.Content, ContentBase64: f.ContentBase64})
	}
	body, err := json.Marshal(struct {
		Files       []file `json:"files"`
		Message     string `json:"message"`
		ExpectedSha string `json:"expectedSha,omitempty"`
	}{Files: files, Message: req.Message, ExpectedSha: req.ExpectedSha})
	if err != nil {
		return nil, problem.Internal(instance, err.Error(), "")
	}
	return body, nil
}

// WriteAllResult is the output of the list form: every path that was written
// and the one commit they landed as.
type WriteAllResult struct {
	Paths  []string `json:"paths"`
	Commit Commit   `json:"commit"`
}

// WriteAll answers fs_write when it carries a list of files. A Package is
// several files that belong together and the history should say so, so the
// whole list is one commit, see PLAN.md section 2.1.
//
// Every rule the one file form applies is applied to each file, and applied to
// all of them before any of them is written: a list holding one path the
// caller may not write is refused whole, and nothing of it reaches the disk.
// The manifests in the list count as written while the list is judged, which
// is what lets one call write a Package's kitbash.yaml and the files beneath
// it, including the files of a folder that carries no manifest of its own.
func (s *Service) WriteAll(ctx context.Context, req WriteAllRequest) (*WriteAllResult, *problem.Problem) {
	if len(req.Files) == 0 {
		return nil, problem.BadRequest("", "files is empty, so this call writes nothing",
			"Send one file in path and content, or at least one entry in files.")
	}
	if len(req.Files) > MaxWriteFiles {
		return nil, problem.TooLarge(req.Files[0].Path,
			fmt.Sprintf("the call holds %d files, over the %d file limit", len(req.Files), MaxWriteFiles),
			"Write the folder in several calls, one Package at a time.")
	}

	// Nothing below touches the filesystem. Every path is resolved, every
	// content decoded and every manifest parsed first, so a list that is
	// refused is a list that did not happen.
	type planned struct {
		full string
		rel  string
		data []byte
	}
	var (
		repo    string
		work    = make([]planned, 0, len(req.Files))
		seen    = map[string]bool{}
		pending = map[string]bool{}
		total   int
	)
	for _, file := range req.Files {
		clean, prob := s.resolve(file.Path)
		if prob != nil {
			return nil, prob
		}
		if s.isRoot(clean) {
			return nil, problem.InvalidPath(clean, "the path is a root, not a file")
		}
		top, ok := s.topFolder(clean)
		if !ok || top == clean {
			return nil, problem.InvalidPath(clean,
				"a file must live inside a top level folder, which is the git repository it is committed to")
		}
		if repo == "" {
			repo = top
		}
		if top != repo {
			// One call is one commit, and a commit belongs to one repository.
			return nil, problem.BadRequest(clean,
				fmt.Sprintf("%s is in %s and the files before it are in %s, and one write is one commit in one repository",
					clean, top, repo),
				"Write each top level folder with its own fs_write.")
		}
		if seen[clean] {
			return nil, problem.BadRequest(clean, fmt.Sprintf("%s appears twice in files", clean),
				"Send each path once, with the content it should end up holding.")
		}
		seen[clean] = true
		if info, err := s.stat(clean); err == nil && info.IsDir() {
			return nil, problem.InvalidPath(clean, "the path is a folder, not a file")
		}
		data, prob := payload(clean, WriteRequest{
			Path:          clean,
			Content:       file.Content,
			ContentBase64: file.ContentBase64,
		})
		if prob != nil {
			return nil, prob
		}
		if int64(len(data)) > MaxBytes {
			return nil, problem.TooLarge(clean,
				fmt.Sprintf("the content is %d bytes, over the %d byte limit", len(data), MaxBytes),
				"Split the file, or build the content inside a Package instead.")
		}
		total += len(data)
		if total > MaxWriteTotalBytes {
			return nil, problem.TooLarge(clean,
				fmt.Sprintf("the call is over the %d byte limit for one write", MaxWriteTotalBytes),
				"Write the folder in several calls, one Package at a time.")
		}
		if filepath.Base(clean) == manifest.FileName {
			if _, err := manifest.Parse(data); err != nil {
				var invalid *manifest.ErrInvalid
				if errors.As(err, &invalid) {
					return nil, problem.InvalidManifest(clean, invalid.Error())
				}
				return nil, problem.Internal(clean, err.Error(), "")
			}
			pending[filepath.Dir(clean)] = true
		}
		rel, err := filepath.Rel(repo, clean)
		if err != nil {
			return nil, problem.Internal(clean, err.Error(), "")
		}
		work = append(work, planned{full: clean, rel: rel, data: data})
	}

	by := authorship{Author: req.Author, ApprovedBy: req.ApprovedBy}
	if prob := by.check(repo); prob != nil {
		return nil, prob
	}
	// The visibility rule reads the manifests of this call as written, so the
	// order of the list says nothing: the manifest of a Package makes every
	// file below it writable whether it came first or last.
	for _, p := range work {
		if prob := s.writeVisible(filepath.Dir(p.full), filepath.Base(p.full), pending); prob != nil {
			return nil, prob
		}
	}

	// The list is a call kitbash would make. Only now is it offered to the
	// queue, as one call: an admin approves a Package, not thirteen files.
	input, prob := req.input(repo)
	if prob != nil {
		return nil, prob
	}
	if prob := s.Queue(ctx, repo, telemetry.ToolFSWrite, input); prob != nil {
		return nil, prob
	}

	if prob := s.ensureRepo(ctx, repo); prob != nil {
		return nil, prob
	}
	if req.ExpectedSha != "" {
		head, err := s.headSha(ctx, repo)
		if err != nil {
			return nil, gitProblem(repo, err)
		}
		if head != req.ExpectedSha {
			return nil, problem.Conflict(repo, fmt.Sprintf(
				"expectedSha %s does not match %s, the head of %s",
				req.ExpectedSha, headOrNone(head), repo))
		}
	}

	paths := make([]string, 0, len(work))
	rels := make([]string, 0, len(work))
	for _, p := range work {
		if err := s.makeDir(filepath.Dir(p.full)); err != nil {
			return nil, writeProblem(p.full, err)
		}
		if err := s.writeFile(p.full, p.data); err != nil {
			return nil, writeProblem(p.full, err)
		}
		if err := s.share(p.full); err != nil {
			return nil, writeProblem(p.full, err)
		}
		paths = append(paths, p.full)
		rels = append(rels, p.rel)
	}
	if err := s.stage(ctx, repo, rels); err != nil {
		return nil, gitProblem(repo, err)
	}
	if !s.staged(ctx, repo, rels) {
		// Writing the same files twice is a no operation, so the call is
		// idempotent the way the one file form is.
		before, err := s.lastCommit(ctx, repo, rels[0])
		if err != nil {
			return nil, gitProblem(repo, err)
		}
		if before != nil {
			return &WriteAllResult{Paths: paths, Commit: *before}, nil
		}
	}
	commit, err := s.commitPaths(ctx, repo, req.Message, rels, by)
	if err != nil {
		return nil, gitProblem(repo, err)
	}
	return &WriteAllResult{Paths: paths, Commit: *commit}, nil
}

// headOrNone names the head of a repository, or says it has none.
func headOrNone(sha string) string {
	if sha == "" {
		return "no commit, the repository is empty"
	}
	return sha
}

// headSha is the commit a repository is at, empty when it has none yet. It is
// what expectedSha is held against for a list, because a list is a change to
// the repository rather than to one file.
func (s *Service) headSha(ctx context.Context, repo string) (string, error) {
	out, err := s.git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		if strings.Contains(err.Error(), "does not have any commits yet") ||
			strings.Contains(err.Error(), "unknown revision") ||
			strings.Contains(err.Error(), "ambiguous argument") {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}
