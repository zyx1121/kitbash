package fs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
)

// text is one file of a list write.
func text(s string) *string { return &s }

// aPackage is the list an agent writes to make a Package: the manifest, the
// Containerfile and a file in a folder that carries no manifest of its own.
// The manifest is last on purpose, because the rule reads the whole list
// before it judges any of it.
func aPackage(folder string) []fs.WriteFileEntry {
	return []fs.WriteFileEntry{
		{Path: filepath.Join(folder, "Dockerfile"), Content: text("FROM node:22-alpine\nCMD [\"node\", \"server.js\"]\n")},
		{Path: filepath.Join(folder, "server.js"), Content: text("console.log('up')\n")},
		{Path: filepath.Join(folder, "public", "index.html"), Content: text("<!doctype html>\n")},
		{Path: filepath.Join(folder, "kitbash.yaml"), Content: text(packageManifest)},
	}
}

// One write, one commit, any number of files: writing a Package one file at a
// time cost the first clean agent trial thirteen calls, see PLAN.md 2.1.
func TestWriteAllMakesOneCommitOfAWholePackage(t *testing.T) {
	service, root := tree(t)
	folder := filepath.Join(root, "app")

	out, prob := service.WriteAll(context.Background(), fs.WriteAllRequest{
		Files:   aPackage(folder),
		Message: "Add the app Package"})
	if prob != nil {
		t.Fatalf("WriteAll: %s: %s", prob.Slug(), prob.Detail)
	}
	if len(out.Paths) != 4 {
		t.Errorf("the result names %v, want every path that was written", out.Paths)
	}
	for _, path := range out.Paths {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was not written: %v", path, err)
		}
	}
	if out.Commit.Sha == "" || out.Commit.Author != "tester" {
		t.Errorf("commit is %+v, want one attributed to tester", out.Commit)
	}
	if got := commitCount(t, folder); got != 1 {
		t.Errorf("the repository has %d commits, want 1", got)
	}
	// The file inside the folder that carries no manifest is in that commit,
	// which is the M10 failure as one call rather than five.
	history, prob := service.History(context.Background(), filepath.Join(folder, "public", "index.html"), 0)
	if prob != nil {
		t.Fatalf("History: %s", prob.Detail)
	}
	if len(history.Commits) != 1 || history.Commits[0].Sha != out.Commit.Sha {
		t.Errorf("the page's history is %+v, want the one commit the list made", history.Commits)
	}
}

func TestWriteAllRefusesFilesFromTwoRepositories(t *testing.T) {
	service, root := tree(t)

	_, prob := service.WriteAll(context.Background(), fs.WriteAllRequest{
		Files: []fs.WriteFileEntry{
			{Path: filepath.Join(root, "app", "kitbash.yaml"), Content: text(packageManifest)},
			{Path: filepath.Join(root, "handbook", "README.md"), Content: text("# handbook\n")},
		},
		Message: "Write two repositories at once"})
	if prob == nil {
		t.Fatal("a list spanning two top level folders was accepted")
	}
	if prob.Slug() != problem.SlugBadRequest {
		t.Errorf("problem is %s, want bad-request", prob.Slug())
	}
	if _, err := os.Stat(filepath.Join(root, "app")); err == nil {
		t.Error("the refused list wrote something")
	}
}

func TestWriteAllRefusesADuplicatePath(t *testing.T) {
	service, root := tree(t)
	folder := filepath.Join(root, "app")

	_, prob := service.WriteAll(context.Background(), fs.WriteAllRequest{
		Files: []fs.WriteFileEntry{
			{Path: filepath.Join(folder, "kitbash.yaml"), Content: text(packageManifest)},
			{Path: filepath.Join(folder, "server.js"), Content: text("one\n")},
			{Path: filepath.Join(folder, "server.js"), Content: text("two\n")},
		},
		Message: "Write one file twice"})
	if prob == nil {
		t.Fatal("a list naming one path twice was accepted")
	}
	if prob.Slug() != problem.SlugBadRequest {
		t.Errorf("problem is %s, want bad-request", prob.Slug())
	}
	if _, err := os.Stat(filepath.Join(folder, "server.js")); err == nil {
		t.Error("the refused list wrote something")
	}
}

// A list is refused whole. One path the caller may not write is a call that
// did not happen, not a folder half written.
func TestWriteAllRefusesTheWholeListForOneInvisiblePath(t *testing.T) {
	service, root := tree(t)
	folder := filepath.Join(root, "app")

	_, prob := service.WriteAll(context.Background(), fs.WriteAllRequest{
		Files: []fs.WriteFileEntry{
			{Path: filepath.Join(folder, "kitbash.yaml"), Content: text(packageManifest)},
			{Path: filepath.Join(folder, "server.js"), Content: text("up\n")},
			{Path: filepath.Join(root, "nomanifest", "smuggled.txt"), Content: text("smuggled\n")},
		},
		Message: "Write a Package and one file elsewhere"})
	if prob == nil {
		t.Fatal("a list holding a path with no manifest above it was accepted")
	}
	// The path is in another top level folder, so this is refused before
	// visibility even comes up; the visible case is the one below.
	if prob.Slug() != problem.SlugBadRequest && prob.Slug() != problem.SlugNotVisible {
		t.Errorf("problem is %s, want bad-request or not-visible", prob.Slug())
	}
	if _, err := os.Stat(folder); err == nil {
		t.Error("the refused list wrote something")
	}

	// And inside one repository: a Package folder with no manifest anywhere,
	// which is the write the rule still refuses.
	_, prob = service.WriteAll(context.Background(), fs.WriteAllRequest{
		Files: []fs.WriteFileEntry{
			{Path: filepath.Join(root, "nomanifest", "a.txt"), Content: text("a\n")},
			{Path: filepath.Join(root, "nomanifest", "b.txt"), Content: text("b\n")},
		},
		Message: "Write into a folder with no manifest"})
	if prob == nil {
		t.Fatal("a list under a folder with no manifest above it was accepted")
	}
	if prob.Slug() != problem.SlugNotVisible {
		t.Errorf("problem is %s, want not-visible", prob.Slug())
	}
	if _, err := os.Stat(filepath.Join(root, "nomanifest", "a.txt")); err == nil {
		t.Error("the refused list wrote something")
	}
}

func TestWriteAllRefusesMoreFilesThanTheLimit(t *testing.T) {
	service, root := tree(t)
	folder := filepath.Join(root, "app")
	files := []fs.WriteFileEntry{{Path: filepath.Join(folder, "kitbash.yaml"), Content: text(packageManifest)}}
	for i := range fs.MaxWriteFiles {
		files = append(files, fs.WriteFileEntry{
			Path: filepath.Join(folder, fmt.Sprintf("file%03d.md", i)), Content: text("x\n")})
	}

	_, prob := service.WriteAll(context.Background(), fs.WriteAllRequest{Files: files, Message: "Write a tree"})
	if prob == nil {
		t.Fatal("a list over the file limit was accepted")
	}
	if prob.Slug() != problem.SlugTooLarge {
		t.Errorf("problem is %s, want too-large", prob.Slug())
	}
	if _, err := os.Stat(folder); err == nil {
		t.Error("the refused list wrote something")
	}
}

func TestWriteAllRefusesMoreBytesThanTheLimit(t *testing.T) {
	service, root := tree(t)
	folder := filepath.Join(root, "app")
	big := strings.Repeat("x", fs.MaxBytes)
	files := []fs.WriteFileEntry{{Path: filepath.Join(folder, "kitbash.yaml"), Content: text(packageManifest)}}
	for i := range fs.MaxWriteTotalBytes/fs.MaxBytes + 1 {
		files = append(files, fs.WriteFileEntry{
			Path: filepath.Join(folder, fmt.Sprintf("file%02d.bin", i)), Content: text(big)})
	}

	_, prob := service.WriteAll(context.Background(), fs.WriteAllRequest{Files: files, Message: "Write a big Package"})
	if prob == nil {
		t.Fatal("a list over the byte limit was accepted")
	}
	if prob.Slug() != problem.SlugTooLarge {
		t.Errorf("problem is %s, want too-large", prob.Slug())
	}
	if _, err := os.Stat(folder); err == nil {
		t.Error("the refused list wrote something")
	}
}

// expectedSha on a list is the repository's head, because a list is a change
// to the repository rather than to one file.
func TestWriteAllHoldsExpectedShaToTheRepositoryHead(t *testing.T) {
	service, root := tree(t)
	ctx := context.Background()
	folder := filepath.Join(root, "app")

	first, prob := service.WriteAll(ctx, fs.WriteAllRequest{Files: aPackage(folder), Message: "Add the app Package"})
	if prob != nil {
		t.Fatalf("WriteAll: %s", prob.Detail)
	}

	_, prob = service.WriteAll(ctx, fs.WriteAllRequest{
		Files:       []fs.WriteFileEntry{{Path: filepath.Join(folder, "server.js"), Content: text("changed\n")}},
		Message:     "Change the server",
		ExpectedSha: strings.Repeat("0", 40)})
	if prob == nil {
		t.Fatal("a stale expectedSha was accepted")
	}
	if prob.Slug() != problem.SlugConflict {
		t.Errorf("problem is %s, want conflict", prob.Slug())
	}
	if body, err := os.ReadFile(filepath.Join(folder, "server.js")); err != nil || strings.Contains(string(body), "changed") {
		t.Errorf("the refused write landed: %q %v", body, err)
	}

	if _, prob := service.WriteAll(ctx, fs.WriteAllRequest{
		Files:       []fs.WriteFileEntry{{Path: filepath.Join(folder, "server.js"), Content: text("changed\n")}},
		Message:     "Change the server",
		ExpectedSha: first.Commit.Sha}); prob != nil {
		t.Fatalf("the current head was refused: %s: %s", prob.Slug(), prob.Detail)
	}
}

// A member's list under the shared root is one queued call, not one per file:
// an admin approves a Package, and approving it writes the whole list.
func TestWriteAllUnderTheSharedRootIsOneApproval(t *testing.T) {
	service, root, q := shared(t, false)
	folder := filepath.Join(root, "app")

	_, prob := service.WriteAll(context.Background(), fs.WriteAllRequest{
		Files: aPackage(folder), Message: "Add the app Package"})
	if prob == nil {
		t.Fatal("a member's write under the shared root was not queued")
	}
	if prob.Slug() != problem.SlugQueued || prob.Status != 202 {
		t.Fatalf("problem is %s at %d, want queued at 202", prob.Slug(), prob.Status)
	}
	if len(q.queued) != 1 {
		t.Fatalf("%d approvals were queued, want one for the whole list", len(q.queued))
	}
	if q.queued[0].Tool != telemetry.ToolFSWrite {
		t.Errorf("the approval is for %s, want fs_write", q.queued[0].Tool)
	}
	var input struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(q.queued[0].Input, &input); err != nil {
		t.Fatalf("the queued input is not an fs_write input: %v", err)
	}
	if len(input.Files) != 4 || input.Message == "" {
		t.Errorf("the queued input is %+v, want the whole list and its message", input)
	}
	if _, err := os.Stat(folder); err == nil {
		t.Error("a queued list wrote something")
	}
}

// gitStatus is the porcelain status of a repository, empty when the tree and
// the index match HEAD.
func gitStatus(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", "-c", "safe.directory="+repo, "status", "--porcelain")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git status in %s: %v", repo, err)
	}
	return strings.TrimSpace(string(out))
}

// A refusal the kernel makes in the middle of the list is still a list that
// did not happen: the files written before it go back, so the repository is
// what it was and spec/mcp-surface.yaml is telling the truth.
func TestWriteAllPutsTheFolderBackWhenAWriteIsRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a folder with no write bit, so there is nothing to refuse")
	}
	service, root := tree(t)
	ctx := context.Background()
	folder := filepath.Join(root, "app")

	// A Package, and then a second write over it whose third file lands in a
	// folder the caller may not write.
	if _, prob := service.WriteAll(ctx, fs.WriteAllRequest{
		Files: aPackage(folder), Message: "Add the app Package"}); prob != nil {
		t.Fatalf("WriteAll: %s: %s", prob.Slug(), prob.Detail)
	}
	sealed := filepath.Join(folder, "sealed")
	if err := os.MkdirAll(sealed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealed, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sealed, 0o755) })
	before := gitStatus(t, folder)

	_, prob := service.WriteAll(ctx, fs.WriteAllRequest{
		Files: []fs.WriteFileEntry{
			{Path: filepath.Join(folder, "server.js"), Content: text("console.log('changed')\n")},
			{Path: filepath.Join(folder, "public", "about.html"), Content: text("<!doctype html>\n")},
			{Path: filepath.Join(sealed, "third.txt"), Content: text("refused\n")},
			{Path: filepath.Join(folder, "fourth.txt"), Content: text("fourth\n")},
			{Path: filepath.Join(folder, "fifth.txt"), Content: text("fifth\n")},
		},
		Message: "Write five files, one of them into a sealed folder"})
	if prob == nil {
		t.Fatal("a write into a folder the caller may not write was accepted")
	}
	if prob.Slug() != problem.SlugNotPermitted {
		t.Errorf("problem is %s, want not-permitted", prob.Slug())
	}

	// The two files written before the refusal are back the way they were, and
	// the ones after it were never written.
	body, err := os.ReadFile(filepath.Join(folder, "server.js"))
	if err != nil {
		t.Fatalf("reading the file the refused write replaced: %v", err)
	}
	if strings.Contains(string(body), "changed") {
		t.Errorf("server.js holds %q, want what it held before the refused write", body)
	}
	for _, name := range []string{
		filepath.Join("public", "about.html"), "fourth.txt", "fifth.txt",
		filepath.Join("sealed", "third.txt"),
	} {
		if _, err := os.Stat(filepath.Join(folder, name)); err == nil {
			t.Errorf("%s was left behind by a refused write", name)
		}
	}
	if got := gitStatus(t, folder); got != before {
		t.Errorf("the repository is %q after a refused write, want %q", got, before)
	}
}

// The same guarantee when git is what fails: a list with no commit is a list
// that did not happen, and the index does not keep it either.
func TestWriteAllPutsTheFolderBackWhenTheCommitFails(t *testing.T) {
	service, root := tree(t)
	ctx := context.Background()
	folder := filepath.Join(root, "app")

	if _, prob := service.WriteAll(ctx, fs.WriteAllRequest{
		Files: aPackage(folder), Message: "Add the app Package"}); prob != nil {
		t.Fatalf("WriteAll: %s: %s", prob.Slug(), prob.Detail)
	}
	before := gitStatus(t, folder)

	// A commit with an author that is not a member name is refused by the
	// surface, so the failure is made by taking git's own refusal: an empty
	// commit message is one git will not make a commit from.
	_, prob := service.WriteAll(ctx, fs.WriteAllRequest{
		Files: []fs.WriteFileEntry{
			{Path: filepath.Join(folder, "server.js"), Content: text("console.log('changed')\n")},
			{Path: filepath.Join(folder, "public", "about.html"), Content: text("<!doctype html>\n")},
		},
		Message: ""})
	if prob == nil {
		t.Fatal("a commit with no message was made")
	}
	body, err := os.ReadFile(filepath.Join(folder, "server.js"))
	if err != nil {
		t.Fatalf("reading the file the refused write replaced: %v", err)
	}
	if strings.Contains(string(body), "changed") {
		t.Errorf("server.js holds %q, want what it held before the refused write", body)
	}
	if _, err := os.Stat(filepath.Join(folder, "public", "about.html")); err == nil {
		t.Error("a file of the refused write was left behind")
	}
	if got := gitStatus(t, folder); got != before {
		t.Errorf("the repository is %q after a refused commit, want %q", got, before)
	}
}
