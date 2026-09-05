package fs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ledongthuc/pdf"

	"github.com/zyx1121/kitbash/internal/problem"
)

// ReadMeta is the structured output of fs_read.
type ReadMeta struct {
	Path      string `json:"path"`
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
	Sha       string `json:"sha,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ReadResult carries the content of one file plus its metadata. Exactly one of
// Text and Image is set.
type ReadResult struct {
	Meta  ReadMeta
	Text  string
	Image []byte
}

// ReadOptions is the byte window a caller asks for. Both are ignored for
// anything that is not text.
type ReadOptions struct {
	Offset int64
	Limit  int64
	Window bool
}

// Read answers fs_read.
func (s *Service) Read(ctx context.Context, path string, opts ReadOptions) (*ReadResult, *problem.Problem) {
	clean, prob := s.resolve(path)
	if prob != nil {
		return nil, prob
	}
	info, err := os.Stat(clean)
	if err != nil {
		return nil, statProblem(clean, err)
	}
	if info.IsDir() {
		return nil, problem.InvalidPath(clean, "the path is a folder, not a file")
	}
	if !info.Mode().IsRegular() {
		return nil, problem.InvalidPath(clean, "the path is not a regular file")
	}
	if _, prob := s.folderManifest(filepath.Dir(clean)); prob != nil {
		return nil, prob
	}

	mediaType := MediaType(clean, sniff(clean))
	meta := ReadMeta{Path: clean, MediaType: mediaType, Size: info.Size()}
	// A failing history lookup is a real failure, not an empty sha: it means
	// the caller cannot see the repository at all.
	sha, err := s.fileSha(ctx, clean)
	if err != nil {
		return nil, gitProblem(clean, err)
	}
	meta.Sha = sha

	switch {
	case IsText(mediaType):
		text, truncated, prob := readText(clean, info.Size(), opts)
		if prob != nil {
			return nil, prob
		}
		meta.Truncated = truncated
		return &ReadResult{Meta: meta, Text: text}, nil

	case IsImage(mediaType):
		if info.Size() > MaxBytes {
			return nil, problem.TooLarge(clean,
				fmt.Sprintf("the image is %d bytes, over the %d byte limit", info.Size(), MaxBytes),
				"Store a smaller rendition of the image next to it and read that instead.")
		}
		data, err := os.ReadFile(clean)
		if err != nil {
			return nil, statProblem(clean, err)
		}
		return &ReadResult{Meta: meta, Image: data}, nil

	case mediaType == "application/pdf":
		if info.Size() > MaxBytes {
			return nil, problem.TooLarge(clean,
				fmt.Sprintf("the document is %d bytes, over the %d byte limit", info.Size(), MaxBytes),
				"Split the document, or read a text export of it instead.")
		}
		text, err := extractPDF(clean)
		if err != nil {
			return nil, problem.Internal(clean,
				"the text of this PDF could not be extracted: "+err.Error(),
				"download via fs_read offset/limit is not supported for pdf")
		}
		return &ReadResult{Meta: meta, Text: text}, nil

	default:
		return nil, problem.UnsupportedMediaType(clean,
			fmt.Sprintf("%s is not readable through fs_read; M1 returns text, PNG, JPEG and PDF", mediaType))
	}
}

// readText reads a text file whole, or the window the caller asked for.
func readText(path string, size int64, opts ReadOptions) (string, bool, *problem.Problem) {
	if !opts.Window {
		if size > MaxBytes {
			return "", false, problem.TooLarge(path,
				fmt.Sprintf("the file is %d bytes, over the %d byte limit", size, MaxBytes),
				"Read it in windows: pass offset and limit.")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", false, statProblem(path, err)
		}
		return string(data), false, nil
	}

	limit := opts.Limit
	if limit <= 0 || limit > MaxBytes {
		limit = MaxBytes
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false, statProblem(path, err)
	}
	defer f.Close()
	if opts.Offset > 0 {
		if _, err := f.Seek(opts.Offset, io.SeekStart); err != nil {
			return "", false, statProblem(path, err)
		}
	}
	buf := make([]byte, limit)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", false, statProblem(path, err)
	}
	truncated := opts.Offset+int64(n) < size
	return string(buf[:n]), truncated, nil
}

// extractPDF pulls the plain text out of a PDF, best effort.
func extractPDF(path string) (text string, err error) {
	defer func() {
		// The extractor panics on some malformed documents.
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	plain, err := r.GetPlainText()
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(plain)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// fileSha is the commit that last touched a file.
func (s *Service) fileSha(ctx context.Context, clean string) (string, error) {
	repo, ok := s.topFolder(clean)
	if !ok || !isRepo(repo) {
		return "", nil
	}
	rel, err := filepath.Rel(repo, clean)
	if err != nil {
		return "", err
	}
	commit, err := s.lastCommit(ctx, repo, rel)
	if err != nil || commit == nil {
		return "", err
	}
	return commit.Sha, nil
}
