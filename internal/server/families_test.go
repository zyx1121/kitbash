package server_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/bridge"
	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/pkg"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/proc"
	"github.com/zyx1121/kitbash/internal/server"
)

// packageManifest is a Package the fixture can build and run.
const packageManifest = `name: ffmpeg
description: Transcode and probe media files. Use for any audio or video conversion.
provides:
  tools:
    - name: transcode
      description: Convert a media file to another container or codec.
      input: { type: object }
      output: { type: object }
deploy:
  units:
    - type: container
      build: .
      expose: mcp
`

// whole is the fixture for the full surface: fs, pkg, proc and the bridge over
// a fake container runtime.
type whole struct {
	files  *fs.Service
	runner *podman.Fake
	root   string
	folder string
}

func newWhole(t *testing.T) *whole {
	t.Helper()
	root := t.TempDir()
	files, err := fs.New("tester", []string{root})
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	w := &whole{files: files, runner: podman.NewFake(), root: root}
	w.folder = filepath.Join(root, "ffmpeg")
	if err := os.MkdirAll(w.folder, 0o755); err != nil {
		t.Fatal(err)
	}
	return w
}

// connectWhole serves every family over an in memory transport.
func connectWhole(t *testing.T, w *whole) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	processes := proc.New(w.files, w.runner, nil)
	tools := bridge.New(w.files, processes, w.runner)
	srv := server.New("test", server.Deps{
		Files:     w.files,
		Packages:  pkg.New(w.files, w.runner, tools),
		Processes: processes,
		Bridge:    tools,
	})
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "kitbash-test", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		session.Close()
		serverSession.Wait()
		tools.Close()
	})
	return session
}

func TestToolsListCarriesThePkgAndProcFamilies(t *testing.T) {
	w := newWhole(t)
	s := connectWhole(t, w)

	tools, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if tool.InputSchema == nil {
			t.Errorf("tool %s has no input schema", tool.Name)
		}
		if tool.Name != "fs_read" && tool.OutputSchema == nil {
			t.Errorf("tool %s has no output schema", tool.Name)
		}
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
	}
	want := []string{
		"fs_history", "fs_list", "fs_read", "fs_write",
		"pkg_build", "pkg_import", "pkg_inspect", "pkg_list",
		"proc_list", "proc_logs", "proc_run", "proc_stop",
	}
	sort.Strings(names)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("tools/list is %v, want exactly %v", names, want)
	}
}

func TestPkgBuildSchemaViolationIsProblemDetails(t *testing.T) {
	w := newWhole(t)
	s := connectWhole(t, w)

	res := call(t, s, "pkg_build", map[string]any{"path": "/org/ffmpeg", "extra": true})
	p := problemOf(t, res)
	if p.Slug() != problem.SlugBadRequest {
		t.Errorf("problem is %s, want bad-request", p.Slug())
	}
	if p.Instance != "pkg_build" {
		t.Errorf("instance is %q, want pkg_build", p.Instance)
	}
}

func TestBuildRunListAndStopOverTheSurface(t *testing.T) {
	w := newWhole(t)
	s := connectWhole(t, w)

	// Write the Package the way an agent does, then build and run it.
	ok(t, call(t, s, "fs_write", map[string]any{
		"path":    filepath.Join(w.folder, "kitbash.yaml"),
		"content": packageManifest,
		"message": "Add the ffmpeg Package",
	}), "fs_write")
	ok(t, call(t, s, "fs_write", map[string]any{
		"path":    filepath.Join(w.folder, "Containerfile"),
		"content": "FROM alpine\n",
		"message": "Add the Containerfile",
	}), "fs_write")

	res := call(t, s, "pkg_build", map[string]any{"path": w.folder})
	ok(t, res, "pkg_build")
	built := structured[pkg.BuildResult](t, res)
	if !strings.HasPrefix(built.Digest, "sha256:") {
		t.Fatalf("digest is %q, want an image ID", built.Digest)
	}

	res = call(t, s, "pkg_list", map[string]any{})
	ok(t, res, "pkg_list")
	packages := structured[pkg.ListResult](t, res)
	if len(packages.Packages) != 1 || packages.Packages[0].Digest != built.Digest {
		t.Errorf("pkg_list is %+v, want the built ffmpeg Package", packages.Packages)
	}

	res = call(t, s, "proc_run", map[string]any{"package": w.folder, "digest": built.Digest})
	ok(t, res, "proc_run")
	process := structured[proc.Process](t, res)
	if process.State != proc.StateRunning || process.Name != "ffmpeg" {
		t.Fatalf("proc_run returned %+v, want a running ffmpeg Process", process)
	}
	if len(process.Tools) != 1 || process.Tools[0] != "ffmpeg_transcode" {
		t.Errorf("tools are %v, want ffmpeg_transcode", process.Tools)
	}

	res = call(t, s, "proc_list", map[string]any{})
	ok(t, res, "proc_list")
	list := structured[proc.ListResult](t, res)
	if len(list.Processes) != 1 || list.Processes[0].ID != process.ID {
		t.Errorf("proc_list is %+v, want the one Process", list.Processes)
	}

	res = call(t, s, "proc_stop", map[string]any{"id": process.ID})
	ok(t, res, "proc_stop")
	stopped := structured[proc.StopResult](t, res)
	if stopped.State != proc.StateStopped {
		t.Errorf("proc_stop returned %+v, want state stopped", stopped)
	}

	// A stopped Process stays known, so stopping it again is a no operation.
	res = call(t, s, "proc_stop", map[string]any{"id": process.ID})
	ok(t, res, "proc_stop")

	res = call(t, s, "proc_stop", map[string]any{"id": "0192f000-0000-7000-8000-000000000000"})
	p := problemOf(t, res)
	if p.Slug() != problem.SlugNotFound {
		t.Errorf("stopping an unknown id is %s, want not-found", p.Slug())
	}
}

// twinManifest is a Package whose tools land on the same surface name as
// another Package's, which is what two folders with one manifest name do.
const twinManifest = `name: twin
description: A Package that publishes one tool, for the surface name conflict.
provides:
  tools:
    - name: go
      description: A tool that exists to occupy a name on the caller's surface.
      input: { type: object }
      output: { type: object }
deploy:
  units:
    - type: container
      build: .
      expose: mcp
`

// The second Process runs, but its tools cannot join a surface that already
// answers those names, and proc_run says so rather than quietly shadowing the
// first Package.
func TestProcRunReportsToolsThatCouldNotJoinTheSurface(t *testing.T) {
	w := newWhole(t)
	s := connectWhole(t, w)

	run := func(folder, name string) *mcp.CallToolResult {
		t.Helper()
		ok(t, call(t, s, "fs_write", map[string]any{
			"path":    filepath.Join(folder, "kitbash.yaml"),
			"content": twinManifest,
			"message": "Add a twin Package",
		}), "fs_write")
		ok(t, call(t, s, "fs_write", map[string]any{
			"path":    filepath.Join(folder, "Containerfile"),
			"content": "FROM alpine\n",
			"message": "Add the Containerfile",
		}), "fs_write")
		ok(t, call(t, s, "pkg_build", map[string]any{"path": folder}), "pkg_build")
		return call(t, s, "proc_run", map[string]any{"package": folder, "name": name})
	}

	first := run(filepath.Join(w.root, "twin-a"), "twin")
	ok(t, first, "proc_run")
	if tools := structured[proc.Process](t, first).Tools; len(tools) != 1 || tools[0] != "twin_go" {
		t.Fatalf("the first Process published %v, want twin_go", tools)
	}

	second := run(filepath.Join(w.root, "twin-b"), "other")
	p := problemOf(t, second)
	if p.Slug() != problem.SlugConflict {
		t.Errorf("problem is %s, want conflict", p.Slug())
	}
	if !strings.Contains(p.Detail, "twin_go") {
		t.Errorf("detail is %q, want it to name the tool", p.Detail)
	}
	if !strings.Contains(p.Detail, "running") {
		t.Errorf("detail is %q, want it to say the Process is running", p.Detail)
	}
	// The instance is the Process id, and it is the id proc_stop takes: the
	// caller can act on the conflict without calling proc_list first.
	ok(t, call(t, s, "proc_stop", map[string]any{"id": p.Instance}), "proc_stop")

	// Both Processes are running; only the surface is unchanged.
	res := call(t, s, "proc_list", map[string]any{})
	ok(t, res, "proc_list")
	if list := structured[proc.ListResult](t, res); len(list.Processes) != 2 {
		t.Errorf("proc_list is %+v, want both Processes", list.Processes)
	}
	names := toolNames(t, s)
	seen := 0
	for _, name := range names {
		if name == "twin_go" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("tools/list carries twin_go %d times: %v", seen, names)
	}
}

// toolNames lists the surface as it stands.
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

func TestPkgImportWithoutAKitIsNotFound(t *testing.T) {
	w := newWhole(t)
	s := connectWhole(t, w)

	res := call(t, s, "pkg_import", map[string]any{
		"path":   filepath.Join(w.root, "time"),
		"source": "npm:time-mcp@1.0.0",
	})
	p := problemOf(t, res)
	if p.Slug() != problem.SlugNotFound {
		t.Errorf("problem is %s, want not-found", p.Slug())
	}
	if !strings.Contains(p.Fix, "import kit") {
		t.Errorf("fix is %q, want it to name an import kit", p.Fix)
	}
}
