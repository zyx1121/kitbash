package fs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/zyx1121/kitbash/internal/problem"
)

// RootsPath is the path reported when fs_list is called without one.
const RootsPath = "/"

// FolderEntry is one visible folder: where it is, what it is called and what
// it is for. A tool result is read by a model at a price per byte, so this is
// what deciding whether to enter a folder takes and nothing else, see PLAN.md
// section 4.5. A folder inside a Package is visible through the manifest above
// it and carries none of its own, so its description is empty.
type FolderEntry struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// FileEntry is one file directly inside a folder, named and measured. The path
// is the folder's own with this name on the end, and what a file holds is
// fs_read's answer, so neither is repeated here.
type FileEntry struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// ListResult is the output of fs_list. Files is absent at the roots, which
// answer the top level folders and nothing inside them, and absent for a
// folder that holds none.
type ListResult struct {
	Path     string         `json:"path"`
	Manifest map[string]any `json:"manifest,omitempty"`
	Folders  []FolderEntry  `json:"folders"`
	Files    []FileEntry    `json:"files,omitempty"`
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
	// and the names come from the folder that was checked. The names are all
	// that comes from the descriptor: everything said about an entry below is
	// a fresh resolution below the root, because the listing and the
	// description are two moments and the folder can change between them. The
	// open is non blocking and nothing is read until the descriptor is known to
	// be a directory, so a FIFO here is refused, not waited on.
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
	if _, prob := s.folderManifest(clean); prob != nil {
		return nil, prob
	}
	if s.isRoot(clean) {
		// A root named as a path is the roots view of that root: its visible
		// top level folders, and nothing inside them.
		return s.listRoot(clean, dir)
	}

	result := &ListResult{Path: clean, Folders: []FolderEntry{}}
	// The manifest this folder carries itself, not the one that speaks for it:
	// a folder inside a Package is described by the Package, and repeating the
	// Package's manifest in the listing of each of its folders is the cost
	// this rule was meant to remove.
	if own, held := s.visible(clean); held {
		result.Manifest = own.Raw
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
			// This folder is visible, so everything below it is: a subfolder
			// that carries a manifest is described by it, and one that does
			// not is listed by name.
			result.Folders = append(result.Folders, s.folderEntry(child))
			continue
		}
		if entry, ok := s.fileEntry(child); ok {
			result.Files = append(result.Files, entry)
		}
	}
	sortEntries(result)
	return result, nil
}

// listRoot is the roots view of one root, which is what naming it as a path
// answers. A top level folder is visible only by a manifest of its own,
// because there is nothing above it to inherit one from.
func (s *Service) listRoot(root string, dir *os.File) (*ListResult, *problem.Problem) {
	result := &ListResult{Path: root, Folders: []FolderEntry{}}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, statProblem(root, err)
	}
	for _, e := range entries {
		if !e.IsDir() || hidden(e.Name()) {
			continue
		}
		child := filepath.Join(root, e.Name())
		if _, held := s.visible(child); !held {
			continue
		}
		result.Folders = append(result.Folders, s.folderEntry(child))
	}
	sortEntries(result)
	return result, nil
}

// listRoots returns the visible top level folders. The roots themselves are
// not listed, only their visible children: a top level folder is visible only
// by a manifest of its own, because there is nothing above it to inherit one
// from. No files and nothing inside them, which is what makes the answer to
// "what is on this host" one line per folder.
func (s *Service) listRoots() (*ListResult, *problem.Problem) {
	result := &ListResult{Path: RootsPath, Folders: []FolderEntry{}}
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
			dir := filepath.Join(root, e.Name())
			if _, held := s.visible(dir); !held {
				continue
			}
			result.Folders = append(result.Folders, s.folderEntry(dir))
		}
	}
	sortEntries(result)
	return result, nil
}

// folderEntry describes a folder the caller may enter. A folder with a
// manifest of its own is named and described by it; one inside a Package is
// named by the folder itself, because the Package's description is the answer
// to the folder above and repeating it here says nothing new.
func (s *Service) folderEntry(dir string) FolderEntry {
	entry := FolderEntry{Path: dir, Name: filepath.Base(dir)}
	if m, held := s.visible(dir); held {
		entry.Name = m.Name
		entry.Description = m.Description
	}
	return entry
}

// fileEntry describes one file: its name and its size, which is what deciding
// whether to read it takes. The media type is fs_read's answer and is not
// guessed here, so a listing of a hundred files opens none of them.
//
// The size comes from a stat that resolves below the root, not from the
// directory entry: os.DirEntry.Info lstats the path again, so a folder swapped
// for a symlink between the listing and the description would put the size of
// a file outside the root on the surface. It is metadata rather than content,
// and it still is not the caller's to read.
func (s *Service) fileEntry(path string) (FileEntry, bool) {
	info, err := s.stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return FileEntry{}, false
	}
	return FileEntry{Name: filepath.Base(path), Size: info.Size()}, true
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
	sort.Slice(r.Files, func(i, j int) bool { return r.Files[i].Name < r.Files[j].Name })
}
