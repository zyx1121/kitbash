package bridge_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/bridge"
	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/proc"
	"github.com/zyx1121/kitbash/internal/server"
)

// packageManifest declares one tool with a schema strict enough that the
// surface can be seen refusing bad input before the call is forwarded.
const packageManifest = `name: ffmpeg
description: Transcode and probe media files. Use for any audio or video conversion.
provides:
  tools:
    - name: transcode
      description: Convert a media file to another container or codec.
      input: { $ref: schemas/transcode.in.json }
      output: { $ref: schemas/transcode.out.json }
    - name: undeclared-by-the-server
      description: A tool the manifest declares and the server does not offer.
      input: { type: object }
      output: { type: object }
deploy:
  units:
    - type: container
      build: .
      expose: mcp
`

const inputSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["path"],
  "properties": { "path": { "type": "string" } }
}`

const outputSchema = `{
  "type": "object",
  "required": ["path", "seconds"],
  "properties": { "path": { "type": "string" }, "seconds": { "type": "integer" } }
}`

type harness struct {
	files     *fs.Service
	runner    *podman.Fake
	processes *proc.Service
	bridge    *bridge.Bridge
	server    *mcp.Server
	folder    string
	root      string
}

// newHarness wires the whole surface over one Package whose stdio server runs
// in this process instead of in a container.
func newHarness(t *testing.T, tool mcp.ToolHandler) *harness {
	t.Helper()
	root := t.TempDir()
	files, err := fs.New("tester", []string{root})
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	folder := filepath.Join(root, "ffmpeg")
	if err := os.MkdirAll(filepath.Join(folder, "schemas"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(folder, manifest.FileName), packageManifest)
	writeFile(t, filepath.Join(folder, "schemas", "transcode.in.json"), inputSchema)
	writeFile(t, filepath.Join(folder, "schemas", "transcode.out.json"), outputSchema)

	runner := podman.NewFake()
	h := &harness{files: files, runner: runner, folder: folder, root: root}
	h.processes = proc.New(files, runner, nil)
	h.bridge = bridge.New(files, h.processes, runner)
	h.server = server.New("test", server.Deps{
		Files:     files,
		Processes: h.processes,
		Bridge:    h.bridge,
	})

	// The Package: an MCP server the bridge reaches over an in memory pair
	// rather than over podman exec.
	packageServer := mcp.NewServer(&mcp.Implementation{Name: "ffmpeg", Version: "1"}, nil)
	packageServer.AddTool(&mcp.Tool{
		Name:        "transcode",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, tool)
	h.bridge.SetTransport(func(ctx context.Context, _ *proc.Process) (mcp.Transport, error) {
		clientSide, serverSide := mcp.NewInMemoryTransports()
		if _, err := packageServer.Connect(ctx, serverSide, nil); err != nil {
			return nil, err
		}
		return clientSide, nil
	})
	return h
}

// run builds and starts the Package, the way pkg_build and proc_run would.
func (h *harness) run(t *testing.T) *proc.Process {
	t.Helper()
	ctx := context.Background()
	if _, _, err := h.runner.Build(ctx, h.folder, h.folder+"/Containerfile", "localhost/kitbash/ffmpeg:test",
		map[string]string{podman.LabelPath: h.folder, podman.LabelName: "ffmpeg", podman.LabelUser: "tester"}); err != nil {
		t.Fatalf("building: %v", err)
	}
	process, prob := h.processes.Run(ctx, h.folder, "", "")
	if prob != nil {
		t.Fatalf("proc.Run: %s", prob.Detail)
	}
	if prob := h.bridge.Add(ctx, process); prob != nil {
		t.Fatalf("bridge.Add: %s", prob.Detail)
	}
	return process
}

// connect opens a client session against the kitbash surface.
func (h *harness) connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	clientSide, serverSide := mcp.NewInMemoryTransports()
	serverSession, err := h.server.Connect(ctx, serverSide, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "kitbash-test", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		session.Close()
		serverSession.Wait()
		h.bridge.Close()
	})
	return session
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// echo is a Package tool that answers with the shape its manifest promises.
func echo(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
		return nil, err
	}
	out := map[string]any{"path": in.Path, "seconds": 12}
	encoded, _ := json.Marshal(out)
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(encoded)}},
		StructuredContent: json.RawMessage(encoded),
	}, nil
}

func toolNames(t *testing.T, s *mcp.ClientSession) []string {
	t.Helper()
	tools, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func problemOf(t *testing.T, res *mcp.CallToolResult) problem.Problem {
	t.Helper()
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if len(res.Content) != 1 {
		t.Fatalf("expected one content block, got %d", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("the error content is %T, want text", res.Content[0])
	}
	var p problem.Problem
	if err := json.Unmarshal([]byte(text.Text), &p); err != nil {
		t.Fatalf("the error content is not problem details: %s", text.Text)
	}
	if !strings.HasPrefix(p.Type, problem.Base) {
		t.Fatalf("the error type is %q, want a kitbash error class", p.Type)
	}
	return p
}

func TestPublishedToolsCarryThePackageNamespace(t *testing.T) {
	h := newHarness(t, echo)
	h.run(t)
	s := h.connect(t)

	names := toolNames(t, s)
	found := map[string]bool{}
	for _, name := range names {
		found[name] = true
	}
	if !found["ffmpeg_transcode"] {
		t.Errorf("tools/list is %v, want ffmpeg_transcode", names)
	}
	// A tool the manifest declares is published even when the server does not
	// offer it; the reverse, a server tool the manifest omits, is not.
	if !found["ffmpeg_undeclared-by-the-server"] {
		t.Errorf("tools/list is %v, want every tool the manifest declares", names)
	}
}

func TestCallIsForwardedToThePackage(t *testing.T) {
	h := newHarness(t, echo)
	h.run(t)
	s := h.connect(t)

	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ffmpeg_transcode",
		Arguments: map[string]any{"path": "/org/media/clip.mov"},
	})
	if err != nil {
		t.Fatalf("calling the tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("the call failed: %+v", res.Content)
	}
	var out struct {
		Path    string `json:"path"`
		Seconds int    `json:"seconds"`
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
	if out.Path != "/org/media/clip.mov" || out.Seconds != 12 {
		t.Errorf("the Package answered %+v, want the forwarded path", out)
	}
	if len(res.Content) == 0 {
		t.Error("the Package's own content blocks were dropped")
	}
}

func TestInputIsValidatedAgainstTheManifest(t *testing.T) {
	h := newHarness(t, echo)
	h.run(t)
	s := h.connect(t)

	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ffmpeg_transcode",
		Arguments: map[string]any{"wrong": 1},
	})
	if err != nil {
		t.Fatalf("calling the tool: %v", err)
	}
	p := problemOf(t, res)
	if p.Slug() != problem.SlugBadRequest {
		t.Errorf("problem is %s, want bad-request", p.Slug())
	}
}

func TestRemoteErrorBecomesABadRequest(t *testing.T) {
	h := newHarness(t, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "ffmpeg: no such file"}},
		}, nil
	})
	h.run(t)
	s := h.connect(t)

	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ffmpeg_transcode",
		Arguments: map[string]any{"path": "/org/media/absent.mov"},
	})
	if err != nil {
		t.Fatalf("calling the tool: %v", err)
	}
	p := problemOf(t, res)
	if p.Slug() != problem.SlugBadRequest {
		t.Errorf("problem is %s, want bad-request", p.Slug())
	}
	if !strings.Contains(p.Detail, "no such file") {
		t.Errorf("detail is %q, want the Package's own message", p.Detail)
	}
	if p.Fix != "The Package refused the call; read the detail." {
		t.Errorf("fix is %q, want it to point at the Package's own message", p.Fix)
	}
}

// errorText is the raw text of an error result, before anything parses it.
func errorText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if len(res.Content) != 1 {
		t.Fatalf("expected one content block, got %d", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("the error content is %T, want text", res.Content[0])
	}
	return text.Text
}

// A kit that already speaks problem details keeps its own answer: the agent
// reads the kit's not-found as a not-found, with the kit's own fix, rather
// than a bad-request carrying JSON as a string, see issue #58.
func TestAPackageProblemPassesThroughUnchanged(t *testing.T) {
	own := problem.NotFoundFix("/org/media/absent.mov",
		"the source file is not in the workflow's inbox",
		"Import the file with the kit's own import tool first.")
	sent := own.JSON()
	h := newHarness(t, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: sent}},
		}, nil
	})
	h.run(t)
	s := h.connect(t)

	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ffmpeg_transcode",
		Arguments: map[string]any{"path": "/org/media/absent.mov"},
	})
	if err != nil {
		t.Fatalf("calling the tool: %v", err)
	}
	if got := errorText(t, res); got != sent {
		t.Errorf("the problem came back as\n%s\nwant it unchanged:\n%s", got, sent)
	}
	p := problemOf(t, res)
	if p.Slug() != problem.SlugNotFound {
		t.Errorf("problem is %s, want the kit's own not-found", p.Slug())
	}
	if p.Instance != "/org/media/absent.mov" {
		t.Errorf("instance is %q, want the kit's own", p.Instance)
	}
}

// A problem is passed through, not trusted: it is text a Package wrote, so the
// detail is clipped, the title is clipped and a fix too long to be advice is
// dropped. Otherwise a hostile Package answers every call with a megabyte.
func TestAPassedProblemIsClipped(t *testing.T) {
	own := &problem.Problem{
		Type:     problem.Base + problem.SlugTooLarge,
		Title:    strings.Repeat("t", bridge.MaxPassedTitle+50),
		Status:   413,
		Detail:   strings.Repeat("d", bridge.MaxPassedDetail*2),
		Instance: "/org/media/clip.mov",
		Fix:      strings.Repeat("f", bridge.MaxPassedFix+1),
	}
	h := newHarness(t, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: own.JSON()}},
		}, nil
	})
	h.run(t)
	s := h.connect(t)

	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ffmpeg_transcode",
		Arguments: map[string]any{"path": "/org/media/clip.mov"},
	})
	if err != nil {
		t.Fatalf("calling the tool: %v", err)
	}
	p := problemOf(t, res)
	if p.Slug() != problem.SlugTooLarge {
		t.Errorf("problem is %s, want the kit's own too-large", p.Slug())
	}
	if len(p.Detail) != bridge.MaxPassedDetail {
		t.Errorf("the detail is %d bytes, want it clipped to %d", len(p.Detail), bridge.MaxPassedDetail)
	}
	if len([]rune(p.Title)) != bridge.MaxPassedTitle {
		t.Errorf("the title is %d characters, want it clipped to %d", len([]rune(p.Title)), bridge.MaxPassedTitle)
	}
	if p.Fix != "" {
		t.Errorf("the fix is %d bytes, want a fix too long to be advice dropped", len(p.Fix))
	}
}

// refusedWith runs one call whose Package answers with this text and returns
// the error result.
func refusedWith(t *testing.T, text string) *mcp.CallToolResult {
	t.Helper()
	h := newHarness(t, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
	})
	h.run(t)
	s := h.connect(t)
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ffmpeg_transcode",
		Arguments: map[string]any{"path": "/org/media/clip.mov"},
	})
	if err != nil {
		t.Fatalf("calling the tool: %v", err)
	}
	if !res.IsError {
		t.Fatal("isError was not preserved")
	}
	return res
}

// Every field of a passed through problem is text a Package wrote, so every
// field is bounded. The instance and the type were the two that were not:
// a megabyte of instance and an invented error class of any length reached the
// agent, and the class reaches Telemetry as a span attribute besides.
func TestAPassedProblemBoundsEveryField(t *testing.T) {
	t.Run("the instance is clipped", func(t *testing.T) {
		own := &problem.Problem{
			Type: problem.Base + problem.SlugNotFound, Title: "Not found", Status: 404,
			Detail: "the source file is not in the inbox", Instance: strings.Repeat("i", 1<<20),
		}
		p := problemOf(t, refusedWith(t, own.JSON()))
		if len(p.Instance) != bridge.MaxPassedInstance {
			t.Errorf("the instance is %d bytes, want it clipped to %d", len(p.Instance), bridge.MaxPassedInstance)
		}
	})
	t.Run("an oversize type is refused", func(t *testing.T) {
		own := &problem.Problem{
			Type: problem.Base + strings.Repeat("s", 1<<16), Title: "Not found", Status: 404,
			Detail: "gone",
		}
		p := problemOf(t, refusedWith(t, own.JSON()))
		if p.Slug() != problem.SlugBadRequest {
			t.Errorf("problem is %s, want the wrapper", p.Slug())
		}
		if len(p.Slug()) > bridge.MaxPassedType {
			t.Errorf("a Package invented a %d byte error class", len(p.Slug()))
		}
	})
}

// queued is the surface's own answer to a call an admin has to approve. A
// Package returning it would tell the agent that an admin is about to run
// something that was never queued, with an approval id the agent could poll.
func TestAPackageCannotSpoofTheSurfacesOwnClasses(t *testing.T) {
	for _, c := range []struct {
		name string
		own  *problem.Problem
	}{
		{"queued", &problem.Problem{
			Type: problem.Base + problem.SlugQueued, Title: "Queued", Status: 202,
			Detail:   "the call waits for an administrator",
			Instance: "0192f0a0-0000-7000-8000-000000000000",
			Fix:      "Call approvals_list to read the result once it is approved.",
		}},
		{"not visible", &problem.Problem{
			Type: problem.Base + problem.SlugNotVisible, Title: "Not visible", Status: 404,
			Detail: "the folder carries no kitbash.yaml", Instance: "/org/media",
		}},
		{"invalid path", &problem.Problem{
			Type: problem.Base + problem.SlugInvalidPath, Title: "Invalid path", Status: 400,
			Detail: "/org/media is a symlink", Instance: "/org/media",
		}},
		{"a class with the wrong status", &problem.Problem{
			Type: problem.Base + problem.SlugNotFound, Title: "Not found", Status: 202,
			Detail: "gone", Instance: "/org/media/clip.mov",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := problemOf(t, refusedWith(t, c.own.JSON()))
			if p.Slug() != problem.SlugBadRequest {
				t.Errorf("a Package answered as %s %d, want the wrapper", p.Slug(), p.Status)
			}
		})
	}
}

// A kit's own internal failure is the kit's fault and stays a 500 internal,
// which is the class an agent reads as "not your input".
func TestAPackageInternalKeepsItsStatus(t *testing.T) {
	own := problem.Internal("/org/media/clip.mov", "the kit fell over", "Try again later.")
	p := problemOf(t, refusedWith(t, own.JSON()))
	if p.Status != 500 || p.Slug() != problem.SlugInternal {
		t.Errorf("got %d %s, want the kit's own 500 internal", p.Status, p.Slug())
	}
	if p.Detail != own.Detail {
		t.Errorf("detail is %q, want %q", p.Detail, own.Detail)
	}
}

// The bar for passing through is narrow: JSON that is not a problem of a
// kitbash class keeps the wrapper, so a Package cannot answer with any
// document it likes and have it become the surface's error.
func TestOnlyAKitbashProblemPassesThrough(t *testing.T) {
	for _, c := range []struct {
		name string
		text string
	}{
		{"another error vocabulary", `{"type":"https://example.com/errors/nope","title":"Nope","status":400,"detail":"no"}`},
		{"a document with more in it", `{"type":"` + problem.Base + `not-found","title":"Not found","status":404,"detail":"no","extra":1}`},
		{"no detail", `{"type":"` + problem.Base + `not-found","title":"Not found","status":404}`},
		{"a status that is not one", `{"type":"` + problem.Base + `not-found","title":"Not found","status":7,"detail":"no"}`},
		{"plain JSON", `{"error":"no such file"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			text := c.text
			h := newHarness(t, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: text}},
				}, nil
			})
			h.run(t)
			s := h.connect(t)

			res, err := s.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "ffmpeg_transcode",
				Arguments: map[string]any{"path": "/org/media/clip.mov"},
			})
			if err != nil {
				t.Fatalf("calling the tool: %v", err)
			}
			p := problemOf(t, res)
			if p.Slug() != problem.SlugBadRequest {
				t.Errorf("problem is %s, want the wrapper", p.Slug())
			}
			if !strings.Contains(p.Detail, strings.TrimPrefix(text, "{")[:8]) {
				t.Errorf("detail is %q, want the Package's own text", p.Detail)
			}
		})
	}
}

// Not every tool answers with a structure. A text only result has nothing to
// validate against the manifest, so it goes through as it came.
func TestTextOnlyResultPassesThrough(t *testing.T) {
	h := newHarness(t, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "the file is 12 seconds long"}},
		}, nil
	})
	h.run(t)
	s := h.connect(t)

	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ffmpeg_transcode",
		Arguments: map[string]any{"path": "/org/media/clip.mov"},
	})
	if err != nil {
		t.Fatalf("calling the tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("a text only answer was reported as an error: %+v", res.Content)
	}
	if len(res.Content) != 1 {
		t.Fatalf("the result carries %d content blocks, want the one the Package sent", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "the file is 12 seconds long" {
		t.Errorf("the content is %+v, want the Package's own text", res.Content[0])
	}
}

func TestOutputThatBreaksTheManifestIsReported(t *testing.T) {
	h := newHarness(t, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		out := map[string]any{"path": "/org/media/clip.mov", "seconds": "twelve"}
		encoded, _ := json.Marshal(out)
		return &mcp.CallToolResult{StructuredContent: json.RawMessage(encoded)}, nil
	})
	h.run(t)
	s := h.connect(t)

	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ffmpeg_transcode",
		Arguments: map[string]any{"path": "/org/media/clip.mov"},
	})
	if err != nil {
		t.Fatalf("calling the tool: %v", err)
	}
	p := problemOf(t, res)
	if p.Slug() != problem.SlugInternal {
		t.Errorf("problem is %s, want internal", p.Slug())
	}
	if p.Detail != "the Package returned output that does not match its manifest" {
		t.Errorf("detail is %q, want the manifest mismatch sentence", p.Detail)
	}
}

func TestRemoveUnpublishesTheTools(t *testing.T) {
	h := newHarness(t, echo)
	process := h.run(t)
	s := h.connect(t)

	if names := toolNames(t, s); !contains(names, "ffmpeg_transcode") {
		t.Fatalf("tools/list is %v, want ffmpeg_transcode before the stop", names)
	}
	if _, prob := h.processes.Stop(context.Background(), process.ID); prob != nil {
		t.Fatalf("Stop: %s", prob.Detail)
	}
	h.bridge.Remove(process)

	if names := toolNames(t, s); contains(names, "ffmpeg_transcode") {
		t.Errorf("tools/list is %v, want ffmpeg_transcode gone after the stop", names)
	}
}

func TestSyncPublishesRunningProcesses(t *testing.T) {
	h := newHarness(t, echo)
	ctx := context.Background()
	if _, _, err := h.runner.Build(ctx, h.folder, h.folder+"/Containerfile", "localhost/kitbash/ffmpeg:test",
		map[string]string{podman.LabelPath: h.folder, podman.LabelName: "ffmpeg", podman.LabelUser: "tester"}); err != nil {
		t.Fatalf("building: %v", err)
	}
	if _, prob := h.processes.Run(ctx, h.folder, "", ""); prob != nil {
		t.Fatalf("proc.Run: %s", prob.Detail)
	}
	// A session that starts after the Process is already running.
	h.bridge.Sync(ctx)
	s := h.connect(t)

	if names := toolNames(t, s); !contains(names, "ffmpeg_transcode") {
		t.Errorf("tools/list is %v, want the running Process's tools", names)
	}
}

func TestKitsFindsTheImportHook(t *testing.T) {
	h := newHarness(t, echo)
	ctx := context.Background()
	kit := filepath.Join(h.root, "import-mcp")
	if err := os.MkdirAll(kit, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(kit, manifest.FileName), `name: import-mcp
description: Wrap an npm MCP server as a Package the caller can build and run.
provides:
  kit: [import]
  tools:
    - name: import
      description: Wrap one npm MCP server as a Package folder.
      input:
        type: object
        required: [source]
        properties:
          source: { type: string, pattern: "^npm:" }
      output: { type: object }
deploy:
  units:
    - type: container
      build: .
      expose: mcp
`)
	if _, _, err := h.runner.Build(ctx, kit, kit+"/Containerfile", "localhost/kitbash/import-mcp:test",
		map[string]string{podman.LabelPath: kit, podman.LabelName: "import-mcp", podman.LabelUser: "tester"}); err != nil {
		t.Fatalf("building: %v", err)
	}
	if _, prob := h.processes.Run(ctx, kit, "", ""); prob != nil {
		t.Fatalf("proc.Run: %s", prob.Detail)
	}
	h.run(t)

	kits, prob := h.bridge.Kits(ctx)
	if prob != nil {
		t.Fatalf("Kits: %s", prob.Detail)
	}
	if len(kits) != 1 {
		t.Fatalf("Kits returned %d kits, want only the import kit", len(kits))
	}
	if kits[0].Process.Package != kit {
		t.Errorf("the kit is at %s, want %s", kits[0].Process.Package, kit)
	}
	if err := kits[0].Tool.ValidateInput([]byte(`{"source":"npm:time-mcp@1.0.0"}`)); err != nil {
		t.Errorf("the kit refused a source it should accept: %v", err)
	}
	if err := kits[0].Tool.ValidateInput([]byte(`{"source":"oci://alpine"}`)); err == nil {
		t.Error("the kit accepted a source its pattern excludes")
	}
}

func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
