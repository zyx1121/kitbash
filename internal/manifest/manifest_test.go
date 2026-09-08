package manifest_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/manifest"
)

func TestParseValid(t *testing.T) {
	m, err := manifest.Parse([]byte(`name: ffmpeg
description: Transcode and probe media files. Use for any audio or video conversion.
tags: [media, cli]
deploy:
  units:
    - type: container
      build: .
      expose: mcp
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Name != "ffmpeg" {
		t.Errorf("name is %q", m.Name)
	}
	if len(m.Tags) != 2 {
		t.Errorf("tags are %v", m.Tags)
	}
	if !m.IsPackage() {
		t.Error("a manifest with a deploy block is a package")
	}
	if m.Raw["description"] == nil {
		t.Error("the raw document is not kept")
	}
}

func TestParseInvalid(t *testing.T) {
	cases := map[string]string{
		"no description":     "name: solo\n",
		"name not kebab":     "name: Not Kebab\ndescription: A description long enough to pass the minimum length.\n",
		"description short":  "name: solo\ndescription: short\n",
		"unknown key":        "name: solo\ndescription: A description long enough to pass the minimum length.\ncolour: blue\n",
		"deploy without any": "name: solo\ndescription: A description long enough to pass the minimum length.\ndeploy: {}\n",
		"not a mapping":      "- one\n- two\n",
		"empty":              "",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := manifest.Parse([]byte(doc))
			if err == nil {
				t.Fatal("expected an error")
			}
			var invalid *manifest.ErrInvalid
			if !errors.As(err, &invalid) {
				t.Fatalf("error is %T, want ErrInvalid", err)
			}
			if strings.TrimSpace(invalid.Error()) == "" {
				t.Error("the validator produced no message")
			}
		})
	}
}

func TestParseRefusesAHugeManifest(t *testing.T) {
	doc := "name: big\ndescription: A manifest padded well past the limit a manifest may reach.\ntags: [" +
		strings.Repeat("\"padding\", ", 8000) + "\"end\"]\n"
	if len(doc) <= manifest.MaxBytes {
		t.Fatalf("the fixture is %d bytes, not over the %d byte limit", len(doc), manifest.MaxBytes)
	}
	_, err := manifest.Parse([]byte(doc))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "over the") {
		t.Errorf("error is %q, want the size limit", err)
	}
}

func TestVisible(t *testing.T) {
	dir := t.TempDir()
	if _, ok := manifest.Visible(dir); ok {
		t.Error("a folder without a manifest is not visible")
	}
}

// kitbash follows no symlinks, and the manifest is no exception. A link would
// lend the name and the description of a manifest elsewhere to a folder that
// carries none, which is how an invisible folder would reach the surface.
func TestLoadRefusesASymlinkedManifest(t *testing.T) {
	elsewhere := t.TempDir()
	real := filepath.Join(elsewhere, manifest.FileName)
	if err := os.WriteFile(real, []byte("name: borrowed\ndescription: A manifest that lives somewhere else.\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	dir := t.TempDir()
	if err := os.Symlink(real, filepath.Join(dir, manifest.FileName)); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := manifest.Load(dir); !errors.Is(err, syscall.ELOOP) {
		t.Errorf("Load followed the link, or failed with %v, want ELOOP", err)
	}
	if m, ok := manifest.Visible(dir); ok {
		t.Errorf("a folder whose manifest is a symlink is visible as %q", m.Name)
	}
}

// The same refusal one level up: the folder itself is reached through a link.
// LoadBelow is how the fs family asks, with the root the caller may not
// resolve out of, so a folder swapped for a link mid call cannot lend a
// manifest to the surface either.
func TestLoadBelowRefusesASymlinkedFolder(t *testing.T) {
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, manifest.FileName),
		[]byte("name: borrowed\ndescription: A manifest that lives somewhere else.\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	root := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(root, "docs")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := manifest.LoadBelow(root, "docs"); !errors.Is(err, syscall.ELOOP) {
		t.Errorf("LoadBelow followed the link, or failed with %v, want ELOOP", err)
	}
	if m, ok := manifest.VisibleBelow(root, "docs"); ok {
		t.Errorf("a folder that is a symlink is visible as %q", m.Name)
	}
	// The lexical belt still holds: a path that climbs out of the root is
	// refused before anything is opened.
	if _, ok := manifest.VisibleBelow(root, "../elsewhere"); ok {
		t.Error("a folder outside the root is visible")
	}
}

// A manifest that is a FIFO must not hang the visibility check either.
func TestLoadRefusesAManifestThatIsNotARegularFile(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, manifest.FileName), 0o644); err != nil {
		t.Skipf("this filesystem has no FIFOs: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := manifest.Load(dir)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, manifest.ErrNotRegular) {
			t.Errorf("Load failed with %v, want ErrNotRegular", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Load blocked on a FIFO instead of refusing it")
	}
	if _, ok := manifest.Visible(dir); ok {
		t.Error("a folder whose manifest is a FIFO is visible")
	}
}
