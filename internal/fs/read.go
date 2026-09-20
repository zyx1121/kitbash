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
	// The open closes the window between the walk resolve just made and this
	// call: a component swapped for a symlink is refused by the kernel, never
	// followed. The whole call is then served from this one descriptor, so the
	// bytes returned are the bytes of the file that was checked. The open is
	// non blocking and the type of the descriptor is judged before a single
	// byte is read, so a FIFO planted by the caller is refused instead of
	// hanging the session.
	f, err := s.read(clean)
	if err != nil {
		return nil, openProblem(clean, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, statProblem(clean, err)
	}
	if info.IsDir() {
		// The default advice of invalid-path is about the shape of the path,
		// and this path is the right shape: it names a folder. Saying so, and
		// naming the two calls that do work on a folder, is what saves the
		// caller the retries the bench watched them spend, see issue #153.
		return nil, problem.InvalidPathFix(clean, "this path is a folder",
			"Call fs_list on it, or fs_read one of its files.")
	}
	if !info.Mode().IsRegular() {
		return nil, problem.InvalidPath(clean, "the path is not a regular file")
	}
	if _, prob := s.folderManifest(filepath.Dir(clean)); prob != nil {
		return nil, prob
	}

	head, prob := sniffFile(clean, f)
	if prob != nil {
		return nil, prob
	}
	mediaType := MediaType(clean, head)
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
		text, truncated, prob := readText(clean, f, info.Size(), opts)
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
		data, err := io.ReadAll(f)
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
		text, err := extractPDF(f, info.Size())
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

// readText reads a text file whole, or the window the caller asked for, from
// the descriptor Read already opened below its root.
func readText(path string, f *os.File, size int64, opts ReadOptions) (string, bool, *problem.Problem) {
	if !opts.Window {
		if size > MaxBytes {
			return "", false, problem.TooLarge(path,
				fmt.Sprintf("the file is %d bytes, over the %d byte limit", size, MaxBytes),
				"Read it in windows: pass offset and limit.")
		}
		data, err := io.ReadAll(f)
		if err != nil {
			return "", false, statProblem(path, err)
		}
		return string(data), false, nil
	}

	limit := opts.Limit
	if limit <= 0 || limit > MaxBytes {
		limit = MaxBytes
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, limit)
	n, err := f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return "", false, statProblem(path, err)
	}
	truncated := offset+int64(n) < size
	return string(buf[:n]), truncated, nil
}

// extractPDF pulls the plain text out of a PDF, best effort. It reads the
// descriptor Read opened below its root rather than opening the path again.
func extractPDF(f *os.File, size int64) (text string, err error) {
	defer func() {
		// The extractor panics on some malformed documents.
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	r, err := pdf.NewReader(f, size)
	if err != nil {
		return "", err
	}
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
	if !ok || !s.isRepo(repo) {
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
