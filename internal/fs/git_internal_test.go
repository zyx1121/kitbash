package fs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every git this package starts is held to the work it was asked for. The
// flake of issue #134 is what a detached grandchild costs: the write is
// answered, the caller moves on, and a gc nobody waited for is still writing
// under .git/objects. The configuration is read off the command line rather
// than off a behaviour, because a repository small enough to test with is a
// repository git would not have packed anyway.
func TestEveryGitInvocationRefusesBackgroundMaintenance(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "docs")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dir := t.TempDir()
	recorded := filepath.Join(dir, "argv")
	fake := filepath.Join(dir, "git")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + recorded + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the fake git: %v", err)
	}
	t.Setenv("PATH", dir)

	service, err := New("tester", []string{root})
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	if _, err := service.git(context.Background(), repo, "log", "-1"); err != nil {
		t.Fatalf("git: %v", err)
	}

	body, err := os.ReadFile(recorded)
	if err != nil {
		t.Fatalf("reading the recorded command line: %v", err)
	}
	line := strings.TrimSpace(string(body))
	for _, want := range []string{"-c gc.auto=0", "-c maintenance.auto=false", "-c gc.autoDetach=false"} {
		if !strings.Contains(line, want) {
			t.Errorf("git was run as %q, want it to carry %q", line, want)
		}
	}
	if !strings.HasSuffix(line, "log -1") {
		t.Errorf("git was run as %q, want the command it was asked for at the end", line)
	}
}
