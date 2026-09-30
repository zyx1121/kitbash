// Command errors renders the page behind every error type URI, so an agent
// that follows the type of a problem it was handed reads what the class means
// and what to do about it, and the landing page kitbash.zyx.tw answers / with.
// The set of slugs is internal/problem's; the test keeps the two in step. The
// pages are static HTML served by the gateway at https://kitbash.zyx.tw/, see
// deploy.sh.
//
// usage: go run ./packaging/errors <outdir>  (writes <outdir>/index.html and <outdir>/errors/)
package main

import (
	"embed"
	"fmt"
	"html"
	"html/template"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zyx1121/kitbash/internal/problem"
)

// static is copied to errors/static/ beside the pages: the Inter subset, its
// license and the favicon every zyx.tw site uses (static/README.md).
//
//go:embed static/InterVariable.woff2 static/LICENSE.txt static/favicon.ico
var static embed.FS

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

// frame is what every page shares, the frame of every zyx.tw site: the zyx mark
// top left (mirrored on hover, linking to www.zyx.tw), the nav top right,
// Privacy and Terms bottom left, the copyright bottom right, all 14 px on 20 px
// lines 20 px in from the corners, over a 64 px fade at each edge. Each corner
// item carries the tip every zyx.tw site shows (CornerTip in www.zyx.tw's
// packages/ui), saying what its label leaves out. A tip opens at once on hover
// and on keyboard focus of a link, toward the page and lined up with the item's
// outer edge, and stays open while the pointer is on it (a bridge as wide as
// the item spans the 4 px gap, beneath the link's own hit area where they
// overlap, so it never takes a click meant for a link); the script in "bottom"
// closes it on Escape or a press and keeps a touch from opening it, as Base UI
// does. Like www.zyx.tw's, it is visual only. The column, type sizes and dark
// tokens are www.zyx.tw's too, so a page an agent is sent to reads like the
// rest of zyx.tw. "top" takes which page this is ("landing", "index" or
// "page"), "bottom" the year. Links that leave zyx.tw open in a new tab with no
// referrer, as www.zyx.tw/privacy says.
var frame = template.Must(template.New("frame").Parse(`{{define "head"}}<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="icon" href="/errors/static/favicon.ico">
<style>
@font-face{font-family:Inter;src:url(/errors/static/InterVariable.woff2) format("woff2");font-weight:100 900;font-display:swap}
:root{color-scheme:dark;--background:oklch(0 0 0);--foreground:oklch(0.985 0 0);--muted:oklch(0.269 0 0);--muted-foreground:oklch(0.65 0 0);--border:oklch(1 0 0 / 10%);--ring:oklch(0.556 0 0)}
*{box-sizing:border-box;margin:0;padding:0}
html{font-family:Inter,ui-sans-serif,system-ui,sans-serif;font-feature-settings:"liga" 1,"calt" 1,"ss01" 1,"zero" 1;-webkit-font-smoothing:antialiased;-moz-osx-font-smoothing:grayscale;scrollbar-gutter:stable}
body{background:var(--background);color:var(--foreground);font-size:20px;line-height:28px}
@media (min-width:640px){body{font-size:16px;line-height:24px}}
main{margin:0 auto;width:100%;max-width:36rem;padding:120px 20px 100px}
@media (min-width:1024px){main{max-width:48rem}}
@media (min-width:1536px){main{max-width:64rem}}
h1{font-size:30px;line-height:36px;font-weight:400;text-wrap:pretty}
.sub{margin-top:12px;color:var(--muted-foreground)}
section{margin-top:60px}
section:first-of-type{margin-top:100px}
h2{font-size:24px;line-height:32px;font-weight:400}
section p{margin-top:12px;text-wrap:pretty}
section .rows{margin-top:20px}
.steps{margin-top:20px;list-style:none;counter-reset:step;display:flex;flex-direction:column;row-gap:20px}
.steps li{counter-increment:step;display:grid;grid-template-columns:28px minmax(0,1fr)}
.steps li::before{content:counter(step);color:var(--muted-foreground);font-variant-numeric:tabular-nums}
.steps li>*{grid-column:2}
.steps p{margin-top:0}
pre{margin-top:12px;padding:12px 16px;border:1px solid var(--border);border-radius:8px;overflow-x:auto;line-height:1.45}
pre code{background:none;padding:0;border-radius:0;overflow-wrap:normal}
.rows{margin-top:100px;display:grid;grid-template-columns:7rem 1fr;gap:12px 20px}
.rows dt,.muted{color:var(--muted-foreground)}
.note{margin-top:80px}
.list{margin-top:100px;list-style:none;display:flex;flex-direction:column;row-gap:12px}
.list li{display:grid;grid-template-columns:1fr}
@media (min-width:640px){.list li{grid-template-columns:13rem 1fr;column-gap:20px}}
code{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:.875em;background:var(--muted);padding:1px 4px;border-radius:4px;overflow-wrap:anywhere}
a{color:inherit;border-radius:6px}
a:focus-visible{outline:2px solid color-mix(in oklab,var(--ring) 50%,transparent)}
main a{text-decoration:underline;text-decoration-color:color-mix(in oklab,var(--muted-foreground) 40%,transparent);text-underline-offset:4px;outline-offset:2px;transition:text-decoration-color 150ms cubic-bezier(0.4,0,0.2,1)}
main a:hover{text-decoration-color:var(--foreground)}
.corner{position:fixed;z-index:50;display:flex;align-items:center;gap:16px;font-size:14px;line-height:20px}
.tl{top:20px;left:20px}.tr{top:20px;right:20px}.bl{bottom:20px;left:20px;max-width:calc(100% - 7.25rem);flex-wrap:wrap;row-gap:4px}.br{right:20px;bottom:20px}
.corner a{position:relative;text-decoration:none;outline-offset:4px}
.link{color:var(--muted-foreground);transition:color 150ms cubic-bezier(0.4,0,0.2,1)}
.link::after{content:"";position:absolute;inset:-2px -8px}
.link:hover,.link[aria-current=page]{color:var(--foreground)}
.mark svg{display:block;height:20px;width:auto;fill:currentColor}
.mark:hover svg{transform:scaleX(-1)}
@media (prefers-reduced-motion:no-preference){.mark svg{transition:transform 300ms cubic-bezier(0.4,0,0.2,1)}}
.br p{color:var(--muted-foreground);font-variant-numeric:tabular-nums}
.tipped{position:relative;display:flex}
.tl .tipped,.tr .tipped{--tip-from:-8px}.bl .tipped,.br .tipped{--tip-from:8px}
.tip,.tipped::after{position:absolute;z-index:50;pointer-events:none;background:var(--foreground);opacity:0;visibility:hidden}
.tip{width:max-content;max-width:320px;padding:6px 12px;border-radius:8px;color:var(--background);font-size:12px;line-height:16px;font-weight:400;font-variant-numeric:normal;transform:scale(.95)}
.tipped::before{content:"";position:absolute;left:0;right:0;height:4px;pointer-events:none}
.tipped::after{content:"";left:calc(50% - 5px);width:10px;height:10px;border-radius:2px;transform:rotate(45deg)}
.tl .tip,.tr .tip{top:calc(100% + 4px)}.tl .tipped::before,.tr .tipped::before{top:100%}.tl .tipped::after,.tr .tipped::after{top:calc(100% + 1px)}
.bl .tip,.br .tip{bottom:calc(100% + 4px)}.bl .tipped::before,.br .tipped::before{bottom:100%}.bl .tipped::after,.br .tipped::after{bottom:calc(100% + 1px)}
.tl .tip{left:0;transform-origin:top left}.tr .tip{right:0;transform-origin:top right}.bl .tip{left:0;transform-origin:bottom left}.br .tip{right:0;transform-origin:bottom right}
@media (hover:hover){.tipped:hover>.tip,.tipped:hover::after{opacity:1;visibility:visible;transition:visibility 0s}.tipped:hover>.tip{transform:none;pointer-events:auto}.tipped:hover::before{pointer-events:auto}}
.tipped:has(>a:focus-visible)>.tip,.tipped:has(>a:focus-visible)::after{opacity:1;visibility:visible;transition:visibility 0s}.tipped:has(>a:focus-visible)>.tip{transform:none}
.tipped.off>.tip,.tipped.off::after{opacity:0!important;visibility:hidden!important;pointer-events:none!important}.tipped.off>.tip{transform:scale(.95)!important}
@media (prefers-reduced-motion:no-preference){.tip,.tipped::after{transition:opacity 150ms ease,transform 150ms ease,visibility 0s linear 150ms}.tipped.off>.tip,.tipped.off::after{transition:opacity 150ms ease,transform 150ms ease,visibility 0s linear 150ms!important}.tipped:hover>.tip,.tipped:has(>a:focus-visible)>.tip{animation:tip-in 150ms ease}.tipped:hover::after,.tipped:has(>a:focus-visible)::after{animation:tip-arrow-in 150ms ease}}
@keyframes tip-in{from{opacity:0;transform:translateY(var(--tip-from)) scale(.95)}}
@keyframes tip-arrow-in{from{opacity:0;transform:translateY(var(--tip-from)) rotate(45deg)}}
.fade{pointer-events:none;position:fixed;left:0;right:0;z-index:40;height:64px;background:transparent;-webkit-backdrop-filter:blur(12px);backdrop-filter:blur(12px);-webkit-mask-image:linear-gradient(var(--edge-direction),#000 35%,transparent);mask-image:linear-gradient(var(--edge-direction),#000 35%,transparent)}
.fade.top{top:0;--edge-direction:to bottom}
.fade.bottom{bottom:0;--edge-direction:to top}
</style>{{end}}
{{define "top"}}<header><div class="fade top"></div>
<div class="corner tl"><span class="tipped"><a class="mark" href="https://www.zyx.tw" aria-label="zyx.tw"><svg viewBox="0 0 4096 3615" aria-hidden="true" focusable="false"><path d="M2845.95 13.9357C2917.17 -5.94859 2998.62 -8.73553 3072 33.6369C3135.18 70.1192 3172.66 128.948 3193.36 190.107C3213.74 250.29 3220.34 319.143 3218.74 390.501C3215.54 533.321 3178.64 711.473 3116.9 908.909C3087.54 1002.79 3067.38 1067.46 3055.58 1116.28C3043.27 1167.21 3044.77 1183.61 3045.32 1186.44C3049.46 1207.64 3053.85 1217.91 3057.65 1224.5C3061.46 1231.08 3068.16 1240.01 3084.44 1254.2C3086.61 1256.09 3100.05 1265.59 3150.32 1280.4C3198.49 1294.59 3264.57 1309.46 3360.54 1330.97C3562.38 1376.22 3735.1 1433.34 3860.37 1501.97C3922.96 1536.27 3979.27 1576.42 4021.19 1624.15C4063.8 1672.67 4096 1734.54 4096 1807.5C4096 1892.25 4052.87 1961.41 4000.04 2013.15C3947.46 2064.65 3876.66 2107.96 3796.99 2144.95C3637.03 2219.22 3415.77 2279.62 3157.74 2323.89C2979.46 2354.48 2848.13 2377.03 2749.36 2396.67C2649.09 2416.6 2590.56 2432.05 2554.26 2446.64C2495.41 2470.29 2468.77 2482.03 2445.24 2495.62C2421.7 2509.22 2398.22 2526.42 2348.31 2565.57C2317.52 2589.71 2274.88 2632.68 2207.49 2709.56C2141.09 2785.3 2055.9 2887.77 1940.28 3026.89C1772.92 3228.25 1609.99 3389.69 1465.7 3491.1C1393.83 3541.61 1320.93 3581.28 1250.05 3601.07C1178.83 3620.95 1097.38 3623.73 1024 3581.36C960.82 3544.88 923.345 3486.05 902.64 3424.9C882.264 3364.71 875.65 3295.86 877.25 3224.5C880.452 3081.68 917.353 2903.53 979.096 2706.09C1008.45 2612.2 1028.61 2547.53 1040.41 2498.71C1052.72 2447.78 1051.22 2431.39 1050.67 2428.56C1046.53 2407.36 1042.14 2397.08 1038.34 2390.5C1034.54 2383.91 1027.84 2374.98 1011.55 2360.79C1009.38 2358.9 995.931 2349.4 945.669 2334.59C897.5 2320.41 831.421 2305.53 735.449 2284.02C533.619 2238.78 360.909 2181.67 235.64 2113.04C173.051 2078.74 116.735 2038.59 74.8088 1990.85C32.2045 1942.34 0.0020352 1880.47 0 1807.51C0.00115737 1722.76 43.1356 1653.6 95.9632 1601.86C148.539 1550.36 219.335 1507.05 299.007 1470.06C458.965 1395.78 680.22 1335.38 938.257 1291.1C1116.53 1260.51 1247.86 1237.96 1346.63 1218.33C1446.91 1198.39 1505.44 1182.95 1541.74 1168.36C1600.59 1144.7 1627.22 1132.96 1650.76 1119.37C1674.3 1105.78 1697.78 1088.58 1747.68 1049.43C1778.47 1025.28 1821.11 982.314 1888.51 905.431C1954.9 829.699 2040.09 727.225 2155.71 588.109C2323.07 386.75 2486 225.308 2630.29 123.899C2702.17 73.388 2775.07 33.7253 2845.95 13.9357Z"/></svg></a><span class="tip" aria-hidden="true">www.zyx.tw</span></span></div>
<nav class="corner tr" aria-label="Main"><span class="tipped"><a class="link" href="/errors/"{{if eq . "index"}} aria-current="page"{{end}}>Errors</a><span class="tip" aria-hidden="true">Every error class and its fix</span></span><span class="tipped"><a class="link" href="https://github.com/zyx1121/kitbash" target="_blank" rel="noopener noreferrer">GitHub</a><span class="tip" aria-hidden="true">zyx1121/kitbash</span></span></nav>
</header>{{end}}
{{define "bottom"}}<footer><div class="fade bottom"></div>
<nav class="corner bl" aria-label="Legal"><span class="tipped"><a class="link" href="https://www.zyx.tw/privacy">Privacy</a><span class="tip" aria-hidden="true">What every zyx.tw site stores and logs</span></span><span class="tipped"><a class="link" href="https://www.zyx.tw/terms">Terms</a><span class="tip" aria-hidden="true">The rules for every zyx.tw site</span></span></nav>
<div class="corner br"><p class="tipped">© {{.}}<span class="tip" aria-hidden="true">Loki</span></p></div>
</footer>
<script>for(const t of document.querySelectorAll(".tipped")){t.addEventListener("pointerenter",e=>t.classList.toggle("off",e.pointerType==="touch"));t.addEventListener("pointerdown",()=>t.classList.add("off"));t.addEventListener("focusout",()=>t.classList.remove("off"))}addEventListener("keydown",e=>{if(e.key==="Escape")for(const t of document.querySelectorAll(".tipped:hover,.tipped:focus-within"))t.classList.add("off")})</script>{{end}}`))

var page = template.Must(template.Must(frame.Clone()).New("page").Parse(`<!doctype html>
<html lang="en">
<head>
{{template "head"}}
<title>{{.Title}}: kitbash error {{.Slug}}</title>
</head>
<body>
{{template "top" "page"}}
<main>
<h1>{{.Title}}</h1>
<p class="sub"><code>{{.Type}}</code></p>
<dl class="rows">
<dt>Status</dt><dd>{{.Status}}</dd>
<dt>When</dt><dd>{{.When}}</dd>
<dt>What to do</dt><dd>{{.Fix}}</dd>
</dl>
<p class="note muted">Every problem kitbash returns is an <a href="https://www.rfc-editor.org/rfc/rfc9457" target="_blank" rel="noopener noreferrer">RFC 9457</a> object with <code>type</code>, <code>title</code>, <code>status</code>, <code>detail</code>, an optional <code>instance</code> and a <code>fix</code> written for the call that failed. This page explains the class; the <code>detail</code> and <code>fix</code> in the object explain the instance.</p>
</main>
{{template "bottom" .Year}}
</body>
</html>
`))

var index = template.Must(template.Must(frame.Clone()).New("index").Parse(`<!doctype html>
<html lang="en">
<head>
{{template "head"}}
<title>kitbash errors</title>
</head>
<body>
{{template "top" "index"}}
<main>
<h1>kitbash errors</h1>
<p class="sub">Every error the kitbash MCP surface returns names one of these classes in its <code>type</code>. The class says what kind of refusal it was; the object's <code>detail</code> and <code>fix</code> say what to do this time.</p>
<ol class="list">
{{range .Entries}}<li><a href="/errors/{{.Slug}}/">{{.Slug}}</a><span>{{.Title}} <span class="muted">{{.Status}}</span></span></li>
{{end}}</ol>
</main>
{{template "bottom" .Year}}
</body>
</html>
`))

// block is one piece of a landing section: a paragraph, label and text rows,
// or numbered steps.
type block struct {
	P     string
	Rows  [][2]string
	Steps []step
}

type step struct{ Text, Code string }

type section struct {
	Heading string
	Blocks  []block
}

// intro is what the landing page says: in HTML at / and in Markdown at
// /index.md. Text is plain, with `backticks` for code and [text](url) for
// links, so the Markdown is the text itself.
var intro = struct {
	Title, Tagline string
	Sections       []section
}{
	Title:   "kitbash",
	Tagline: "An operating system for AI agents.",
	Sections: []section{
		{"What it is", []block{{P: "kitbash turns one machine into a shared computer for an organization's agents. Each member gets a Linux user, and their agent connects over SSH to an MCP surface that is the whole system. Nothing is built for a human at a terminal. It is open source under the MIT License."}}},
		{"What it does", []block{
			{Rows: [][2]string{
				{"Files", "Shared ones under `/org`, each member's under `/home/<member>`. Every write is a git commit."},
				{"Packages", "A folder whose `kitbash.yaml` has a `deploy` section, built from a `Dockerfile` or pinned to an image."},
				{"Processes", "Packages running as rootless containers. Web apps get their own address. MCP servers add their tools to the surface. Jobs can run on a schedule."},
				{"Telemetry", "What ran, who ran it and what it cost, in one place to query."},
				{"Secrets", "Kept out of Files and handed to the Processes that name them."},
				{"Approvals", "A member's write to `/org` waits for an admin."},
				{"Skills", "Recipes in `/org/skills` for serving models, databases and web apps."},
			}},
		}},
		{"Deploy", []block{
			{Steps: []step{
				{"Create one Proxmox VE virtual machine from the Alpine cloud image, with 4 cores, 4 GB of memory and 40 GB of disk. kitbash runs every Process as a rootless container itself, so it needs a virtual machine of its own.", ""},
				{"Download the release's apk and signing key, check them against `SHA256SUMS`, then install on the host.", "cp *.rsa.pub /etc/apk/keys/\napk add kitbashd-<version>-r0.<arch>.apk\nsh /usr/share/kitbash/install.sh"},
				{"Create the first admin on the console.", "kitbash-adduser alice '<ssh public key>' admin"},
			}},
		}},
		{"Use", []block{
			{Steps: []step{
				{"Connect an agent from the member's own machine.", "claude mcp add kitbash -- ssh alice@<host>"},
				{"The admin adds everyone else with `users_create`, from their own agent.", ""},
				{"Ask the agent for something that runs. It writes the Files, builds the Package, runs the Process and reads the Telemetry.", ""},
			}},
		}},
		{"Configure", []block{
			{Rows: [][2]string{
				{"Domain", "`KITBASH_DOMAIN` gives each web Process `https://<name>.<member>.<domain>`, with one wildcard DNS record."},
				{"TLS", "`KITBASH_TLS=acme`, the default with a domain, gets certificates from Let's Encrypt on ports 80 and 443. `gateway` leaves TLS to a proxy in front."},
				{"Public address", "`KITBASH_PUBLIC_ADDRESS` is the public address of a host behind NAT or a gateway."},
				{"Gateway", "`KITBASH_GATEWAY_ADDRESS` is the address the gateway forwards from, and the only one allowed on port 80."},
			}},
			{P: "Set them when running `install.sh`. To change one later, edit `/etc/conf.d/kitbashd` and run `install.sh` again. The [README](https://github.com/zyx1121/kitbash#install) has every step and setting, and [PLAN.md](https://github.com/zyx1121/kitbash/blob/main/PLAN.md) has the design."},
		}},
	},
}

var (
	codeSpan = regexp.MustCompile("`([^`]+)`")
	linkSpan = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
)

// inline escapes text for HTML and turns `code` into <code> and, outside
// code, [text](url) into a link.
func inline(text string) template.HTML {
	var b strings.Builder
	for {
		loc := codeSpan.FindStringSubmatchIndex(text)
		if loc == nil {
			b.WriteString(links(template.HTMLEscapeString(text)))
			return template.HTML(b.String())
		}
		b.WriteString(links(template.HTMLEscapeString(text[:loc[0]])))
		b.WriteString("<code>" + template.HTMLEscapeString(text[loc[2]:loc[3]]) + "</code>")
		text = text[loc[1]:]
	}
}

// links turns [text](url) in escaped text into links, for http and https URLs
// and paths on this host only; anything else stays text. A link that leaves
// zyx.tw opens in a new tab with no referrer.
func links(escaped string) string {
	return linkSpan.ReplaceAllStringFunc(escaped, func(m string) string {
		parts := linkSpan.FindStringSubmatch(m)
		raw := html.UnescapeString(parts[2])
		u, err := url.Parse(raw)
		switch {
		case err != nil:
			return m
		case u.Scheme == "http" || u.Scheme == "https":
			attrs := ""
			if host := u.Hostname(); host != "zyx.tw" && !strings.HasSuffix(host, ".zyx.tw") {
				attrs = ` target="_blank" rel="noopener noreferrer"`
			}
			return `<a href="` + parts[2] + `"` + attrs + `>` + parts[1] + `</a>`
		case u.Scheme == "" && u.Host == "" && strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") && !strings.HasPrefix(raw, "/\\"):
			return `<a href="` + parts[2] + `">` + parts[1] + `</a>`
		default:
			return m
		}
	})
}

var landing = template.Must(template.Must(frame.Clone()).New("landing").Funcs(template.FuncMap{"inline": inline}).Parse(`<!doctype html>
<html lang="en">
<head>
{{template "head"}}
<title>{{.Title}}</title>
<meta name="description" content="{{.Tagline}}">
<link rel="alternate" type="text/markdown" href="/index.md">
</head>
<body>
{{template "top" "landing"}}
<main>
<h1>{{.Title}}</h1>
<p class="sub">{{inline .Tagline}}</p>
{{range .Sections}}<section><h2>{{.Heading}}</h2>
{{- range .Blocks}}
{{- if .P}}<p>{{inline .P}}</p>
{{- else if .Rows}}<dl class="rows">{{range .Rows}}<dt>{{index . 0}}</dt><dd>{{inline (index . 1)}}</dd>{{end}}</dl>
{{- else}}<ol class="steps">{{range .Steps}}<li><p>{{inline .Text}}</p>{{if .Code}}<pre><code>{{.Code}}</code></pre>{{end}}</li>{{end}}</ol>
{{- end}}
{{- end}}</section>
{{end}}</main>
{{template "bottom" .Year}}
</body>
</html>
`))

// markdown is the landing page for agents, from the same intro.
func markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n%s\n", intro.Title, intro.Tagline)
	for _, s := range intro.Sections {
		fmt.Fprintf(&b, "\n## %s\n", s.Heading)
		for _, bl := range s.Blocks {
			b.WriteString("\n")
			switch {
			case bl.P != "":
				b.WriteString(bl.P + "\n")
			case len(bl.Rows) > 0:
				for _, r := range bl.Rows {
					fmt.Fprintf(&b, "- **%s**: %s\n", r[0], r[1])
				}
			default:
				for i, st := range bl.Steps {
					if i > 0 {
						b.WriteString("\n")
					}
					fmt.Fprintf(&b, "%d. %s\n", i+1, st.Text)
					if st.Code != "" {
						b.WriteString("\n   ```sh\n")
						for _, line := range strings.Split(st.Code, "\n") {
							b.WriteString("   " + line + "\n")
						}
						b.WriteString("   ```\n")
					}
				}
			}
		}
	}
	b.WriteString("\nPart of [zyx.tw](https://www.zyx.tw): [Privacy](https://www.zyx.tw/privacy), [Terms](https://www.zyx.tw/terms).\n")
	return b.String()
}

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
	if err := writeLanding(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%d pages in %s and the landing page in %s\n", len(entries)+1, out, os.Args[1])
}

func render(out string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	year := time.Now().Year()
	if err := write(filepath.Join(out, "index.html"), index, struct {
		Entries []entry
		Year    int
	}{entries, year}); err != nil {
		return err
	}
	for _, e := range entries {
		dir := filepath.Join(out, e.Slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		data := struct {
			entry
			Type string
			Year int
		}{e, problem.Base + e.Slug, year}
		if err := write(filepath.Join(dir, "index.html"), page, data); err != nil {
			return err
		}
	}
	files, err := fs.Sub(static, "static")
	if err != nil {
		return err
	}
	// CopyFS never overwrites, so a second render into the same directory
	// starts static/ afresh.
	dst := filepath.Join(out, "static")
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return os.CopyFS(dst, files)
}

// writeLanding writes the landing page into root: index.html and index.md.
func writeLanding(root string) error {
	data := struct {
		Title, Tagline string
		Sections       []section
		Year           int
	}{intro.Title, intro.Tagline, intro.Sections, time.Now().Year()}
	if err := write(filepath.Join(root, "index.html"), landing, data); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "index.md"), []byte(markdown()), 0o644)
}

// write renders t with data into the file at path.
func write(path string, t *template.Template, data any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := t.Execute(f, data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
