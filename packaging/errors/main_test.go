package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/zyx1121/kitbash/internal/problem"
)

// Every slug internal/problem can return has a page, and no page names a slug
// it cannot return. The list is read from the source rather than exported, so
// adding a slug there fails here until it has a page.
func TestEveryProblemSlugHasAPage(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "internal", "problem", "problem.go"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`Slug[A-Za-z]+\s*=\s*"([a-z-]+)"`)
	want := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		want[m[1]] = true
	}
	if len(want) == 0 {
		t.Fatal("no slugs found in problem.go")
	}
	have := map[string]bool{}
	for _, e := range entries {
		if have[e.Slug] {
			t.Errorf("%s has two pages", e.Slug)
		}
		have[e.Slug] = true
		if !want[e.Slug] {
			t.Errorf("%s has a page but problem.go has no such slug", e.Slug)
		}
		if e.When == "" || e.Fix == "" || e.Status == "" {
			t.Errorf("%s: every page needs status, when and fix", e.Slug)
		}
	}
	for s := range want {
		if !have[s] {
			t.Errorf("problem.go returns %s and it has no page", s)
		}
	}
	if problem.Base != "https://kitbash.zyx.tw/errors/" {
		t.Errorf("problem.Base is %s; deploy.sh serves the pages under /errors/ on kitbash.zyx.tw", problem.Base)
	}
}

func TestRender(t *testing.T) {
	dir := t.TempDir()
	if err := render(filepath.Join(dir, "errors")); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(dir, "errors", e.Slug, "index.html")); err != nil {
			t.Error(err)
		}
	}
}
