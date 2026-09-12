package proc_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/proc"
	"github.com/zyx1121/kitbash/internal/telemetry/teltest"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// runnerManifest is a Package whose container unit names the kit that runs it.
// Nothing of it is ever started on this host: the kit answers with the Process
// and kitbashd registers what it answered.
const runnerManifest = `name: trainer
description: A job that runs on a machine with a GPU, started by the runner this manifest names.
deploy:
  units:
    - type: container
      build: .
      runner: /org/pve-runner
      expose: none
      limits: { memory: "512Mi" }
`

// runnerPath is the Package folder of the kit runnerManifest names.
const runnerPath = "/org/pve-runner"

// stubKits stands in for the MCP bridge: the kit a manifest named, and
// whatever that kit answers when a hook calls it.
type stubKits struct {
	at     map[string]proc.Kit
	result map[string]string
	// errs is what one tool of the kit refuses with, by tool name, which is a
	// kit that will not do what it is asked.
	errs  map[string]*problem.Problem
	calls []stubCall
}

// stubCall is one tool call a hook made on a kit.
type stubCall struct {
	Tool string
	Args map[string]any
}

func (s *stubKits) KitAt(_ context.Context, path, hook, _ string) (*proc.Kit, *problem.Problem) {
	if kit, running := s.at[path]; running {
		return &kit, nil
	}
	return nil, problem.NotFoundFix(path,
		"the manifest names the "+hook+" kit at "+path+", and no Process of it is running",
		"Run the kit first with proc_run on "+path+", then call this tool again.")
}

func (s *stubKits) CallTool(_ context.Context, _ *proc.Process, tool string, args map[string]any) (json.RawMessage, *problem.Problem) {
	s.calls = append(s.calls, stubCall{Tool: tool, Args: args})
	if prob, refused := s.errs[tool]; refused {
		return nil, prob
	}
	return json.RawMessage(s.result[tool]), nil
}

// called is the one call made on a tool, and whether it was made at all.
func (s *stubKits) called(tool string) (stubCall, bool) {
	for _, call := range s.calls {
		if call.Tool == tool {
			return call, true
		}
	}
	return stubCall{}, false
}

// runKit is one running run kit. Its run tool accepts the hook's four
// arguments and its stop tool accepts a Process id, which is the whole
// contract of PLAN.md section 3.
func runKit(path string) proc.Kit {
	run := manifest.Tool{
		Name: manifest.ToolRun,
		Input: map[string]any{
			"type":     "object",
			"required": []any{"package", "digest", "name", "unit"},
			"properties": map[string]any{
				"package": map[string]any{"type": "string"},
				"digest":  map[string]any{"type": "string"},
				"name":    map[string]any{"type": "string"},
				"unit":    map[string]any{"type": "object"},
			},
		},
		Output: map[string]any{"type": "object"},
	}
	stop := manifest.Tool{
		Name: manifest.ToolStop,
		Input: map[string]any{
			"type":       "object",
			"required":   []any{"id"},
			"properties": map[string]any{"id": map[string]any{"type": "string"}},
		},
		Output: map[string]any{"type": "object"},
	}
	return proc.Kit{
		Process: &proc.Process{ID: path, Package: path, State: proc.StateRunning, Expose: manifest.ExposeMCP},
		Tool:    run,
		Tools:   []manifest.Tool{run, stop},
	}
}

// ranProcess is what the fake kit answers with: a Process it started
// somewhere else, named by a UUIDv7 the way every other Process is.
func ranProcess(state string) string {
	body, _ := json.Marshal(map[string]any{"id": uuid.V7(), "state": state, "endpoint": ""})
	return string(body)
}

// runnerFixture is a Package that names a runner, its kit, and the fake
// kitbashd the registration lands in.
func runnerFixture(t *testing.T, kits *stubKits) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	f.processes.SetKits(kits)
	return f, f.pack(t, "trainer", runnerManifest)
}

func TestRunDispatchesToTheRunnerKit(t *testing.T) {
	kits := &stubKits{
		at:     map[string]proc.Kit{runnerPath: runKit(runnerPath)},
		result: map[string]string{manifest.ToolRun: ranProcess(proc.StateRunning)},
	}
	f, folder := runnerFixture(t, kits)
	digest := f.build(folder, "trainer")

	process, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if process.State != proc.StateRunning || process.Runner != runnerPath {
		t.Errorf("the Process is %+v, want it running and owned by %s", process, runnerPath)
	}
	call, made := kits.called(manifest.ToolRun)
	if !made {
		t.Fatalf("the kit was called on %v, want its run tool", kits.calls)
	}
	if call.Args["package"] != folder || call.Args["digest"] != digest || call.Args["name"] != "trainer" {
		t.Errorf("the kit was called with %v, want the Package, its newest digest and the Process name", call.Args)
	}
	unit, ok := call.Args["unit"].(map[string]any)
	if !ok || unit["type"] != "container" || unit["runner"] != runnerPath {
		t.Errorf("the kit was handed the unit %v, want the container unit as the manifest wrote it", call.Args["unit"])
	}

	// Nothing of this Package was started here: the kit owns it, and the
	// registration is what kitbashd holds.
	if len(f.daemon.Starts()) != 0 {
		t.Errorf("kitbashd was asked to start %d containers, want none for a Process a kit owns", len(f.daemon.Starts()))
	}
	reg, held := f.daemon.Registration(process.ID)
	if !held {
		t.Fatalf("the Process %s was not registered with kitbashd", process.ID)
	}
	if reg.Runner != runnerPath || reg.Container != "" || reg.Digest != digest {
		t.Errorf("the registration is %+v, want the runner, no container and the digest", reg)
	}
}

func TestRunWithoutARunnerStaysOnTheBuiltInRunner(t *testing.T) {
	kits := &stubKits{at: map[string]proc.Kit{}}
	f := newFixture(t)
	f.processes.SetKits(kits)
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")

	process, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if process.Runner != "" {
		t.Errorf("the Process names the runner %q, want the built in one", process.Runner)
	}
	if len(kits.calls) != 0 {
		t.Errorf("a kit was called %v for a Package that names no runner", kits.calls)
	}
	if len(f.daemon.Starts()) != 1 {
		t.Errorf("kitbashd was asked to start %d containers, want the one the built in runner starts",
			len(f.daemon.Starts()))
	}
}

func TestRunWithARunnerThatIsNotRunning(t *testing.T) {
	f, folder := runnerFixture(t, &stubKits{at: map[string]proc.Kit{}})
	f.build(folder, "trainer")

	_, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob == nil {
		t.Fatal("a Package whose runner is not running was started")
	}
	if prob.Slug() != problem.SlugNotFound {
		t.Errorf("problem is %s, want not-found", prob.Slug())
	}
	if !strings.Contains(prob.Fix, "proc_run") {
		t.Errorf("fix is %q, want it to say to run the kit first", prob.Fix)
	}
}

func TestRunWithARunnerWhoseSchemaRefusesTheHook(t *testing.T) {
	kit := runKit(runnerPath)
	kit.Tool.Input = map[string]any{
		"type":                 "object",
		"required":             []any{"image"},
		"additionalProperties": false,
		"properties":           map[string]any{"image": map[string]any{"type": "string"}},
	}
	kits := &stubKits{at: map[string]proc.Kit{runnerPath: kit}}
	f, folder := runnerFixture(t, kits)
	f.build(folder, "trainer")

	_, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob == nil {
		t.Fatal("a kit whose run tool refuses the hook was called")
	}
	if prob.Slug() != problem.SlugInvalidManifest {
		t.Errorf("problem is %s, want invalid-manifest", prob.Slug())
	}
	if len(kits.calls) != 0 {
		t.Errorf("the kit was called %v although its schema refuses the hook", kits.calls)
	}
}

func TestRunWithARunnerThatAnswersNoProcessID(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"id": "container-7", "state": proc.StateRunning})
	kits := &stubKits{
		at:     map[string]proc.Kit{runnerPath: runKit(runnerPath)},
		result: map[string]string{manifest.ToolRun: string(body)},
	}
	f, folder := runnerFixture(t, kits)
	f.build(folder, "trainer")

	_, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob == nil {
		t.Fatal("a kit that answered with an id no other tool could name was believed")
	}
	if prob.Slug() != problem.SlugInternal {
		t.Errorf("problem is %s, want internal", prob.Slug())
	}
	if len(f.daemon.Registrations()) != 0 {
		t.Errorf("%d Processes were registered from an answer that is not a Process",
			len(f.daemon.Registrations()))
	}
}

func TestListShowsTheProcessAKitOwns(t *testing.T) {
	kits := &stubKits{
		at:     map[string]proc.Kit{runnerPath: runKit(runnerPath)},
		result: map[string]string{manifest.ToolRun: ranProcess(proc.StateRunning)},
	}
	f, folder := runnerFixture(t, kits)
	digest := f.build(folder, "trainer")
	process, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}

	list, prob := f.processes.List(context.Background())
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	var found *proc.Process
	for i := range list.Processes {
		if list.Processes[i].ID == process.ID {
			found = &list.Processes[i]
		}
	}
	if found == nil {
		t.Fatalf("proc_list answered %+v, want the Process the kit runs", list.Processes)
	}
	if found.Runner != runnerPath || found.Package != folder || found.Digest != digest {
		t.Errorf("the listed Process is %+v, want the runner, the Package and the digest", *found)
	}
	if found.State != proc.StateRunning {
		t.Errorf("the listed Process is %s, want running for as long as it is registered", found.State)
	}
}

func TestRunAgainForgetsTheRegistrationTheKitReplaced(t *testing.T) {
	kits := &stubKits{
		at:     map[string]proc.Kit{runnerPath: runKit(runnerPath)},
		result: map[string]string{manifest.ToolRun: ranProcess(proc.StateRunning)},
	}
	f, folder := runnerFixture(t, kits)
	f.build(folder, "trainer")
	first, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	// The fixture answers with a new Process every time, which is a kit that
	// replaced the one it was running under that name.
	kits.result[manifest.ToolRun] = ranProcess(proc.StateRunning)
	second, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run again: %s", prob.Detail)
	}

	list, prob := f.processes.List(context.Background())
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	var ids []string
	for _, p := range list.Processes {
		if p.Package == folder {
			ids = append(ids, p.ID)
		}
	}
	if len(ids) != 1 || ids[0] != second.ID {
		t.Fatalf("proc_list holds %v for %s, want only the Process the kit answered with, %s",
			ids, folder, second.ID)
	}
	if _, held := f.daemon.Registration(first.ID); held {
		unregistered := false
		for _, id := range f.daemon.Unregistered() {
			if id == first.ID {
				unregistered = true
			}
		}
		if !unregistered {
			t.Errorf("the registration of the replaced Process %s was not revoked", first.ID)
		}
	}
}

// refusedRegistration is a kitbashd that will not register anything, which is
// what a member at their Process limit, an id another member holds and a
// daemon that is not answering all look like from here.
func refusedRegistration() teltest.Response {
	return teltest.Problem(http.StatusConflict, problem.SlugConflict, "Conflict",
		"tester already has 64 Processes registered",
		"Stop a Process you are no longer using, which unregisters it, then run this one.")
}

// A kit has started the Process by the time the registration is asked for, so
// a registration that fails leaves one running that nothing knows about. The
// kit is told to stop it again, and the caller reads the problem that refused
// the registration with what became of the Process added to it.
func TestRunWhoseRegistrationFailsStopsTheProcessTheKitStarted(t *testing.T) {
	kits := &stubKits{
		at: map[string]proc.Kit{runnerPath: runKit(runnerPath)},
		result: map[string]string{
			manifest.ToolRun:  ranProcess(proc.StateRunning),
			manifest.ToolStop: `{"id":"stopped"}`,
		},
	}
	f, folder := runnerFixture(t, kits)
	f.build(folder, "trainer")
	f.daemon.AnswerProcesses(refusedRegistration())

	_, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob == nil {
		t.Fatal("a Process that could not be registered was reported as run")
	}
	if prob.Slug() != problem.SlugConflict {
		t.Errorf("problem is %s, want the one the registration failed with", prob.Slug())
	}
	run, _ := kits.called(manifest.ToolRun)
	stop, forwarded := kits.called(manifest.ToolStop)
	if !forwarded {
		t.Fatalf("the kit was called %v, want the Process it started stopped again", kits.calls)
	}
	started := run.Args["name"]
	if stop.Args["id"] == nil || stop.Args["id"] == started {
		t.Errorf("the kit was asked to stop %v, want the id it answered the run with", stop.Args)
	}
	if !strings.Contains(prob.Detail, "stopped again through the kit") {
		t.Errorf("detail is %q, want it to say what became of the Process the kit had started", prob.Detail)
	}
	if !strings.Contains(prob.Fix, "Nothing of this Package is left running") {
		t.Errorf("fix is %q, want it to say nothing is left running", prob.Fix)
	}
}

// The other half: a kit that cannot be told to stop leaves a Process running
// that no tool can reach, so the problem says which id it is and that the kit
// has to be told directly.
func TestRunWhoseRegistrationFailsAndTheKitCannotStop(t *testing.T) {
	cases := map[string]func(*stubKits){
		"the kit declares no stop tool": func(kits *stubKits) {
			kit := runKit(runnerPath)
			kit.Tools = []manifest.Tool{kit.Tool}
			kits.at[runnerPath] = kit
		},
		"the kit refuses to stop it": func(kits *stubKits) {
			kits.errs = map[string]*problem.Problem{
				manifest.ToolStop: problem.Internal(runnerPath, "the machine is not answering", ""),
			}
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			kits := &stubKits{
				at: map[string]proc.Kit{runnerPath: runKit(runnerPath)},
				result: map[string]string{
					manifest.ToolRun:  ranProcess(proc.StateRunning),
					manifest.ToolStop: `{"id":"stopped"}`,
				},
			}
			arrange(kits)
			f, folder := runnerFixture(t, kits)
			f.build(folder, "trainer")
			f.daemon.AnswerProcesses(refusedRegistration())

			_, prob := f.processes.Run(context.Background(), folder, "", "")
			if prob == nil {
				t.Fatal("a Process that could not be registered was reported as run")
			}
			if prob.Slug() != problem.SlugConflict {
				t.Errorf("problem is %s, want the one the registration failed with", prob.Slug())
			}
			if !strings.Contains(prob.Detail, "still running with no registration") {
				t.Errorf("detail is %q, want it to say the Process is still running", prob.Detail)
			}
			if !strings.Contains(prob.Fix, runnerPath) {
				t.Errorf("fix is %q, want it to name the kit that has to be told", prob.Fix)
			}
		})
	}
}

// An endpoint a kit answers that is not on this host's loopback address is
// reported to the caller and not registered: the Telemetry fan out is POSTed
// to what is registered, and kitbashd must never be pointed at the network.
func TestRunRegistersNoEndpointOutsideTheLoopback(t *testing.T) {
	answer, _ := json.Marshal(map[string]any{
		"id": uuid.V7(), "state": proc.StateRunning, "endpoint": "http://gpu-box.example:8080",
	})
	kits := &stubKits{
		at:     map[string]proc.Kit{runnerPath: runKit(runnerPath)},
		result: map[string]string{manifest.ToolRun: string(answer)},
	}
	f, folder := runnerFixture(t, kits)
	f.build(folder, "trainer")

	process, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if process.Endpoint != "http://gpu-box.example:8080" {
		t.Errorf("proc_run answered the endpoint %q, want the one the kit published", process.Endpoint)
	}
	reg, held := f.daemon.Registration(process.ID)
	if !held {
		t.Fatalf("the Process %s was not registered", process.ID)
	}
	if reg.Endpoint != "" {
		t.Errorf("the registration carries the endpoint %q, want none: kitbashd delivers to the loopback address only",
			reg.Endpoint)
	}
}

// A kit that refuses to stop a Process keeps it: the registration stays, so
// proc_list still names it and proc_stop can be called again.
func TestStopTheKitRefusesLeavesTheRegistration(t *testing.T) {
	kits := &stubKits{
		at:     map[string]proc.Kit{runnerPath: runKit(runnerPath)},
		result: map[string]string{manifest.ToolRun: ranProcess(proc.StateRunning)},
		errs: map[string]*problem.Problem{
			manifest.ToolStop: problem.Internal(runnerPath, "the machine is not answering", ""),
		},
	}
	f, folder := runnerFixture(t, kits)
	f.build(folder, "trainer")
	process, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}

	if _, prob := f.processes.Stop(context.Background(), process.ID); prob == nil {
		t.Fatal("a stop the kit refused was reported as a stop")
	}
	if _, held := f.daemon.Registration(process.ID); !held {
		t.Fatalf("the registration of %s is gone although the kit still runs it", process.ID)
	}
	for _, id := range f.daemon.Unregistered() {
		if id == process.ID {
			t.Errorf("the Process %s was unregistered although the kit refused to stop it", id)
		}
	}
	list, prob := f.processes.List(context.Background())
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	found := false
	for _, p := range list.Processes {
		if p.ID == process.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("proc_list no longer holds %s, which its kit is still running", process.ID)
	}
}

// builderManifest names a build kit and no runner, which is the pairing that
// does not work: the image is wherever the kit built it and this host has
// nothing to run.
const builderOnlyManifest = `name: trainer
description: A job built by a kit somewhere else, with nothing here to run it from.
deploy:
  units:
    - type: container
      build: .
      builder: /org/nix-build
      expose: none
`

// A Package that names a builder and no runner has no image in this member's
// store, and "this Package has not been built yet" would send the agent back
// to pkg_build, which it has already called. The problem names the builder and
// says the unit needs a runner too, see PLAN.md section 3.
func TestRunOfAPackageBuiltByAKitWithNoRunner(t *testing.T) {
	f := newFixture(t)
	f.processes.SetKits(&stubKits{at: map[string]proc.Kit{}})
	folder := f.pack(t, "trainer", builderOnlyManifest)

	_, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob == nil {
		t.Fatal("a Package with no image on this host was run")
	}
	if prob.Slug() != problem.SlugNotFound {
		t.Errorf("problem is %s, want not-found", prob.Slug())
	}
	if !strings.Contains(prob.Detail, "/org/nix-build") {
		t.Errorf("detail is %q, want it to name the build kit that holds the image", prob.Detail)
	}
	if !strings.Contains(prob.Fix, "runner") {
		t.Errorf("fix is %q, want it to say the unit needs a runner as well", prob.Fix)
	}
}

func TestStopForwardsToTheRunnerKit(t *testing.T) {
	kits := &stubKits{
		at: map[string]proc.Kit{runnerPath: runKit(runnerPath)},
		result: map[string]string{
			manifest.ToolRun:  ranProcess(proc.StateRunning),
			manifest.ToolStop: `{"id":"stopped"}`,
		},
	}
	f, folder := runnerFixture(t, kits)
	f.build(folder, "trainer")
	process, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}

	out, prob := f.processes.Stop(context.Background(), process.ID)
	if prob != nil {
		t.Fatalf("Stop: %s", prob.Detail)
	}
	if out.State != proc.StateStopped {
		t.Errorf("proc_stop answered %+v, want the stopped state", out)
	}
	call, made := kits.called(manifest.ToolStop)
	if !made {
		t.Fatalf("the kit was called on %v, want its stop tool", kits.calls)
	}
	if call.Args["id"] != process.ID {
		t.Errorf("the kit was asked to stop %v, want the Process id", call.Args)
	}
	// The registration goes with the Process, so proc_list no longer holds it.
	list, prob := f.processes.List(context.Background())
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	for _, p := range list.Processes {
		if p.ID == process.ID {
			t.Errorf("proc_list still holds the stopped Process %s", p.ID)
		}
	}
}

func TestStopWithARunnerThatDeclaresNoStopTool(t *testing.T) {
	kit := runKit(runnerPath)
	kit.Tools = []manifest.Tool{kit.Tool}
	kits := &stubKits{
		at:     map[string]proc.Kit{runnerPath: kit},
		result: map[string]string{manifest.ToolRun: ranProcess(proc.StateRunning)},
	}
	f, folder := runnerFixture(t, kits)
	f.build(folder, "trainer")
	process, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}

	_, prob = f.processes.Stop(context.Background(), process.ID)
	if prob == nil {
		t.Fatal("a Process whose kit declares no stop tool was reported stopped")
	}
	if prob.Slug() != problem.SlugNotPermitted {
		t.Errorf("problem is %s, want not-permitted", prob.Slug())
	}
	if !strings.Contains(prob.Fix, runnerPath) {
		t.Errorf("fix is %q, want it to name the kit that owns the Process", prob.Fix)
	}
}
