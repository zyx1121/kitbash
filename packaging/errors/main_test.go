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
// nav, Privacy and Terms, the copyright, each with its tip, and the font and
// favicon it loads.
func hasFrame(t *testing.T, name, html string) {
	t.Helper()
	for _, want := range []string{
		`<a class="mark" href="https://www.zyx.tw" aria-label="zyx.tw">`,
		`<a class="link" href="/errors/"`,
		`href="https://github.com/zyx1121/kitbash" target="_blank" rel="noopener noreferrer">GitHub</a>`,
		`href="https://www.zyx.tw/privacy">Privacy</a>`,
		`href="https://www.zyx.tw/terms">Terms</a>`,
		"© " + strconv.Itoa(time.Now().Year()),
		`<span class="tip" aria-hidden="true">www.zyx.tw</span>`,
		`<span class="tip" aria-hidden="true">Every error class and its fix</span>`,
		`<span class="tip" aria-hidden="true">zyx1121/kitbash</span>`,
		`<span class="tip" aria-hidden="true">What every zyx.tw site stores and logs</span>`,
		`<span class="tip" aria-hidden="true">The rules for every zyx.tw site</span>`,
		`<span class="tip" aria-hidden="true">Loki (詹詠翔)</span>`,
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

// The landing page at / wears the same frame, says what kitbash is, what it
// does and how to deploy, use and configure it, and says the same in index.md
// for agents.
func TestLanding(t *testing.T) {
	root := t.TempDir()
	if err := writeLanding(root); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	page, md := read("index.html"), read("index.md")
	hasFrame(t, "the landing page", page)
	if strings.Contains(page, ` aria-current="page"`) {
		t.Error("the landing page marks a nav entry as current")
	}
	if !strings.HasPrefix(md, "# kitbash\n\nAn operating system for AI agents.\n") {
		t.Error("index.md does not open with the title and the tagline")
	}
	for _, heading := range []string{"What it is", "What it does", "Deploy", "Use", "Configure"} {
		if !strings.Contains(page, "<h2>"+heading+"</h2>") || !strings.Contains(md, "## "+heading+"\n") {
			t.Errorf("the landing page lacks %q in HTML or Markdown", heading)
		}
	}
	for _, object := range []string{"Files", "Packages", "Processes", "Telemetry", "Secrets", "Approvals", "Skills"} {
		if !strings.Contains(page, "<dt>"+object+"</dt>") || !strings.Contains(md, "- **"+object+"**: ") {
			t.Errorf("the landing page does not describe %s", object)
		}
	}
	// Placeholders in the commands are escaped, not parsed as tags.
	if !strings.Contains(page, "<pre><code>claude mcp add kitbash -- ssh alice@&lt;host&gt;</code></pre>") || strings.Contains(page, "<host>") {
		t.Error("the connect command is not an escaped code block")
	}
	if !strings.Contains(md, "   ```sh\n   claude mcp add kitbash -- ssh alice@<host>\n   ```\n") {
		t.Error("index.md lacks the connect command as a fenced block")
	}
	if !strings.Contains(page, `<a href="https://github.com/zyx1121/kitbash#install" target="_blank" rel="noopener noreferrer">README</a>`) {
		t.Error("the README link does not leave zyx.tw in a new tab with no referrer")
	}
	if !strings.Contains(page, `<link rel="alternate" type="text/markdown" href="/index.md">`) {
		t.Error("the landing page does not point agents at index.md")
	}
}

// inline keeps links to zyx.tw in the same tab and sends every other one to a
// new tab with no referrer, and it escapes everything else.
func TestInline(t *testing.T) {
	for in, want := range map[string]string{
		"[a](https://www.zyx.tw/terms)": `<a href="https://www.zyx.tw/terms">a</a>`,
		"[a](https://evilzyx.tw/)":      `<a href="https://evilzyx.tw/" target="_blank" rel="noopener noreferrer">a</a>`,
		"[a](/errors/)":                 `<a href="/errors/">a</a>`,
		"`<b>` & <i>":                   "<code>&lt;b&gt;</code> &amp; &lt;i&gt;",
		"`[a](https://x.example/)`":     "<code>[a](https://x.example/)</code>",
		"[a](javascript:alert(1))":      "[a](javascript:alert(1))",
		"[a](//evil.example/)":          "[a](//evil.example/)",
		// Quotes are escaped before the link is built, so a URL cannot leave its attribute.
		`[x](https://a.example/?q="><s>)`: `<a href="https://a.example/?q=&#34;&gt;&lt;s&gt;" target="_blank" rel="noopener noreferrer">x</a>`,
	} {
		if got := string(inline(in)); got != want {
			t.Errorf("inline(%q) = %q, want %q", in, got, want)
		}
	}
}
