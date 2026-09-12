package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/safepath"
)

// MaxFiles and MaxTotalBytes bound one multi file write. An import kit is a
// Package like any other, so what it hands back is input: a folder arriving as
// a thousand files, or as one enormous one, is refused before any of it is
// written.
const (
	MaxFiles      = 256
	MaxTotalBytes = 8 << 20
)

// File is one file of a multi file write. Exactly one of Content and
// ContentBase64 is set, which is the shape an import kit returns, see
// $defs.importFile in spec/mcp-surface.yaml.
type File struct {
	// Path is relative to the folder being written, with no leading slash and
	// no dot component.
	Path          string
	Content       *string
	ContentBase64 *string
}

// WriteFilesRequest is one multi file write: the folder, its files and the
// message they are committed with.
//
// Author and ApprovedBy are what they are on WriteRequest: empty on a call an
// agent made, set when an admin's session executes an approved pkg_import.
type WriteFilesRequest struct {
	Path       string
	Files      []File
	Message    string
	Author     string
	ApprovedBy string
}

// WriteFilesResult is the folder that was written and the one commit it landed
// as.
type WriteFilesResult struct {
	Path   string `json:"path"`
	Commit Commit `json:"commit"`
}

// WriteFiles writes several files into one folder and commits them as a single
// commit. pkg_import uses it: a Package that arrives as ten files is one
// version of Files, not ten.
//
// Every rule fs_write applies to one file applies to each of these: the folder
// resolves inside a root, every ancestor is visible, no path component begins
// with a dot, no component is a symlink, each file is at most MaxBytes, and a
// kitbash.yaml among them is validated before anything is committed.
func (s *Service) WriteFiles(ctx context.Context, req WriteFilesRequest) (*WriteFilesResult, *problem.Problem) {
	folder, files, message := req.Path, req.Files, req.Message
	clean, prob := s.resolve(folder)
	if prob != nil {
		return nil, prob
	}
	by := authorship{Author: req.Author, ApprovedBy: req.ApprovedBy}
	if prob := by.check(clean); prob != nil {
		return nil, prob
	}
	if s.isRoot(clean) {
		return nil, problem.InvalidPath(clean, "the path is a root, not a folder inside one")
	}
	if len(files) == 0 {
		return nil, problem.BadRequest(clean, "no files were given to write",
			"Send at least one file.")
	}
	if len(files) > MaxFiles {
		return nil, problem.TooLarge(clean,
			fmt.Sprintf("the write holds %d files, over the %d file limit", len(files), MaxFiles),
			"Write the folder in several calls, or import a smaller Package.")
	}
	repo, ok := s.topFolder(clean)
	if !ok {
		return nil, problem.InvalidPath(clean,
			"a folder must live under a top level folder, which is the git repository it is committed to")
	}
	if prob := s.ancestorsVisible(clean); prob != nil {
		return nil, prob
	}

	// Decode and validate everything before touching the filesystem, so a bad
	// file in the middle of the list does not leave half a folder behind.
	type pending struct {
		full string
		rel  string
		data []byte
	}
	planned := make([]pending, 0, len(files))
	seen := map[string]bool{}
	total := 0
	for _, file := range files {
		if prob := checkRelative(clean, file.Path); prob != nil {
			return nil, prob
		}
		full, prob := s.resolve(filepath.Join(clean, file.Path))
		if prob != nil {
			return nil, prob
		}
		if seen[full] {
			return nil, problem.BadRequest(full, fmt.Sprintf("%s appears twice in the file list",
				file.Path), "Send each path once.")
		}
		seen[full] = true
		data, prob := payload(full, WriteRequest{
			Path:          full,
			Content:       file.Content,
			ContentBase64: file.ContentBase64,
		})
		if prob != nil {
			return nil, prob
		}
		if int64(len(data)) > MaxBytes {
			return nil, problem.TooLarge(full,
				fmt.Sprintf("the content is %d bytes, over the %d byte limit", len(data), MaxBytes),
				"Split the file, or build the content inside a Package instead.")
		}
		total += len(data)
		if total > MaxTotalBytes {
			return nil, problem.TooLarge(clean,
				fmt.Sprintf("the write is over the %d byte limit for one folder", MaxTotalBytes),
				"Write the folder in several calls, or import a smaller Package.")
		}
		if filepath.Base(full) == manifest.FileName {
			if _, err := manifest.Parse(data); err != nil {
				var invalid *manifest.ErrInvalid
				if errors.As(err, &invalid) {
					return nil, problem.InvalidManifest(full, invalid.Error())
				}
				return nil, problem.Internal(full, err.Error(), "")
			}
		}
		rel, err := filepath.Rel(repo, full)
		if err != nil {
			return nil, problem.Internal(full, err.Error(), "")
		}
		planned = append(planned, pending{full: full, rel: rel, data: data})
	}

	if prob := s.ensureRepo(ctx, repo); prob != nil {
		return nil, prob
	}
	rels := make([]string, 0, len(planned))
	for _, p := range planned {
		if err := s.makeDir(filepath.Dir(p.full)); err != nil {
			return nil, writeProblem(p.full, err)
		}
		if err := s.writeFile(p.full, p.data); err != nil {
			return nil, writeProblem(p.full, err)
		}
		if err := s.share(p.full); err != nil {
			return nil, writeProblem(p.full, err)
		}
		rels = append(rels, p.rel)
	}

	if err := s.stage(ctx, repo, rels); err != nil {
		return nil, gitProblem(clean, err)
	}
	if !s.staged(ctx, repo, rels) {
		// Writing the same folder twice is a no operation, so the call is
		// idempotent the way every other one is.
		before, err := s.lastCommit(ctx, repo, rels[0])
		if err != nil {
			return nil, gitProblem(clean, err)
		}
		if before != nil {
			return &WriteFilesResult{Path: clean, Commit: *before}, nil
		}
	}
	commit, err := s.commitPaths(ctx, repo, message, rels, by)
	if err != nil {
		return nil, gitProblem(clean, err)
	}
	return &WriteFilesResult{Path: clean, Commit: *commit}, nil
}

// Exists reports whether a path exists, resolving it first. pkg_import uses it
// to refuse a target folder that is already there.
func (s *Service) Exists(_ context.Context, path string) (bool, *problem.Problem) {
	clean, prob := s.resolve(path)
	if prob != nil {
		return false, prob
	}
	if _, err := s.stat(clean); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, openProblem(clean, err)
	}
	return true, nil
}

// checkRelative applies the path rules to one entry of a file list. resolve
// applies the root rules once the path is joined; this says which entry of the
// list broke, and refuses the ways a relative path escapes the folder.
func checkRelative(folder, rel string) *problem.Problem {
	if rel == "." {
		return problem.InvalidPathFix(folder, "a file in the list names the folder itself",
			"Send one path per file, such as src/main.go.")
	}
	if _, err := safepath.Inside(folder, rel); err != nil {
		return problem.InvalidPathFix(filepath.Join(folder, filepath.Base(rel)), err.Error(),
			"Send plain relative paths inside the folder, such as src/main.go.")
	}
	return nil
}
