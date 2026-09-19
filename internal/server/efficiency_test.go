package server_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/problem"
)

// rootListingBudget is what the answer to "what is on this host" may cost. The
// first clean agent trial read 7 KiB of root listing before it wrote anything,
// see PLAN.md section 4.5. Seven folders is the size of a small organization's
// /org, and the whole listing of them is one screen.
const rootListingBudget = 1024

// TestTheRootListingOfSevenFoldersIsUnderAKibibyte measures the bytes a client
// reads, not the shape of the struct: what the tool result carries is what the
// model is charged for.
func TestTheRootListingOfSevenFoldersIsUnderAKibibyte(t *testing.T) {
	f := newFixture(t)
	// Six Packages beside the notes folder the fixture already carries, each
	// with the files and the tags a real one has, so what is measured is a
	// listing of seven folders of a member's home.
	for i := range 6 {
		name := fmt.Sprintf("service-%d", i)
		dir := filepath.Join(f.home, name)
		mkdir(t, dir)
		write(t, filepath.Join(dir, "kitbash.yaml"), fmt.Sprintf(`name: %s
description: The %s service this member runs here.
tags: [service, http, production]
deploy:
  units:
    - type: container
      build: .
      expose: http
`, name, name))
		write(t, filepath.Join(dir, "Dockerfile"), "FROM node:22-alpine\nCMD [\"node\", \"server.js\"]\n")
		write(t, filepath.Join(dir, "server.js"), "console.log('up')\n")
	}
	s := connect(t, f)

	res := call(t, s, "fs_list", map[string]any{"path": f.home})
	ok(t, res, "fs_list")
	encoded, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshaling the listing: %v", err)
	}
	// The fixture's home is a temporary folder, and its name is the test's
	// rather than the host's, so the paths are measured as the host would
	// answer them.
	measured := strings.ReplaceAll(string(encoded), f.home, "/home/tester")
	out := structured[fs.ListResult](t, res)
	if len(out.Folders) != 7 {
		t.Fatalf("the listing holds %d folders, want the six services and notes", len(out.Folders))
	}
	if len(measured) > rootListingBudget {
		t.Errorf("the root listing of %d folders is %d bytes, budget is %d:\n%s",
			len(out.Folders), len(measured), rootListingBudget, measured)
	}
	t.Logf("root listing of %d folders: %d bytes", len(out.Folders), len(measured))
	if strings.Contains(measured, "Dockerfile") {
		t.Error("a root listing carries the files of its folders")
	}
	if strings.Contains(measured, "production") {
		t.Error("a root listing carries the tags of its folders")
	}
}

// One write, one commit, any number of files, see PLAN.md section 2.1.
func TestWriteWithFilesMakesOneCommitAndNeedsNoManifestPerFolder(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)
	app := filepath.Join(f.home, "app")

	res := call(t, s, "fs_write", map[string]any{
		"files": []map[string]any{
			{"path": filepath.Join(app, "Dockerfile"), "content": "FROM node:22-alpine\n"},
			{"path": filepath.Join(app, "public", "index.html"), "content": "<!doctype html>\n"},
			{"path": filepath.Join(app, "kitbash.yaml"), "content": `name: app
description: A web application this member wrote, served over http.
deploy:
  units:
    - type: container
      build: .
      expose: http
`},
		},
		"message": "Add the app Package",
	})
	ok(t, res, "fs_write")
	out := structured[struct {
		Paths  []string `json:"paths"`
		Commit struct {
			Sha string `json:"sha"`
		} `json:"commit"`
	}](t, res)
	if len(out.Paths) != 3 {
		t.Errorf("the result names %v, want the three paths", out.Paths)
	}
	if !shaPattern.MatchString(out.Commit.Sha) {
		t.Errorf("the result carries no commit: %+v", out)
	}
	for _, path := range out.Paths {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was not written: %v", path, err)
		}
	}

	// The page is inside a folder that carries no manifest, and it is on the
	// surface because the Package's manifest speaks for it.
	page := call(t, s, "fs_read", map[string]any{"path": filepath.Join(app, "public", "index.html")})
	ok(t, page, "fs_read")
	history := structured[struct {
		Commits []struct {
			Sha string `json:"sha"`
		} `json:"commits"`
	}](t, call(t, s, "fs_history", map[string]any{"path": filepath.Join(app, "public", "index.html")}))
	if len(history.Commits) != 1 || history.Commits[0].Sha != out.Commit.Sha {
		t.Errorf("the page's history is %+v, want the one commit the list made", history.Commits)
	}
}

// The two forms are one tool and not one call: which files a call that carries
// both means is a guess, and a write is not a guess. The input schema refuses
// it and so does the handler, see mixedForms.
func TestWriteRefusesBothFormsAtOnce(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)
	notes := filepath.Join(f.home, "notes")

	p := problemOf(t, call(t, s, "fs_write", map[string]any{
		"path":    filepath.Join(notes, "one.md"),
		"content": "one\n",
		"files": []map[string]any{
			{"path": filepath.Join(notes, "two.md"), "content": "two\n"},
		},
		"message": "Write both ways at once",
	}))
	if p.Slug() != problem.SlugBadRequest {
		t.Errorf("slug is %s, want bad-request", p.Slug())
	}
	for _, name := range []string{"one.md", "two.md"} {
		if _, err := os.Stat(filepath.Join(notes, name)); err == nil {
			t.Errorf("%s was written by a refused call", name)
		}
	}
}

func TestWriteWithFilesRefusesAnInvisiblePathWhole(t *testing.T) {
	f := newFixture(t)
	s := connect(t, f)

	p := problemOf(t, call(t, s, "fs_write", map[string]any{
		"files": []map[string]any{
			{"path": filepath.Join(f.org, "nomanifest", "a.md"), "content": "a\n"},
			{"path": filepath.Join(f.org, "nomanifest", "b.md"), "content": "b\n"},
		},
		"message": "Write into a folder with no manifest",
	}))
	if p.Slug() != problem.SlugNotVisible {
		t.Errorf("slug is %s, want not-visible", p.Slug())
	}
	if _, err := os.Stat(filepath.Join(f.org, "nomanifest", "a.md")); err == nil {
		t.Error("the refused list wrote something")
	}
}
