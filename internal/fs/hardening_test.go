package fs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/problem"
)

// secret is what a refused read must never return, whatever route it takes.
const secret = "the crown jewels"

// hardened builds one root holding a visible folder with one file in it, and
// the Service that serves it.
func hardened(t *testing.T) (*fs.Service, string, string) {
	t.Helper()
	root := t.TempDir()
	folder := filepath.Join(root, "docs")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(folder, "kitbash.yaml"), "name: docs\ndescription: A folder that is part of the surface.\n")
	writeFile(t, filepath.Join(folder, "note.md"), "a note\n")

	service, err := fs.New("tester", []string{root})
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	return service, root, folder
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func wantSlug(t *testing.T, prob *problem.Problem, tool, slug string) {
	t.Helper()
	if prob == nil {
		t.Fatalf("%s returned no problem, want %s", tool, slug)
	}
	if prob.Slug() != slug {
		t.Errorf("%s returned %s, want %s: %s", tool, prob.Slug(), slug, prob.Detail)
	}
}

// A NUL byte truncates a path inside the C library, so the kernel would act on
// a shorter path than the one that was validated. Every tool refuses it as an
// invalid path before it reaches the filesystem.
func TestPathWithANulByteIsRefused(t *testing.T) {
	service, _, folder := hardened(t)
	ctx := context.Background()
	path := filepath.Join(folder, "note.md\x00.png")

	_, prob := service.Read(ctx, path, fs.ReadOptions{})
	wantSlug(t, prob, "fs_read", problem.SlugInvalidPath)

	_, prob = service.List(ctx, path)
	wantSlug(t, prob, "fs_list", problem.SlugInvalidPath)

	_, prob = service.History(ctx, path, 0)
	wantSlug(t, prob, "fs_history", problem.SlugInvalidPath)

	content := "planted\n"
	_, prob = service.Write(ctx, fs.WriteRequest{Path: path, Content: &content, Message: "planted"})
	wantSlug(t, prob, "fs_write", problem.SlugInvalidPath)
	if !strings.Contains(prob.Detail, "NUL") {
		t.Errorf("the detail is %q, want it to name the NUL byte", prob.Detail)
	}

	// Nothing may have been created under the truncated name either.
	if _, err := os.Stat(filepath.Join(folder, "note.md")); err != nil {
		t.Fatalf("the fixture file went missing: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(folder, "note.md")); err == nil && string(data) != "a note\n" {
		t.Error("the truncated path reached the filesystem and overwrote the file")
	}
}

// kitbash follows no symlinks at all. A link whose target sits inside the same
// root, in the same visible folder, is refused exactly like one that escapes.
func TestSymlinkInsideTheSameRootIsRefused(t *testing.T) {
	service, root, folder := hardened(t)
	ctx := context.Background()
	writeFile(t, filepath.Join(folder, "secret.md"), secret+"\n")
	link := filepath.Join(folder, "link.md")
	if err := os.Symlink(filepath.Join(folder, "secret.md"), link); err != nil {
		t.Fatalf("creating the symlink: %v", err)
	}

	res, prob := service.Read(ctx, link, fs.ReadOptions{})
	wantSlug(t, prob, "fs_read", problem.SlugInvalidPath)
	if res != nil {
		t.Fatalf("fs_read returned content for a symlink: %q", res.Text)
	}
	if strings.Contains(prob.JSON(), secret) {
		t.Error("the problem carries the content of the link target")
	}

	_, prob = service.History(ctx, link, 0)
	wantSlug(t, prob, "fs_history", problem.SlugInvalidPath)

	content := "through a link\n"
	_, prob = service.Write(ctx, fs.WriteRequest{Path: link, Content: &content, Message: "through a link"})
	wantSlug(t, prob, "fs_write", problem.SlugInvalidPath)
	if data, err := os.ReadFile(filepath.Join(folder, "secret.md")); err != nil || string(data) != secret+"\n" {
		t.Error("the write followed the link and replaced the target")
	}

	// A folder that is a link into the same root is refused too, and it is not
	// offered as a folder of the surface.
	mirror := filepath.Join(root, "mirror")
	if err := os.Symlink(folder, mirror); err != nil {
		t.Fatalf("creating the folder symlink: %v", err)
	}
	_, prob = service.List(ctx, mirror)
	wantSlug(t, prob, "fs_list", problem.SlugInvalidPath)

	roots, prob := service.List(ctx, "")
	if prob != nil {
		t.Fatalf("listing the roots: %s", prob.Detail)
	}
	for _, f := range roots.Folders {
		if f.Path == mirror {
			t.Error("a symlinked folder inside the root was listed as part of the surface")
		}
	}
}

// git's own output names host paths and internals. It belongs in the server
// log, through problem.Internal, and never in the problem the agent reads.
func TestGitFailureOutputDoesNotReachTheAgent(t *testing.T) {
	const gitSecret = "OBJECT-STORE-9f21 /var/lib/kitbash/internal"
	service, _, folder := hardened(t)
	ctx := context.Background()
	// A .git directory makes the folder a repository as far as the service is
	// concerned, so every call runs git, which fails loudly.
	if err := os.MkdirAll(filepath.Join(folder, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	fake := filepath.Join(t.TempDir(), "git")
	writeFile(t, fake, "#!/bin/sh\necho \""+gitSecret+"\" >&2\nexit 128\n")
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Setenv("PATH", filepath.Dir(fake)+string(os.PathListSeparator)+os.Getenv("PATH"))

	note := filepath.Join(folder, "note.md")
	for _, c := range []struct {
		tool string
		call func() *problem.Problem
	}{
		{"fs_history", func() *problem.Problem {
			_, prob := service.History(ctx, note, 0)
			return prob
		}},
		{"fs_read", func() *problem.Problem {
			_, prob := service.Read(ctx, note, fs.ReadOptions{})
			return prob
		}},
		{"fs_write", func() *problem.Problem {
			content := "changed\n"
			_, prob := service.Write(ctx, fs.WriteRequest{Path: note, Content: &content, Message: "changed"})
			return prob
		}},
	} {
		prob := c.call()
		wantSlug(t, prob, c.tool, problem.SlugInternal)
		rendered := prob.JSON()
		if strings.Contains(rendered, gitSecret) || strings.Contains(rendered, "OBJECT-STORE") {
			t.Errorf("%s handed git's output to the agent: %s", c.tool, rendered)
		}
		if strings.Contains(rendered, "exit status") || strings.Contains(rendered, "git log") {
			t.Errorf("%s handed the git invocation to the agent: %s", c.tool, rendered)
		}
	}
}

// A FIFO with no writer blocks open(2) forever. A caller can plant one
// anywhere they can write, so every tool must answer instead of hanging the
// session on it.
func TestFifoIsRefusedWithoutBlocking(t *testing.T) {
	service, _, folder := hardened(t)
	ctx := context.Background()
	pipe := filepath.Join(folder, "pipe.md")
	if err := syscall.Mkfifo(pipe, 0o644); err != nil {
		t.Skipf("this filesystem has no FIFOs: %v", err)
	}

	for _, c := range []struct {
		tool string
		call func() *problem.Problem
	}{
		{"fs_read", func() *problem.Problem {
			_, prob := service.Read(ctx, pipe, fs.ReadOptions{})
			return prob
		}},
		{"fs_list", func() *problem.Problem {
			_, prob := service.List(ctx, pipe)
			return prob
		}},
		{"fs_history", func() *problem.Problem {
			_, prob := service.History(ctx, pipe, 0)
			return prob
		}},
		{"fs_write", func() *problem.Problem {
			content := "into a pipe\n"
			_, prob := service.Write(ctx, fs.WriteRequest{Path: pipe, Content: &content, Message: "into a pipe"})
			return prob
		}},
	} {
		done := make(chan *problem.Problem, 1)
		go func() { done <- c.call() }()
		select {
		case prob := <-done:
			wantSlug(t, prob, c.tool, problem.SlugInvalidPath)
		case <-time.After(10 * time.Second):
			// The goroutine is stuck in open(2) and cannot be reclaimed, so the
			// run has to stop here rather than carry on with a wedged worker.
			t.Fatalf("%s blocked on a FIFO instead of refusing it", c.tool)
		}
	}
}

// plantOutside builds the folder a swapped link points at: the same shape as
// the one on the surface, holding the secret under the same names, so
// following the link at any moment is visible in the answer.
func plantOutside(t *testing.T) string {
	t.Helper()
	planted := filepath.Join(t.TempDir(), "docs")
	if err := os.MkdirAll(planted, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(planted, "kitbash.yaml"),
		"name: docs\ndescription: A folder outside every root.\n")
	writeFile(t, filepath.Join(planted, "note.md"), secret+"\n")
	writeFile(t, filepath.Join(planted, "outside.md"), secret+"\n")
	return planted
}

// swapFolder flips folder between the real directory and a symlink to planted
// until stop is closed. It is the window issue #22 named, driven rather than
// staged: the tools below run against a path whose middle changes underneath
// them.
func swapFolder(t *testing.T, folder, real, planted string, stop <-chan struct{}) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := os.Rename(folder, real); err != nil {
				continue
			}
			if err := os.Symlink(planted, folder); err != nil {
				os.Rename(real, folder)
				continue
			}
			os.Remove(folder)
			os.Rename(real, folder)
		}
	}()
	return done
}

// A folder swapped for a link while reads run: every answer is acceptable
// except one, the content behind the link. The counts are asserted as well,
// because a race test where every call fails proves nothing.
func TestAFolderSwappedForALinkWhileReadsRunNeverLeaks(t *testing.T) {
	service, root, folder := hardened(t)
	ctx := context.Background()
	planted := plantOutside(t)
	note := filepath.Join(folder, "note.md")

	// Standing still, the read works. The counts under the swap are a race and
	// cannot be asserted; that the call answers at all is asserted here, so a
	// run where everything is refused cannot pass for a run that proved
	// something.
	if _, prob := service.Read(ctx, note, fs.ReadOptions{}); prob != nil {
		t.Fatalf("fs_read before the swap: %s", prob.Detail)
	}

	stop := make(chan struct{})
	done := swapFolder(t, folder, filepath.Join(root, "docs.real"), planted, stop)
	refused := 0
	for i := 0; i < 4000; i++ {
		res, prob := service.Read(ctx, note, fs.ReadOptions{})
		if prob != nil {
			// Not found, not visible and invalid path are all honest answers
			// while the folder is being swapped underneath the call.
			refused++
			continue
		}
		if strings.Contains(res.Text, secret) {
			close(stop)
			<-done
			t.Fatalf("fs_read followed the swapped folder and returned %q", res.Text)
		}
	}
	close(stop)
	<-done
	if refused == 0 {
		t.Error("the read race never reached the window: nothing was ever refused")
	}
}

// The same swap under fs_list. A listing describes what it found, and the
// description is where the leak was: the directory entry's own Info lstats the
// path a second time, so the size and the modification time of a file outside
// the root reached the surface while the content never did.
func TestAFolderSwappedForALinkWhileListsRunNeverLeaks(t *testing.T) {
	service, root, folder := hardened(t)
	ctx := context.Background()
	planted := plantOutside(t)
	inside, err := os.Stat(filepath.Join(folder, "note.md"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if _, prob := service.List(ctx, folder); prob != nil {
		t.Fatalf("fs_list before the swap: %s", prob.Detail)
	}

	stop := make(chan struct{})
	done := swapFolder(t, folder, filepath.Join(root, "docs.real"), planted, stop)
	refused := 0
	fail := func(format string, args ...any) {
		close(stop)
		<-done
		t.Fatalf(format, args...)
	}
	for i := 0; i < 4000; i++ {
		res, prob := service.List(ctx, folder)
		if prob != nil {
			refused++
			continue
		}
		for _, f := range res.Files {
			if f.Name == "outside.md" {
				fail("fs_list listed a file from outside the root: %+v", f)
			}
			if f.Name == "note.md" && f.Size != inside.Size() {
				fail("fs_list described a file outside the root: %+v, the file inside is %d bytes",
					f, inside.Size())
			}
		}
		if res.Manifest != nil {
			if d, _ := res.Manifest["description"].(string); strings.Contains(d, "outside every root") {
				fail("fs_list returned the manifest from outside the root: %v", res.Manifest)
			}
		}
	}
	close(stop)
	<-done
	if refused == 0 {
		t.Error("the list race never reached the window: nothing was ever refused")
	}
}

// A whole fs_write while a folder inside the repository is swapped for a link
// out of the root: no byte may land outside, whichever moment the write
// arrives in.
func TestAFolderSwappedForALinkWhileWritesRunLandsNothingOutside(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "proj")
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(repo, "kitbash.yaml"), "name: proj\ndescription: A folder on the surface.\n")
	writeFile(t, filepath.Join(sub, "kitbash.yaml"), "name: sub\ndescription: A folder inside it.\n")
	writeFile(t, filepath.Join(sub, "note.md"), "a note\n")
	service, err := fs.New("tester", []string{root})
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	ctx := context.Background()

	planted := filepath.Join(t.TempDir(), "sub")
	if err := os.MkdirAll(planted, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(planted, "kitbash.yaml"), "name: sub\ndescription: A folder outside every root.\n")

	first := "a first write\n"
	if _, prob := service.Write(ctx, fs.WriteRequest{
		Path: filepath.Join(sub, "note.md"), Content: &first, Message: "write"}); prob != nil {
		t.Fatalf("fs_write before the swap: %s", prob.Detail)
	}

	stop := make(chan struct{})
	done := swapFolder(t, sub, filepath.Join(repo, "sub.real"), planted, stop)
	refused := 0
	for i := 0; i < 400; i++ {
		content := "planted\n"
		_, prob := service.Write(ctx, fs.WriteRequest{
			Path: filepath.Join(sub, "note.md"), Content: &content, Message: "write"})
		if prob != nil {
			refused++
		}
	}
	close(stop)
	<-done
	assertOnlyTheManifest(t, planted)
	if refused == 0 {
		t.Error("the write race never reached the window: nothing was ever refused")
	}
}

// The same swap where the folder being replaced is the git repository itself.
// This is the boundary of what the surface promises, so it is asserted rather
// than assumed: kitbash's own write never lands outside the root, and git,
// which asks the kernel for its working directory as a path and works by that
// path afterwards, may still put a repository of its own out there. That is
// the qualification in the header of spec/mcp-surface.yaml, and it is bounded
// by git running as the caller.
func TestARepositorySwappedForALinkWhileWritesRunLandsNoContentOutside(t *testing.T) {
	service, root, folder := hardened(t)
	ctx := context.Background()
	planted := filepath.Join(t.TempDir(), "docs")
	if err := os.MkdirAll(planted, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(planted, "kitbash.yaml"), "name: docs\ndescription: A folder outside every root.\n")

	stop := make(chan struct{})
	done := swapFolder(t, folder, filepath.Join(root, "docs.real"), planted, stop)
	for i := 0; i < 400; i++ {
		content := "planted\n"
		service.Write(ctx, fs.WriteRequest{
			Path: filepath.Join(folder, "note.md"), Content: &content, Message: "write"})
	}
	close(stop)
	<-done

	entries, err := os.ReadDir(planted)
	if err != nil {
		t.Fatalf("read the planted folder: %v", err)
	}
	for _, e := range entries {
		switch e.Name() {
		case manifest.FileName:
		case ".git":
			// git's own path resolution, which the header of the spec says
			// kitbash does not stand behind. It must be git and nothing else.
			t.Logf("git initialised a repository outside the root, as documented: %s",
				filepath.Join(planted, e.Name()))
		default:
			t.Errorf("a write left %q outside the root", filepath.Join(planted, e.Name()))
		}
	}
}

// assertOnlyTheManifest reports anything that appeared in the planted folder,
// which is anything a write let out of the root: a file, or a .git a
// repository was initialised into.
func assertOnlyTheManifest(t *testing.T, planted string) {
	t.Helper()
	entries, err := os.ReadDir(planted)
	if err != nil {
		t.Fatalf("read the planted folder: %v", err)
	}
	for _, e := range entries {
		if e.Name() != manifest.FileName {
			t.Errorf("a write left %q outside the root", filepath.Join(planted, e.Name()))
		}
	}
}

// A folder whose kitbash.yaml is a symlink is not visible. Following it would
// lend the name and the description of a manifest the caller never named to a
// folder that carries none, and put that folder's files on the surface.
func TestSymlinkedManifestLeavesTheFolderInvisible(t *testing.T) {
	service, root, _ := hardened(t)
	ctx := context.Background()

	// A real manifest outside every root, and a folder inside the root that
	// carries nothing but a link to it.
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "kitbash.yaml"), "name: borrowed\ndescription: A manifest that lives outside every root.\n")
	borrowed := filepath.Join(root, "borrower")
	if err := os.MkdirAll(borrowed, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(borrowed, "secret.md"), secret+"\n")
	if err := os.Symlink(filepath.Join(outside, "kitbash.yaml"), filepath.Join(borrowed, "kitbash.yaml")); err != nil {
		t.Fatalf("creating the manifest symlink: %v", err)
	}

	roots, prob := service.List(ctx, "")
	if prob != nil {
		t.Fatalf("listing the roots: %s", prob.Detail)
	}
	for _, f := range roots.Folders {
		if f.Path == borrowed {
			t.Errorf("a folder whose manifest is a symlink was listed as %q", f.Name)
		}
	}

	_, prob = service.List(ctx, borrowed)
	wantSlug(t, prob, "fs_list", problem.SlugNotVisible)

	res, prob := service.Read(ctx, filepath.Join(borrowed, "secret.md"), fs.ReadOptions{})
	wantSlug(t, prob, "fs_read", problem.SlugNotVisible)
	if res != nil {
		t.Fatalf("fs_read returned content from an invisible folder: %q", res.Text)
	}
}
