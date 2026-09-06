package proc_test

import (
	"context"
	"errors"
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
	// The tools a Process adds are the ones the bridge published, which this
	// service does not know and does not guess at.
	if len(process.Tools) != 0 {
		t.Errorf("tools are %v, want none from the service", process.Tools)
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

// A run the runtime refuses is not the caller's to fix: podman reports the
// same exit status for an image that is gone, a name taken since the lookup
// and an option it will not take, and the manifest's own options were checked
// before anything was removed. The runtime's words stay in the server log.
func TestRunRuntimeFailureIsInternal(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "refused the command", err: errors.New("podman run: exit status 125: Error: no such image")},
		{name: "no runtime", err: errors.New(`exec: "podman": executable file not found in $PATH`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			folder := f.pack(t, "ffmpeg", mcpManifest)
			f.build(folder, "ffmpeg")
			f.runner.RunErr = tc.err

			_, prob := f.processes.Run(context.Background(), folder, "", "")
			if prob == nil {
				t.Fatal("a failed run was reported as a success")
			}
			if prob.Slug() != problem.SlugInternal {
				t.Fatalf("problem is %s, want internal", prob.Slug())
			}
			if strings.Contains(prob.Detail, "podman") {
				t.Errorf("detail is %q, want the runtime's own words kept out of it", prob.Detail)
			}
		})
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

// A container name is readable, not unique: two folders may carry the same
// manifest name. The Package path is the identity, so the second Package is
// told to pick another name rather than taking the first one's container.
func TestTwoPackagesWithTheSameNameDoNotShareOneContainer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.pack(t, "a", mcpManifest)
	b := f.pack(t, "b", mcpManifest)
	f.build(a, "ffmpeg")
	f.build(b, "ffmpeg")

	first, prob := f.processes.Run(ctx, a, "", "")
	if prob != nil {
		t.Fatalf("Run a: %s", prob.Detail)
	}
	_, prob = f.processes.Run(ctx, b, "", "")
	if prob == nil {
		t.Fatal("the second Package took the first Package's container")
	}
	if prob.Slug() != problem.SlugConflict {
		t.Fatalf("problem is %s, want conflict", prob.Slug())
	}
	if !strings.Contains(prob.Detail, a) {
		t.Errorf("detail is %q, want it to name the Package that owns the name", prob.Detail)
	}
	if !strings.Contains(prob.Fix, "different name") {
		t.Errorf("fix is %q, want it to say to pass a different name", prob.Fix)
	}
	if len(f.runner.Removed) != 0 {
		t.Errorf("the first Package's container was removed: %v", f.runner.Removed)
	}

	list, prob := f.processes.List(ctx)
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	if len(list.Processes) != 1 || list.Processes[0].ID != first.ID {
		t.Errorf("the Process list is %+v, want only the first Process", list.Processes)
	}

	// The second Package runs perfectly well under a name of its own.
	second, prob := f.processes.Run(ctx, b, "", "other")
	if prob != nil {
		t.Fatalf("Run b under another name: %s", prob.Detail)
	}
	if second.Container == first.Container {
		t.Errorf("both Processes are in container %s", second.Container)
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

// A Process is looked up by id, so the advice when the id is missing or wrong
// is about Processes, not about folders.
func TestMissingIDPointsAtProcList(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cases := []struct {
		name string
		id   string
	}{
		{name: "empty", id: ""},
		{name: "unknown", id: "0192f000-0000-7000-8000-000000000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for tool, call := range map[string]func() *problem.Problem{
				"proc_stop": func() *problem.Problem {
					_, prob := f.processes.Stop(ctx, tc.id)
					return prob
				},
				"proc_logs": func() *problem.Problem {
					_, prob := f.processes.Logs(ctx, tc.id, 0)
					return prob
				},
			} {
				prob := call()
				if prob == nil {
					t.Fatalf("%s accepted the id %q", tool, tc.id)
				}
				if prob.Slug() != problem.SlugNotFound {
					t.Errorf("%s returned %s, want not-found", tool, prob.Slug())
				}
				if !strings.Contains(prob.Fix, "proc_list") {
					t.Errorf("%s says %q, want it to point at proc_list", tool, prob.Fix)
				}
				if strings.Contains(prob.Fix, "fs_list") {
					t.Errorf("%s says %q, which is advice about folders", tool, prob.Fix)
				}
			}
		})
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
		{name: "configured", container: podman.Container{State: "configured"}, want: proc.StateStarting},
		{name: "exited cleanly", container: podman.Container{State: "exited"}, want: proc.StateStopped},
		{name: "exited badly", container: podman.Container{State: "exited", ExitCode: 2}, want: proc.StateFailed},
		{name: "stopped", container: podman.Container{State: "stopped"}, want: proc.StateStopped},
		{name: "stopping", container: podman.Container{State: "stopping"}, want: proc.StateStopped},
		{name: "removing", container: podman.Container{State: "removing"}, want: proc.StateStopped},
		{name: "paused", container: podman.Container{State: "paused"}, want: proc.StateUnhealthy},
		// A word the runtime has never spoken is not a healthy Process.
		{name: "unknown", container: podman.Container{State: "wedged"}, want: proc.StateFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := proc.State(tc.container); got != tc.want {
				t.Errorf("state is %s, want %s", got, tc.want)
			}
		})
	}
}
