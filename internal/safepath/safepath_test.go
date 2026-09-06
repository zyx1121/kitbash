package safepath_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/safepath"
)

func TestInsideAcceptsPathsInTheFolder(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		rel  string
		want string
	}{
		{name: "the folder itself", rel: ".", want: dir},
		{name: "a file", rel: "kitbash.yaml", want: filepath.Join(dir, "kitbash.yaml")},
		{name: "a file below", rel: "src/main.go", want: filepath.Join(dir, "src", "main.go")},
		{name: "a folder that does not exist yet", rel: "new/deep/file.txt",
			want: filepath.Join(dir, "new", "deep", "file.txt")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := safepath.Inside(dir, tc.rel)
			if err != nil {
				t.Fatalf("Inside(%q): %v", tc.rel, err)
			}
			if got != tc.want {
				t.Errorf("Inside(%q) is %q, want %q", tc.rel, got, tc.want)
			}
		})
	}
}

func TestInsideRefusesPathsOutOfTheFolder(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		rel  string
		want string
	}{
		{name: "empty", rel: "", want: "not a relative path"},
		{name: "absolute", rel: "/etc/passwd", want: "not a relative path"},
		{name: "parent", rel: "../secret", want: "leaves this folder"},
		{name: "parent below", rel: "src/../../secret", want: "leaves this folder"},
		{name: "dot component", rel: ".git/config", want: "reserved"},
		{name: "dot component below", rel: "src/.env", want: "reserved"},
		{name: "empty component", rel: "src//main.go", want: "empty path component"},
		{name: "inner dot", rel: "src/./main.go", want: "empty path component"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := safepath.Inside(dir, tc.rel); err == nil {
				t.Fatalf("Inside(%q) was accepted", tc.rel)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Inside(%q) says %q, want it to mention %q", tc.rel, err, tc.want)
			}
		})
	}
}

// A symlink is the way out of a folder that a lexical check does not see.
func TestInsideRefusesASymlinkedComponent(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		t.Skipf("this filesystem does not do symlinks: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "secret.txt")); err != nil {
		t.Skipf("this filesystem does not do symlinks: %v", err)
	}

	for _, rel := range []string{"out", "out/secret.txt", "secret.txt"} {
		if _, err := safepath.Inside(dir, rel); err == nil {
			t.Errorf("Inside(%q) followed a symlink", rel)
		} else if !strings.Contains(err.Error(), "symlink") {
			t.Errorf("Inside(%q) says %q, want it to name the symlink", rel, err)
		}
	}
}
