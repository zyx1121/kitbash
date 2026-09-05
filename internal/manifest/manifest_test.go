package manifest_test

import (
	"errors"
	"strings"
	"testing"

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

func TestVisible(t *testing.T) {
	dir := t.TempDir()
	if _, ok := manifest.Visible(dir); ok {
		t.Error("a folder without a manifest is not visible")
	}
}
