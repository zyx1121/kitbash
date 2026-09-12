package fs_test

import (
	"testing"

	"github.com/zyx1121/kitbash/internal/fs"
)

func TestMediaType(t *testing.T) {
	cases := []struct {
		path   string
		sample string
		want   string
	}{
		{"a/README.md", "# hi", "text/markdown"},
		{"a/kitbash.yaml", "name: a", "application/yaml"},
		{"a/logo.png", "", "image/png"},
		{"a/photo.JPEG", "", "image/jpeg"},
		{"a/paper.pdf", "", "application/pdf"},
		{"a/main.go", "package main", "text/x-go"},
		{"a/noext", "plain text\n", "text/plain"},
		{"a/binary", "\x00\x01\x02\x03", "application/octet-stream"},
	}
	for _, c := range cases {
		if got := fs.MediaType(c.path, []byte(c.sample)); got != c.want {
			t.Errorf("MediaType(%q) is %q, want %q", c.path, got, c.want)
		}
	}
}

func TestTextAndImage(t *testing.T) {
	for _, mt := range []string{"text/markdown", "application/json", "application/yaml", "image/svg+xml"} {
		if !fs.IsText(mt) {
			t.Errorf("%s is text", mt)
		}
	}
	for _, mt := range []string{"image/png", "image/jpeg"} {
		if !fs.IsImage(mt) {
			t.Errorf("%s is an image", mt)
		}
		if fs.IsText(mt) {
			t.Errorf("%s is not text", mt)
		}
	}
}
