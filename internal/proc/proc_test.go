package proc_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/proc"
)

const mcpManifest = `name: ffmpeg
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
      env: { LOG_LEVEL: debug }
      limits: { cpu: "1", memory: "512Mi" }
      restart: never
`

const httpManifest = `name: dashboard
description: A small web interface a member opens in a browser on this host.
deploy:
  units:
    - type: container
      build: .
      expose: http
      port: 8080
`

type fixture struct {
	files     *fs.Service
	runner    *podman.Fake
	processes *proc.Service
	root      string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	files, err := fs.New("tester", []string{root})
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	runner := podman.NewFake()
	return &fixture{files: files, runner: runner, processes: proc.New(files, runner), root: root}
}

// pack writes a Package folder and returns its path.
func (f *fixture) pack(t *testing.T, name, yaml string) string {
	t.Helper()
	folder := filepath.Join(f.root, name)
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, manifest.FileName), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return folder
}

// build adds one image of a Package to the runtime, as pkg_build would.
func (f *fixture) build(folder, name string) string {
	id, _, _ := f.runner.Build(context.Background(), folder, folder+"/Containerfile",
		"localhost/kitbash/"+name+":abcdef123456", map[string]string{
			podman.LabelPath:   folder,
			podman.LabelName:   name,
			podman.LabelCommit: strings.Repeat("a", 40),
			podman.LabelUser:   "tester",
		})
	return id
}

func TestRunLabelsAndNamesTheContainer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	digest := f.build(folder, "ffmpeg")

	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if process.Name != "ffmpeg" || process.Package != folder || process.Digest != digest {
		t.Errorf("process is %+v, want the ffmpeg Package at its newest digest", process)
	}
	if process.State != proc.StateRunning {
		t.Errorf("state is %s, want running", process.State)
	}
	if process.ID == "" || len(process.ID) != 36 {
		t.Errorf("id is %q, want a UUID", process.ID)
	}
	if process.Container != "kitbash-ffmpeg-ffmpeg" {
		t.Errorf("container is %q, want kitbash-ffmpeg-ffmpeg", process.Container)
	}
	if len(process.Tools) != 1 || process.Tools[0] != "ffmpeg_transcode" {
		t.Errorf("tools are %v, want ffmpeg_transcode", process.Tools)
	}

	if len(f.runner.Runs) != 1 {
		t.Fatalf("the runtime was asked for %d runs, want 1", len(f.runner.Runs))
	}
	run := f.runner.Runs[0]
	if !run.Detach || !run.Interactive {
		t.Error("the container must run detached with stdin held open, which is what keeps PID 1 alive")
	}
	if run.Restart != "no" {
		t.Errorf("restart is %q, want the runtime spelling of never", run.Restart)
	}
	if run.CPUs != "1" || run.Memory != "512m" {
		t.Errorf("limits are %q and %q, want 1 and 512m", run.CPUs, run.Memory)
	}
	if run.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("env is %v, want LOG_LEVEL debug", run.Env)
	}
	if len(run.Publish) != 0 {
		t.Errorf("an mcp Process published %v, want no port", run.Publish)
	}
	want := map[string]string{
		podman.LabelUser:    "tester",
		podman.LabelPackage: folder,
		podman.LabelName:    "ffmpeg",
		podman.LabelDigest:  digest,
		podman.LabelExpose:  manifest.ExposeMCP,
	}
	for k, v := range want {
		if run.Labels[k] != v {
			t.Errorf("label %s is %q, want %q", k, run.Labels[k], v)
		}
	}
	if run.Labels[podman.LabelID] != process.ID {
		t.Errorf("the id label is %q, want the Process id %q", run.Labels[podman.LabelID], process.ID)
	}
}

// The manifest writes memory the Kubernetes way and the container runtime
// reads it its own way, so the suffix is converted on the way through.
func TestRunConvertsTheMemorySuffix(t *testing.T) {
	cases := []struct {
		manifest string
		want     string
	}{
		{manifest: "512Mi", want: "512m"},
		{manifest: "256Ki", want: "256k"},
		{manifest: "2Gi", want: "2g"},
	}
	for _, tc := range cases {
		t.Run(tc.manifest, func(t *testing.T) {
			f := newFixture(t)
			folder := f.pack(t, "ffmpeg", `name: ffmpeg
description: Transcode and probe media files. Use for any audio or video conversion.
deploy:
  units:
    - type: container
      build: .
      limits: { memory: "`+tc.manifest+`" }
`)
			f.build(folder, "ffmpeg")
			if _, prob := f.processes.Run(context.Background(), folder, "", ""); prob != nil {
				t.Fatalf("Run: %s", prob.Detail)
			}
			if got := f.runner.Runs[0].Memory; got != tc.want {
				t.Errorf("the runtime was asked for %q, want %q", got, tc.want)
			}
		})
	}
}

// Replacing a Process destroys the running container, so nothing is removed
// until the run that would take its place is known to be startable.
func TestRunKeepsTheOldContainerWhenTheNewOneCannotStart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")
	first, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}

	absent := "sha256:" + strings.Repeat("b", 64)
	if _, prob := f.processes.Run(ctx, folder, absent, ""); prob == nil {
		t.Fatal("a Process was run from a digest that was never built")
	}
	if len(f.runner.Removed) != 0 {
		t.Errorf("the running Process was removed: %v", f.runner.Removed)
	}

	list, prob := f.processes.List(ctx)
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	if len(list.Processes) != 1 || list.Processes[0].ID != first.ID {
		t.Errorf("the Process list is %+v, want the first Process still there", list.Processes)
	}
}

// Exit 125 is the runtime refusing the command itself, and every option in it
// came from the manifest.
func TestRunUsageErrorNamesTheManifestOptions(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")
	f.runner.RunErr = fmt.Errorf("podman run: exit status 125: %w", podman.ErrUsage)

	_, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob == nil {
		t.Fatal("a refused run was reported as a success")
	}
	if prob.Slug() != problem.SlugInvalidManifest {
		t.Fatalf("problem is %s, want invalid-manifest", prob.Slug())
	}
	for _, want := range []string{"limits.memory 512Mi", "limits.cpu 1", "restart never", "env LOG_LEVEL"} {
		if !strings.Contains(prob.Detail, want) {
			t.Errorf("detail is %q, want it to name %q", prob.Detail, want)
		}
	}
	if strings.Contains(prob.Detail, "debug") {
		t.Errorf("detail is %q, want env values kept out of it", prob.Detail)
	}
	if strings.Contains(prob.Detail, folder) || strings.Contains(prob.Detail, "podman") {
		t.Errorf("detail is %q, want host paths and the argv kept out of it", prob.Detail)
	}
	if prob.Fix != "Change the deploy unit in kitbash.yaml, then run again." {
		t.Errorf("fix is %q, want the deploy unit advice", prob.Fix)
	}
}

// Anything else from the runtime is not the caller's to fix.
func TestRunOtherRuntimeFailureIsInternal(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")
	f.runner.RunErr = errors.New(`exec: "podman": executable file not found in $PATH`)

	_, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob == nil {
		t.Fatal("a failed run was reported as a success")
	}
	if prob.Slug() != problem.SlugInternal {
		t.Errorf("problem is %s, want internal", prob.Slug())
	}
}

func TestRunIsIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")

	first, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	second, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run again: %s", prob.Detail)
	}
	if first.ID != second.ID {
		t.Errorf("the second run returned id %s, want the running Process %s", second.ID, first.ID)
	}
	if len(f.runner.Runs) != 1 {
		t.Errorf("the runtime was asked for %d runs, want 1", len(f.runner.Runs))
	}
	if len(f.runner.Removed) != 0 {
		t.Errorf("the running Process was removed: %v", f.runner.Removed)
	}
	if second.Replaced != nil {
		t.Error("an idempotent run reported that it replaced something")
	}
}

func TestRunReplacesOnANewDigest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")

	first, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	newer := f.build(folder, "ffmpeg")

	second, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run again: %s", prob.Detail)
	}
	if second.Digest != newer {
		t.Errorf("digest is %s, want the newest build %s", second.Digest, newer)
	}
	if second.ID == first.ID {
		t.Error("the replacement carries the old Process id")
	}
	if second.Replaced == nil || second.Replaced.ID != first.ID {
		t.Errorf("replaced is %+v, want the first Process", second.Replaced)
	}
	if len(f.runner.Removed) != 1 || f.runner.Removed[0] != "kitbash-ffmpeg-ffmpeg" {
		t.Errorf("removed is %v, want the old container", f.runner.Removed)
	}
}

func TestRunRefusesAReservedPackageName(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "proc", `name: proc
description: A Package whose name collides with a built in tool family.
deploy:
  units:
    - type: container
      build: .
      expose: mcp
`)
	f.build(folder, "proc")

	_, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob == nil {
		t.Fatal("a Package named after a built in family was run")
	}
	if prob.Slug() != problem.SlugInvalidManifest {
		t.Errorf("problem is %s, want invalid-manifest", prob.Slug())
	}
	if len(f.runner.Runs) != 0 {
		t.Error("the runtime was asked to run a reserved name")
	}
}

func TestRunWithoutABuildSaysToBuildFirst(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "ffmpeg", mcpManifest)

	_, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob == nil {
		t.Fatal("an unbuilt Package was run")
	}
	if prob.Slug() != problem.SlugNotFound {
		t.Errorf("problem is %s, want not-found", prob.Slug())
	}
	if !strings.Contains(prob.Fix, "pkg_build") {
		t.Errorf("fix is %q, want it to name pkg_build", prob.Fix)
	}
}

func TestRunPublishesAnEndpointForHTTP(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "dashboard", httpManifest)
	f.build(folder, "dashboard")

	process, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if got := f.runner.Runs[0].Publish; len(got) != 1 || got[0] != 8080 {
		t.Errorf("published %v, want the container port 8080", got)
	}
	want := "http://127.0.0.1:" + f.runner.HostPort("kitbash-dashboard-dashboard")
	if process.Endpoint != want {
		t.Errorf("endpoint is %q, want %q", process.Endpoint, want)
	}
	if len(process.Tools) != 0 {
		t.Errorf("an http Process published tools: %v", process.Tools)
	}
}

func TestListReturnsRunningAndStopped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")
	if _, prob := f.processes.Run(ctx, folder, "", "one"); prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	second, prob := f.processes.Run(ctx, folder, "", "two")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	// Another member's Process is not the caller's business.
	f.runner.AddContainer(podman.Container{
		Name:   "kitbash-ffmpeg-theirs",
		State:  podman.StateRunning,
		Labels: map[string]string{podman.LabelUser: "someone-else", podman.LabelID: "theirs"},
	})
	if _, prob := f.processes.Stop(ctx, second.ID); prob != nil {
		t.Fatalf("Stop: %s", prob.Detail)
	}

	out, prob := f.processes.List(ctx)
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	if len(out.Processes) != 2 {
		t.Fatalf("List returned %+v, want the caller's two Processes", out.Processes)
	}
	states := map[string]string{}
	for _, process := range out.Processes {
		states[process.Name] = process.State
	}
	if states["one"] != proc.StateRunning {
		t.Errorf("one is %s, want running", states["one"])
	}
	if states["two"] != proc.StateStopped {
		t.Errorf("two is %s, want stopped", states["two"])
	}
}

func TestStopUnknownID(t *testing.T) {
	f := newFixture(t)
	_, prob := f.processes.Stop(context.Background(), "0192f000-0000-7000-8000-000000000000")
	if prob == nil {
		t.Fatal("stopping an unknown id succeeded")
	}
	if prob.Slug() != problem.SlugNotFound {
		t.Errorf("problem is %s, want not-found", prob.Slug())
	}
}

func TestLogsCapTheLineCount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")
	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	f.runner.LogLines[process.Container] = []string{"first", "second", "third"}

	out, prob := f.processes.Logs(ctx, process.ID, 2)
	if prob != nil {
		t.Fatalf("Logs: %s", prob.Detail)
	}
	if len(out.Lines) != 2 || out.Lines[0] != "second" {
		t.Errorf("lines are %v, want the last two", out.Lines)
	}
}

func TestStateMapping(t *testing.T) {
	cases := []struct {
		name      string
		container podman.Container
		want      string
	}{
		{name: "running", container: podman.Container{State: "running"}, want: proc.StateRunning},
		{name: "created", container: podman.Container{State: "created"}, want: proc.StateStarting},
		{name: "initialized", container: podman.Container{State: "initialized"}, want: proc.StateStarting},
		{name: "exited cleanly", container: podman.Container{State: "exited"}, want: proc.StateStopped},
		{name: "exited badly", container: podman.Container{State: "exited", ExitCode: 2}, want: proc.StateFailed},
		{name: "paused", container: podman.Container{State: "paused"}, want: proc.StateUnhealthy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := proc.State(tc.container); got != tc.want {
				t.Errorf("state is %s, want %s", got, tc.want)
			}
		})
	}
}
