package fs

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/zyx1121/kitbash/internal/manifest"
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
	info, err := os.Stat(clean)
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
	entries, err := os.ReadDir(clean)
	if err != nil {
		return nil, statProblem(clean, err)
	}
	for _, e := range entries {
		if hidden(e.Name()) {
			continue
		}
		child := filepath.Join(clean, e.Name())
		if e.IsDir() {
			if entry, ok := folderEntry(child); ok {
				result.Folders = append(result.Folders, entry)
			}
			continue
		}
		if entry, ok := fileEntry(child, e); ok {
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
		entries, err := os.ReadDir(root)
		if err != nil {
			// A root that is absent or unreadable simply contributes nothing.
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || hidden(e.Name()) {
				continue
			}
			if entry, ok := folderEntry(filepath.Join(root, e.Name())); ok {
				result.Folders = append(result.Folders, entry)
			}
		}
	}
	sortEntries(result)
	return result, nil
}

// folderEntry describes a folder if it is visible.
func folderEntry(dir string) (FolderEntry, bool) {
	m, ok := manifest.Visible(dir)
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
func fileEntry(path string, e os.DirEntry) (FileEntry, bool) {
	info, err := e.Info()
	if err != nil || !info.Mode().IsRegular() {
		return FileEntry{}, false
	}
	return FileEntry{
		Path:      path,
		Name:      e.Name(),
		Size:      info.Size(),
		MediaType: MediaType(path, sniff(path)),
		Modified:  info.ModTime().UTC().Format(time.RFC3339),
	}, true
}

// sniff reads the head of a file so the media type can be detected by content
// when the extension says nothing.
func sniff(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	return buf[:n]
}

func sortEntries(r *ListResult) {
	sort.Slice(r.Folders, func(i, j int) bool { return r.Folders[i].Path < r.Folders[j].Path })
	sort.Slice(r.Files, func(i, j int) bool { return r.Files[i].Path < r.Files[j].Path })
}
