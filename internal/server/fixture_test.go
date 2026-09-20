package server_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The repositories a test builds do the work a test asked for and nothing
// else. Without this a commit ends by detaching maintenance, and the flake of
// issue #134 is TempDir removing .git/objects while that child still writes
// into it: the run that failed in CI passed on the same commit beside it.
//
// The setting is read back off .git/config rather than off the command the
// fixture ran, because what matters is that it is on the repository: the
// service's own git, and any git a test starts by hand, both read it there.
func TestFixtureRepositoriesDoNoBackgroundWork(t *testing.T) {
	f := newFixture(t)
	want := map[string]string{
		"gc.auto":          "0",
		"maintenance.auto": "false",
		"gc.autodetach":    "false",
	}
	for _, repo := range []string{filepath.Join(f.org, "handbook"), filepath.Join(f.home, "notes")} {
		for key, value := range want {
			if got := gitConfigOf(t, repo, key); got != value {
				t.Errorf("%s has %s = %q, want %q", repo, key, got, value)
			}
		}
	}
}

// gitConfigOf reads one setting out of a repository's own configuration.
func gitConfigOf(t *testing.T, repo, key string) string {
	t.Helper()
	cmd := exec.Command("git", "-c", "safe.directory="+repo, "config", "--get", key)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		// git exits 1 for a setting that is not there, which is the answer.
		return ""
	}
	return strings.TrimSpace(string(out))
}
