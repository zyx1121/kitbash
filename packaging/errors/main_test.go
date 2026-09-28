package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

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
	re := regexp.MustCompile(`Slug[A-Za-z]+\s*=\s*"([a-z0-9-]+)"`)
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

// The pages carry the zyx.tw frame and the files it loads: the mark linking to
// www.zyx.tw, Privacy and Terms, the copyright, the Errors nav current only on
// the index, and the font and favicon beside them. Deleting any of them from
// the templates or from static/ fails here.
func TestPagesCarryTheFrame(t *testing.T) {
	out := t.TempDir()
	if err := render(out); err != nil {
		t.Fatal(err)
	}
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(out, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	year := strconv.Itoa(time.Now().Year())
	shared := []string{
		`<a class="mark" href="https://www.zyx.tw" aria-label="zyx.tw">`,
		`href="https://www.zyx.tw/privacy">Privacy</a>`,
		`href="https://www.zyx.tw/terms">Terms</a>`,
		"© " + year,
		`src:url(/errors/static/InterVariable.woff2)`,
		`<link rel="icon" href="/errors/static/favicon.ico">`,
	}
	idx := read("index.html")
	one := read(filepath.Join(problem.SlugNotFound, "index.html"))
	for _, want := range shared {
		if !strings.Contains(idx, want) {
			t.Errorf("index.html lacks %s", want)
		}
		if !strings.Contains(one, want) {
			t.Errorf("%s/index.html lacks %s", problem.SlugNotFound, want)
		}
	}
	if !strings.Contains(idx, `href="/errors/" aria-current="page">Errors</a>`) {
		t.Error("the index does not mark Errors as the current page")
	}
	if strings.Contains(one, ` aria-current="page"`) {
		t.Error("a class page marks Errors as the current page")
	}
	if !strings.Contains(one, "<code>"+problem.Base+problem.SlugNotFound+"</code>") {
		t.Error("the class page does not show its type URI")
	}
	for _, name := range []string{"InterVariable.woff2", "LICENSE.txt", "favicon.ico"} {
		got := read(filepath.Join("static", name))
		want, err := os.ReadFile(filepath.Join("static", name))
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("static/%s is not a copy of packaging/errors/static/%s", name, name)
		}
	}
	if !strings.HasPrefix(read(filepath.Join("static", "InterVariable.woff2")), "wOF2") {
		t.Error("static/InterVariable.woff2 is not a WOFF2 file")
	}
}
