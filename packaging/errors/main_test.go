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
	// Twice: rendering over an earlier render must work too.
	for range 2 {
		if err := render(filepath.Join(dir, "errors")); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(dir, "errors", e.Slug, "index.html")); err != nil {
			t.Error(err)
		}
	}
}

// hasFrame checks what every page must carry: the mark linking to www.zyx.tw, the
// nav, Privacy and Terms, the copyright, and the font and favicon it loads.
func hasFrame(t *testing.T, name, html string) {
	t.Helper()
	for _, want := range []string{
		`<a class="mark" href="https://www.zyx.tw" aria-label="zyx.tw">`,
		`<a class="link" href="/errors/"`,
		`href="https://github.com/zyx1121/kitbash" target="_blank" rel="noopener noreferrer">GitHub</a>`,
		`href="https://www.zyx.tw/privacy">Privacy</a>`,
		`href="https://www.zyx.tw/terms">Terms</a>`,
		"© " + strconv.Itoa(time.Now().Year()),
		`src:url(/errors/static/InterVariable.woff2)`,
		`<link rel="icon" href="/errors/static/favicon.ico">`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("%s lacks %s", name, want)
		}
	}
}

// The pages carry the zyx.tw frame and the files it loads, with the Errors nav
// current only on the index. Deleting any of it from the templates or from
// static/ fails here.
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
	idx := read("index.html")
	one := read(filepath.Join(problem.SlugNotFound, "index.html"))
	hasFrame(t, "index.html", idx)
	hasFrame(t, problem.SlugNotFound+"/index.html", one)
	if !strings.Contains(one, `href="https://www.rfc-editor.org/rfc/rfc9457" target="_blank" rel="noopener noreferrer"`) {
		t.Error("the RFC 9457 link leaves zyx.tw without a new tab and no referrer")
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

// The landing page at / wears the same frame, names kitbash, and marks no nav
// entry as current.
func TestLanding(t *testing.T) {
	var b strings.Builder
	if err := landing.Execute(&b, time.Now().Year()); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	hasFrame(t, "the landing page", html)
	if !strings.Contains(html, "<h1>kitbash</h1>") || !strings.Contains(html, "An operating system for AI agents.") {
		t.Error("the landing page does not say what kitbash is")
	}
	if strings.Contains(html, ` aria-current="page"`) {
		t.Error("the landing page marks a nav entry as current")
	}
}
