package fs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/zyx1121/kitbash/internal/problem"
)

// RootsPath is the path reported when fs_list is called without one.
const RootsPath = "/"

// FolderEntry is one visible folder, name and description only.
type FolderEntry struct {
	Path        string   `json:"path"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
	Package     bool     `json:"package,omitempty"`
}

// FileEntry is one file directly inside a folder.
type FileEntry struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	MediaType string `json:"mediaType"`
	Modified  string `json:"modified,omitempty"`
}

// ListResult is the output of fs_list.
type ListResult struct {
	Path     string         `json:"path"`
	Manifest map[string]any `json:"manifest,omitempty"`
	Folders  []FolderEntry  `json:"folders"`
	Files    []FileEntry    `json:"files"`
}

// List answers fs_list. An empty path returns the visible top level folders
// across every root.
func (s *Service) List(ctx context.Context, path string) (*ListResult, *problem.Problem) {
	if path == "" {
		return s.listRoots()
	}

	clean, prob := s.resolve(path)
	if prob != nil {
		return nil, prob
	}
	// The folder is opened below its root and listed through that descriptor,
	// so a folder swapped for a symlink after resolve is refused, not followed,
	// and the entries are the entries of the folder that was checked. The open
	// is non blocking and nothing is read until the descriptor is known to be a
	// directory, so a FIFO here is refused, not waited on.
	dir, err := s.read(clean)
	if err != nil {
		return nil, openProblem(clean, err)
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil {
		return nil, statProblem(clean, err)
	}
	if !info.IsDir() {
		return nil, problem.InvalidPath(clean, "the path is a file, not a folder")
	}
	m, prob := s.folderManifest(clean)
	if prob != nil {
		return nil, prob
	}

	result := &ListResult{Path: clean, Folders: []FolderEntry{}, Files: []FileEntry{}}
	if m != nil {
		result.Manifest = m.Raw
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, statProblem(clean, err)
	}
	for _, e := range entries {
		if hidden(e.Name()) {
			continue
		}
		child := filepath.Join(clean, e.Name())
		if e.IsDir() {
			if entry, ok := s.folderEntry(child); ok {
				result.Folders = append(result.Folders, entry)
			}
			continue
		}
		if entry, ok := s.fileEntry(child, e); ok {
			result.Files = append(result.Files, entry)
		}
	}
	sortEntries(result)
	return result, nil
}

// listRoots returns the visible top level folders. The roots themselves are
// not listed, only their visible children.
func (s *Service) listRoots() (*ListResult, *problem.Problem) {
	result := &ListResult{Path: RootsPath, Folders: []FolderEntry{}, Files: []FileEntry{}}
	for _, root := range s.roots {
		f, err := s.read(root)
		if err != nil {
			// A root that is absent, unreadable, or not a folder at all simply
			// contributes nothing.
			continue
		}
		entries, err := f.ReadDir(-1)
		f.Close()
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || hidden(e.Name()) {
				continue
			}
			if entry, ok := s.folderEntry(filepath.Join(root, e.Name())); ok {
				result.Folders = append(result.Folders, entry)
			}
		}
	}
	sortEntries(result)
	return result, nil
}

// folderEntry describes a folder if it is visible.
func (s *Service) folderEntry(dir string) (FolderEntry, bool) {
	m, ok := s.visible(dir)
	if !ok {
		return FolderEntry{}, false
	}
	return FolderEntry{
		Path:        dir,
		Name:        m.Name,
		Description: m.Description,
		Tags:        m.Tags,
		Package:     m.IsPackage(),
	}, true
}

// fileEntry describes one file.
func (s *Service) fileEntry(path string, e os.DirEntry) (FileEntry, bool) {
	info, err := e.Info()
	if err != nil || !info.Mode().IsRegular() {
		return FileEntry{}, false
	}
	return FileEntry{
		Path:      path,
		Name:      e.Name(),
		Size:      info.Size(),
		MediaType: MediaType(path, s.sniff(path)),
		Modified:  info.ModTime().UTC().Format(time.RFC3339),
	}, true
}

// sniff reads the head of a file so the media type can be detected by content
// when the extension says nothing. A symlink is never sniffed: the open fails,
// and the media type falls back to the extension.
func (s *Service) sniff(path string) []byte {
	f, err := s.read(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	head, _ := sniffFile(path, f)
	return head
}

// sniffFile reads the head of an already open file. It reads at an absolute
// offset, so the descriptor's own position is left where the caller put it.
func sniffFile(path string, f *os.File) ([]byte, *problem.Problem) {
	buf := make([]byte, 512)
	n, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return nil, statProblem(path, err)
	}
	return buf[:n], nil
}

func sortEntries(r *ListResult) {
	sort.Slice(r.Folders, func(i, j int) bool { return r.Folders[i].Path < r.Folders[j].Path })
	sort.Slice(r.Files, func(i, j int) bool { return r.Files[i].Path < r.Files[j].Path })
}
