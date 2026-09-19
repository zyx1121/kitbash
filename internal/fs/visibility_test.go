package fs_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/problem"
)

// packageManifest is what makes a folder a Package, and the one thing an agent
// has to write before anything else in it.
const packageManifest = `name: app
description: A web application this member wrote, served over http.
deploy:
  units:
    - type: container
      build: .
      expose: http
`

// TestAFileInsideAPackageNeedsNoManifestBesideIt is the M10 failure, written
// out: the first clean agent trial paid four calls to learn that public/
// wanted a manifest of its own. The manifest of the Package speaks for the
// folders inside it, see PLAN.md section 2.1.
func TestAFileInsideAPackageNeedsNoManifestBesideIt(t *testing.T) {
	service, root := tree(t)
	ctx := context.Background()
	app := filepath.Join(root, "app")
	manifest := packageManifest
	page := "<!doctype html>\n"

	if _, prob := service.Write(ctx, fs.WriteRequest{
		Path: filepath.Join(app, "kitbash.yaml"), Content: &manifest,
		Message: "Add the app Package"}); prob != nil {
		t.Fatalf("writing the manifest: %s", prob.Detail)
	}

	out, prob := service.Write(ctx, fs.WriteRequest{
		Path: filepath.Join(app, "public", "index.html"), Content: &page,
		Message: "Add the index page"})
	if prob != nil {
		t.Fatalf("writing a file inside the Package: %s: %s", prob.Slug(), prob.Detail)
	}
	if out.Commit.Sha == "" {
		t.Errorf("the write made no commit: %+v", out)
	}

	// And what was written is readable and listable through the same rule.
	if _, prob := service.Read(ctx, filepath.Join(app, "public", "index.html"), fs.ReadOptions{}); prob != nil {
		t.Errorf("reading it back: %s: %s", prob.Slug(), prob.Detail)
	}
	listing, prob := service.List(ctx, filepath.Join(app, "public"))
	if prob != nil {
		t.Fatalf("listing the folder inside the Package: %s: %s", prob.Slug(), prob.Detail)
	}
	if len(listing.Files) != 1 || listing.Files[0].Name != "index.html" {
		t.Errorf("the listing is %+v, want index.html", listing.Files)
	}
	if listing.Manifest != nil {
		t.Errorf("a folder inside a Package carries no manifest of its own, got %v", listing.Manifest)
	}
	if _, prob := service.History(ctx, filepath.Join(app, "public", "index.html"), 0); prob != nil {
		t.Errorf("reading its history: %s: %s", prob.Slug(), prob.Detail)
	}
}

// TestAFolderWithNoManifestAboveItIsStillInvisible is the other half of the
// rule: stopping the chain at the nearest manifest is not dropping it. A top
// level folder that carries none is not on the surface, and nothing can be
// written into it.
func TestAFolderWithNoManifestAboveItIsStillInvisible(t *testing.T) {
	service, root := tree(t)
	ctx := context.Background()
	folder := filepath.Join(root, "nomanifest")
	body := "smuggled\n"

	_, prob := service.Write(ctx, fs.WriteRequest{
		Path: filepath.Join(folder, "a.txt"), Content: &body, Message: "Smuggle a file in"})
	if prob == nil {
		t.Fatal("a write into a folder with no manifest above it was accepted")
	}
	if prob.Slug() != problem.SlugNotVisible {
		t.Errorf("problem is %s, want not-visible", prob.Slug())
	}
	if prob.Instance != folder {
		t.Errorf("the problem names %s, want the folder a manifest has to go into, %s", prob.Instance, folder)
	}
	if _, err := os.Stat(filepath.Join(folder, "a.txt")); err == nil {
		t.Error("the file was written even though the write was refused")
	}
	// Two folders deep is the same answer, named at the same folder.
	if _, prob := service.Write(ctx, fs.WriteRequest{
		Path: filepath.Join(folder, "deeper", "a.txt"), Content: &body, Message: "Smuggle a file in"}); prob == nil {
		t.Error("a write two folders inside an invisible one was accepted")
	} else if prob.Instance != folder {
		t.Errorf("the problem names %s, want %s", prob.Instance, folder)
	}
	if _, prob := service.List(ctx, folder); prob == nil || prob.Slug() != problem.SlugNotVisible {
		t.Errorf("listing it answered %v, want not-visible", prob)
	}
}

// TestListOfAPackageListsItsFoldersWithAndWithoutManifests holds the shape of
// a folder listing: a subfolder with a manifest of its own is described by it,
// and one without is listed by its name, because the caller may enter both.
func TestListOfAPackageListsItsFoldersWithAndWithoutManifests(t *testing.T) {
	service, root := tree(t)

	out, prob := service.List(context.Background(), filepath.Join(root, "ffmpeg"))
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	folders := map[string]fs.FolderEntry{}
	for _, folder := range out.Folders {
		folders[filepath.Base(folder.Path)] = folder
	}
	src, held := folders["src"]
	if !held {
		t.Fatalf("the listing is %+v, want the src folder", out.Folders)
	}
	if src.Description == "" {
		t.Error("src carries a manifest of its own and is not described by it")
	}
	if out.Manifest["name"] != "ffmpeg" {
		t.Errorf("the listing carries %v, want the Package's own manifest", out.Manifest)
	}
}

// TestAFolderEntryCarriesNothingMoreThanItsDescription is the price per byte:
// what a folder listing says about a folder is where it is, what it is called
// and what it is for, see PLAN.md section 4.5.
func TestAFolderEntryCarriesNothingMoreThanItsDescription(t *testing.T) {
	service, root := tree(t)

	out, prob := service.List(context.Background(), root)
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	if len(out.Files) != 0 {
		t.Errorf("a root listing carries files: %+v", out.Files)
	}
	for _, folder := range out.Folders {
		if folder.Path == "" || folder.Name == "" {
			t.Errorf("a folder entry is incomplete: %+v", folder)
		}
	}
	listing, prob := service.List(context.Background(), filepath.Join(root, "ffmpeg", "src"))
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	for _, file := range listing.Files {
		if file.Name == "" {
			t.Errorf("a file entry has no name: %+v", file)
		}
	}
}
