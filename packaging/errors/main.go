// Command errors renders the page behind every error type URI, so an agent
// that follows the type of a problem it was handed reads what the class means
// and what to do about it. The set of slugs is internal/problem's; the test
// keeps the two in step. The pages are static HTML served by the gateway at
// https://kitbash.zyx.tw/errors/, see deploy.sh.
//
// usage: go run ./packaging/errors <outdir>
package main

import (
	"fmt"
	"html/template"
	"os"
	"path/filepath"

	"github.com/zyx1121/kitbash/internal/problem"
)

// entry is one error class as the page explains it. Status lists every HTTP
// status the class is returned with, because a slug names a class of refusal
// and not one status (problem.NotAuthenticated, problem.TooMany).
type entry struct {
	Slug   string
	Title  string
	Status string
	When   string
	Fix    string
}

var entries = []entry{
	{problem.SlugNotFound, "Not found", "404",
		"The path, Package, Process, member or approval the call named does not exist for the caller. A folder that exists but carries no manifest is not-visible instead.",
		"Check the name with the matching list tool (fs_list, pkg_list, proc_list, users_list, approvals_list) and call again with one it returned."},
	{problem.SlugNotVisible, "Not visible", "404",
		"The folder exists but has no kitbash.yaml with a name and a description, so the surface does not show it (PLAN.md 2.5, progressive disclosure).",
		"Write a kitbash.yaml with name and description into the folder with fs_write, then call again."},
	{problem.SlugNotPermitted, "Not permitted", "403, or 401 when no identity was sent",
		"The caller may not do this: a write outside /org and the caller's home, an admin only tool (users, approvals, another member's Telemetry) called by a member, a Process calling a tool its manifest permits block does not allow, or a request on the Process receiver with a missing, unknown or revoked token.",
		"Work inside your own home folder, ask an administrator, widen the manifest's permits, or send the token kitbashd minted for this Process."},
	{problem.SlugBadRequest, "Bad request", "400",
		"The input passed the tool's schema but is still malformed: content that is not base64, a digest that is not one, a limit that is not a number.",
		"Read the detail, fix that field and call again."},
	{problem.SlugInvalidPath, "Invalid path", "400",
		"The path is outside /org and /home/<caller>, contains a .. or a dot component, or names a symlink. Files resolves every path below its root and refuses anything that would leave it (PLAN.md 2.1).",
		"Use an absolute path under /org or your home with no .., no dot components and no symlinks."},
	{problem.SlugInvalidManifest, "Invalid manifest", "422",
		"kitbash.yaml does not satisfy spec/manifest.schema.json: a missing name or description, a description over the length limit, an unknown field, a kit hook that is not one of the five, a permits block that is not a list.",
		"Read the detail for the failing field, correct the manifest with fs_write and call again."},
	{problem.SlugUnsupported, "Unsupported media type", "415",
		"fs_read cannot render this file: it is neither text nor an image type the surface returns as content.",
		"Read a text or image file, or read the file through a Package that understands its type."},
	{problem.SlugTooLarge, "Too large", "413",
		"The file, the write, the build context or the query result is over the size kitbash accepts in one call.",
		"Read or write a smaller piece, narrow the query with its filters, or build from a folder that carries less."},
	{problem.SlugConflict, "Conflict", "409, or 429 when a limit in a window was hit",
		"The call raced another change: a write on a file that changed since it was read, a Process name already running, a member that already exists. With 429, the caller asked for more of something than kitbash accepts at once, such as MCP sessions.",
		"Read the current state and call again from it; with 429, end a session or wait and call again."},
	{problem.SlugQueued, "Queued", "202",
		"Not a failure. A member's write under /org, or a pkg_import into it, was not run but put in the approval queue; the instance is the approval id and the outcome lands on it once an admin decides (PLAN.md 2.1).",
		"Ask an admin to run approvals_approve with the id, or call approvals_list to follow it."},
	{problem.SlugInternal, "Internal error", "500",
		"kitbash itself failed: the store, the container runtime, git or the daemon. The cause is not in the detail; it is in the server log and, for admins, in Telemetry as an internal cause.",
		"Call again once; if it repeats, an admin reads the cause with tel_query and the host operator reads /var/log/kitbashd.log."},
}

var page = template.Must(template.New("page").Parse(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}: kitbash error {{.Slug}}</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:3rem auto;padding:0 1rem;color:#222}
h1{font-size:1.5rem}code{background:#f3f3f3;padding:.1em .3em;border-radius:3px}
dt{font-weight:600;margin-top:1rem}dd{margin:0}a{color:#0645ad}
</style>
<p><a href="/errors/">kitbash errors</a></p>
<h1>{{.Title}}</h1>
<p><code>{{.Type}}</code></p>
<dl>
<dt>Status</dt><dd>{{.Status}}</dd>
<dt>When</dt><dd>{{.When}}</dd>
<dt>What to do</dt><dd>{{.Fix}}</dd>
</dl>
<p>Every problem kitbash returns is an <a href="https://www.rfc-editor.org/rfc/rfc9457">RFC 9457</a> object with <code>type</code>, <code>title</code>, <code>status</code>, <code>detail</code>, an optional <code>instance</code> and a <code>fix</code> written for the call that failed. This page explains the class; the <code>detail</code> and <code>fix</code> in the object explain the instance.</p>
`))

var index = template.Must(template.New("index").Parse(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>kitbash errors</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:3rem auto;padding:0 1rem;color:#222}
h1{font-size:1.5rem}code{background:#f3f3f3;padding:.1em .3em;border-radius:3px}a{color:#0645ad}
td{padding:.2rem .6rem .2rem 0;vertical-align:top}
</style>
<h1>kitbash errors</h1>
<p>Every error the kitbash MCP surface returns names one of these classes in its <code>type</code>. The class says what kind of refusal it was; the object's <code>detail</code> and <code>fix</code> say what to do this time.</p>
<table>
{{range .}}<tr><td><a href="/errors/{{.Slug}}/">{{.Slug}}</a></td><td>{{.Status}}</td><td>{{.Title}}</td></tr>
{{end}}</table>
`))

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: errors <outdir>")
		os.Exit(2)
	}
	out := filepath.Join(os.Args[1], "errors")
	if err := render(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%d pages in %s\n", len(entries)+1, out)
}

func render(out string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(out, "index.html"))
	if err != nil {
		return err
	}
	if err := index.Execute(f, entries); err != nil {
		return err
	}
	f.Close()
	for _, e := range entries {
		dir := filepath.Join(out, e.Slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		f, err := os.Create(filepath.Join(dir, "index.html"))
		if err != nil {
			return err
		}
		data := struct {
			entry
			Type string
		}{e, problem.Base + e.Slug}
		if err := page.Execute(f, data); err != nil {
			return err
		}
		f.Close()
	}
	return nil
}
