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
