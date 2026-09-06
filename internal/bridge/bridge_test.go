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
	h.processes = proc.New(files, runner)
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
