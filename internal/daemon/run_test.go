package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/cgroups"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// testDigest is one image id, the shape a registration carries.
const testDigest = "sha256:" + "ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34"

// supervised is a daemon whose members are a fake host and whose runtime is
// the fake runner, with the caller registered as a member of it. The caller's
// own uid is used, because the environment file of a Process is chowned to the
// member and only root may give a file away.
func supervised(t *testing.T) (*harness, *sysusers.Fake) {
	t.Helper()
	fake := sysusers.NewFake()
	h := serveWith(t, Options{
		Admin:           func(*user.User) (bool, error) { return false, nil },
		Users:           fake,
		Runner:          fake,
		ProcessEndpoint: "http://127.0.0.1:4318",
	})
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid()})
	return h, fake
}

// register writes one registration for the caller straight to the store, the
// way processes_register would, and answers its id.
func (h *harness) supervise(owner, container string) string {
	h.t.Helper()
	_, hash, err := store.NewToken()
	if err != nil {
		h.t.Fatalf("NewToken: %v", err)
	}
	id := uuid.V7()
	if err := h.store.RegisterProcess(context.Background(), store.Process{
		ID:           id,
		Owner:        owner,
		Package:      "/home/" + owner + "/echo",
		Name:         "echo",
		Container:    container,
		Digest:       testDigest,
		Expose:       ExposeNone,
		FanoutSecret: "the-secret",
		// A kit that reaches back over /mcp carries its permits block in the
		// registration, and a start must not drop it, see #76.
		Permits:      manifest.Permits{Tools: []string{"fs_read"}, Paths: []string{"/org"}},
		RegisteredAt: time.Now().UTC(),
	}, hash, 0); err != nil {
		h.t.Fatalf("RegisterProcess: %v", err)
	}
	return id
}

// start calls processes_start for one Process.
func (h *harness) start(id string, req startRequest) (*http.Response, []byte) {
	h.t.Helper()
	return h.postJSON(http.MethodPost, processesPath+"/"+id+"/start", req)
}

// TestStartRunsTheContainerAsTheOwnerInTheirCgroup is the acceptance sentence
// of issue 69: kitbashd runs the Process, in the member's delegated cgroup,
// with the limits the manifest declared.
func TestStartRunsTheContainerAsTheOwnerInTheirCgroup(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervise(h.user, "kitbash-echo-echo")

	res, body := h.start(id, startRequest{
		Image:     testDigest,
		Container: "kitbash-echo-echo",
		Labels:    map[string]string{podman.LabelCommit: "abc"},
		Env:       map[string]string{"LOG_LEVEL": "debug"},
		Restart:   "always",
		CPU:       "1",
		Memory:    "512Mi",
		Publish:   []portMapping{{HostPort: 40275, ContainerPort: 8080}},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("start = %d %s, want 200", res.StatusCode, body)
	}
	var answer startResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if answer.ID != id || answer.ContainerID == "" {
		t.Errorf("answer is %+v, want the Process and the id the runtime gave it", answer)
	}

	runs := fake.Runs()
	if len(runs) != 1 {
		t.Fatalf("the runtime was asked for %d runs, want one", len(runs))
	}
	run := runs[0]
	if run.Member != h.user {
		t.Errorf("the container ran as %s, want %s", run.Member, h.user)
	}
	// The podman child runs in the member's leaf, not under the ceiling: the
	// child is not the workload, and a small limit would kill the starter
	// instead of the thing being started.
	if run.Cgroup != cgroups.LeafDir(h.cgroups.Base, h.user) {
		t.Errorf("the child was placed in %q, want the member's leaf", run.Cgroup)
	}
	// The ceiling is the Process's own cgroup and it is written by root, in
	// the spelling the cgroup files take.
	placed := h.cgroups.Placed()
	if len(placed) != 1 {
		t.Fatalf("the cgroups prepared are %+v, want one for this Process", placed)
	}
	if placed[0].ID != id || placed[0].Name != h.user {
		t.Errorf("the cgroup is %+v, want this Process under its owner", placed[0])
	}
	want := cgroups.Limits{Memory: "536870912", CPU: "100000 100000", Pids: DefaultPidsLimit}
	if placed[0].Limits != want {
		t.Errorf("the ceiling is %+v, want %+v", placed[0].Limits, want)
	}
	wantArgs := [][2]string{
		{"--cgroup-parent=" + cgroups.Parent(h.user, id), ""},
		{"--memory", "512m"},
		{"--cpus", "1"},
		{"--pids-limit", "512"},
		{"--restart", "always"},
		{"--publish", "127.0.0.1:40275:8080"},
		{"--env-file", run.EnvFile},
	}
	for _, pair := range wantArgs {
		if !hasArg(run.Args, pair[0], pair[1]) {
			t.Errorf("the command line is %v, want %s %s", run.Args, pair[0], pair[1])
		}
	}
	if !slices.Contains(run.Args, "--cgroups=enabled") {
		t.Errorf("the command line is %v, want --cgroups=enabled", run.Args)
	}
	// The environment is in a file, never on the command line: the token is
	// one of its entries and a command line is readable on the host.
	for _, arg := range run.Args {
		if strings.HasPrefix(arg, "--env") && arg != "--env-file" {
			t.Errorf("the command line carries %q, want the environment in a file only", arg)
		}
	}
}

// The environment file is the member's to read and nobody else's, and it does
// not outlive the run: it holds a live Telemetry token.
func TestStartWritesTheEnvironmentFileForTheMemberAndRemovesIt(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervise(h.user, "kitbash-echo-echo")

	res, body := h.start(id, startRequest{
		Image: testDigest,
		Env: map[string]string{
			"LOG_LEVEL": "debug",
			// A manifest that names what kitbashd speaks for is overruled.
			EnvFanoutSecret:   "forged",
			EnvTelemetryToken: "forged",
		},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("start = %d %s, want 200", res.StatusCode, body)
	}
	run := fake.Runs()[0]
	if run.EnvFile != filepath.Join(h.envDir, id) {
		t.Errorf("the environment file is %q, want one named by the Process id under %s", run.EnvFile, h.envDir)
	}
	if run.EnvMode != 0o600 {
		t.Errorf("the environment file is %v, want 0600", run.EnvMode)
	}
	if run.EnvUID != os.Getuid() {
		t.Errorf("the environment file belongs to uid %d, want the member's %d", run.EnvUID, os.Getuid())
	}
	env := parseEnvFile(run.Env)
	if env["LOG_LEVEL"] != "debug" {
		t.Errorf("the environment is %v, want the unit's own entries", env)
	}
	if env[EnvFanoutSecret] != "the-secret" {
		t.Errorf("the fan out secret is %q, want the registration's", env[EnvFanoutSecret])
	}
	if env[EnvTelemetryToken] == "" || env[EnvTelemetryToken] == "forged" {
		t.Errorf("the token is %q, want the one kitbashd minted", env[EnvTelemetryToken])
	}
	if env[EnvProcess] != id || env[EnvUser] != h.user {
		t.Errorf("the environment is %v, want the Process id and its owner", env)
	}
	if env[EnvTelemetryEndpoint] != "http://127.0.0.1:4318" ||
		env[EnvMCPEndpoint] != "http://127.0.0.1:4318/mcp" {
		t.Errorf("the endpoints are %q and %q, want the receiver this host gives its Processes",
			env[EnvTelemetryEndpoint], env[EnvMCPEndpoint])
	}
	// The token in the file is the one the Process can export with, and the
	// one the registration answered is revoked with it.
	p, found, err := h.store.ProcessByToken(context.Background(), env[EnvTelemetryToken])
	if err != nil || !found || p.ID != id {
		t.Errorf("the token in the file resolves to %+v (found %t, err %v), want the Process", p, found, err)
	}
	if _, err := os.Stat(run.EnvFile); !os.IsNotExist(err) {
		t.Errorf("the environment file is still there after the run: %v", err)
	}
}

// A start mints a new token, which writes the registration again. Everything
// else about it has to survive that: a Process that lost its permits block on
// the way to being started would reach back over /mcp with nothing allowed.
func TestStartKeepsTheRestOfTheRegistration(t *testing.T) {
	h, _ := supervised(t)
	id := h.supervise(h.user, "kitbash-echo-echo")
	ctx := context.Background()
	before, _, err := h.store.Process(ctx, id)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	res, body := h.start(id, startRequest{Image: testDigest})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("start = %d %s, want 200", res.StatusCode, body)
	}
	after, found, err := h.store.Process(ctx, id)
	if err != nil || !found {
		t.Fatalf("Process after the start: found %t, err %v", found, err)
	}
	if !reflect.DeepEqual(after.Permits, before.Permits) {
		t.Errorf("the permits are %+v, want the registration's %+v", after.Permits, before.Permits)
	}
	if after.Package != before.Package || after.Container != before.Container ||
		after.Digest != before.Digest || after.Expose != before.Expose ||
		after.Endpoint != before.Endpoint || after.Admin != before.Admin ||
		!after.RegisteredAt.Equal(before.RegisteredAt) {
		t.Errorf("the registration is %+v, want %+v with only the token replaced", after, before)
	}
	if after.FanoutSecret != before.FanoutSecret {
		t.Errorf("the fan out secret changed on a start; the container holds the one it was given")
	}
}

// The ceiling a start wrote into the cgroup is recorded with the registration,
// because after a reboot the cgroup filesystem is empty and nothing else
// remembers what the Process was limited to.
func TestStartRecordsTheCeilingItWrote(t *testing.T) {
	h, _ := supervised(t)
	id := h.supervise(h.user, "kitbash-echo-echo")

	res, body := h.start(id, startRequest{Image: testDigest, Memory: "512Mi", CPU: "0.5"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("start = %d %s, want 200", res.StatusCode, body)
	}
	p, found, err := h.store.Process(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("Process: found %t, err %v", found, err)
	}
	want := store.Limits{Memory: "512Mi", CPU: "0.5", Pids: DefaultPidsLimit}
	if p.Limits != want {
		t.Errorf("the recorded limits are %+v, want %+v as the request carried them", p.Limits, want)
	}
	// And they are what the cgroup was given, in the spelling its files take.
	placed := h.cgroups.Placed()
	if len(placed) != 1 || placed[0].Limits.Memory != "536870912" || placed[0].Limits.CPU != "50000 100000" {
		t.Errorf("the ceiling written is %+v, want the converted limits", placed)
	}
}

// A host that cannot delegate cgroups still runs Processes: the limits are
// passed to the runtime and recorded, and nothing is placed.
func TestStartWithoutCgroupsRecordsTheLimits(t *testing.T) {
	fake := sysusers.NewFake()
	h := serveWith(t, Options{
		Admin:   func(*user.User) (bool, error) { return false, nil },
		Users:   fake,
		Runner:  fake,
		Cgroups: &cgroups.Fake{Off: true},
	})
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid()})
	id := h.supervise(h.user, "kitbash-echo-echo")

	res, body := h.start(id, startRequest{Image: testDigest, Memory: "512Mi"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("start = %d %s, want 200", res.StatusCode, body)
	}
	run := fake.Runs()[0]
	if run.Cgroup != "" {
		t.Errorf("the child was placed in %q, want nowhere on a host without delegation", run.Cgroup)
	}
	for _, arg := range run.Args {
		if strings.HasPrefix(arg, "--cgroup") {
			t.Errorf("the command line carries %q, want no cgroup on a host without delegation", arg)
		}
	}
	if !hasArg(run.Args, "--memory", "512m") {
		t.Errorf("the command line is %v, want the limit passed to the runtime anyway", run.Args)
	}
}

// A Process is run by the member who registered it and by nobody else: this
// runs a program as them.
func TestStartRefusesAnotherMembersProcess(t *testing.T) {
	h, fake := supervised(t)
	fake.Add(sysusers.Member{Name: "alice", UID: 1005})
	id := h.supervise("alice", "kitbash-echo-echo")

	res, body := h.start(id, startRequest{Image: testDigest})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("start = %d %s, want 403", res.StatusCode, body)
	}
	if got := h.problemOf(res, body).Slug(); got != problem.SlugNotPermitted {
		t.Errorf("problem is %s, want not-permitted", got)
	}
	if runs := fake.Runs(); len(runs) != 0 {
		t.Errorf("the runtime ran %+v for the wrong member", runs)
	}
}

// An id nobody registered is a Process kitbashd does not know, on all three
// actions: the registration is what says who may run what.
func TestActionsOnAnUnknownProcessAreNotFound(t *testing.T) {
	h, _ := supervised(t)
	id := uuid.V7()
	for _, action := range []string{actionStart, actionStop, actionRemove} {
		res, body := h.postJSON(http.MethodPost, processesPath+"/"+id+"/"+action, startRequest{Image: testDigest})
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s = %d %s, want 404", action, res.StatusCode, body)
		}
		if got := h.problemOf(res, body).Slug(); got != problem.SlugNotFound {
			t.Errorf("%s problem is %s, want not-found", action, got)
		}
	}
}

// Exit 125 is podman refusing to parse the command, and every part of it a
// caller chose comes from the unit, so it is the caller's to fix.
func TestStartMapsAUsageFailureOntoTheUnit(t *testing.T) {
	h, fake := supervised(t)
	fake.RunErr = fmt.Errorf("%w: podman run: exit status 125", sysusers.ErrUsage)
	id := h.supervise(h.user, "kitbash-echo-echo")

	res, body := h.start(id, startRequest{Image: testDigest})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("start = %d %s, want 400", res.StatusCode, body)
	}
	prob := h.problemOf(res, body)
	if prob.Slug() != problem.SlugBadRequest {
		t.Errorf("problem is %s, want bad-request", prob.Slug())
	}
	if !strings.Contains(prob.Fix, "deploy.units[0]") {
		t.Errorf("fix is %q, want it to name the unit options", prob.Fix)
	}
	if strings.Contains(prob.Detail, "podman") {
		t.Errorf("detail is %q, want the runtime's own words in the log only", prob.Detail)
	}
	// The environment file goes even when the run failed.
	if _, err := os.Stat(filepath.Join(h.envDir, id)); !os.IsNotExist(err) {
		t.Errorf("the environment file survived a failed run: %v", err)
	}
}

// The bug the first host run found: podman is given ten seconds to let a
// container exit on its own, and a PID 1 that ignores SIGTERM takes every one
// of them, so a child killed after five seconds is a container left half down.
// The budget the runner gives the child has to be wider than the grace the
// daemon asks for.
func TestTheStopBudgetIsWiderThanTheGraceItAsksFor(t *testing.T) {
	if sysusers.StopBudget <= StopTimeout*time.Second {
		t.Errorf("a stop is given %s and the container %d seconds to exit; the child would be killed mid stop",
			sysusers.StopBudget, StopTimeout)
	}
	// And the response outlives the work, or the caller is told nothing while
	// the daemon is still doing it.
	if ActionDeadline <= sysusers.StopBudget || StartDeadline <= sysusers.RunTimeout {
		t.Errorf("the response deadlines (%s, %s) are not wider than the runtime budgets (%s, %s)",
			ActionDeadline, StartDeadline, sysusers.StopBudget, sysusers.RunTimeout)
	}
}

// A runtime that was still working when its budget ran out is not a Package
// that is wrong: the answer says so, and says what to do next.
func TestARuntimeThatRanOutOfTimeSaysSo(t *testing.T) {
	h, fake := supervised(t)
	fake.StopErr = fmt.Errorf("%w: podman stop as loki after 40s", sysusers.ErrTimeout)
	id := h.supervise(h.user, "kitbash-echo-echo")

	res, body := h.postJSON(http.MethodPost, processesPath+"/"+id+"/stop", nil)
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("stop = %d %s, want 500", res.StatusCode, body)
	}
	prob := h.problemOf(res, body)
	if prob.Detail != "the container runtime did not answer in time" {
		t.Errorf("detail is %q, want the runtime's silence named", prob.Detail)
	}
	if !strings.Contains(prob.Fix, "proc_list") {
		t.Errorf("fix is %q, want it to say how to find out what state the Process is in", prob.Fix)
	}
}

// An image the member does not have is a Package that was never built here.
func TestStartWithoutTheImageIsNotFound(t *testing.T) {
	h, fake := supervised(t)
	fake.Missing = map[string]bool{testDigest: true}
	id := h.supervise(h.user, "kitbash-echo-echo")

	res, body := h.start(id, startRequest{Image: testDigest})
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("start = %d %s, want 404", res.StatusCode, body)
	}
	if got := h.problemOf(res, body); !strings.Contains(got.Fix, "pkg_build") {
		t.Errorf("fix is %q, want it to name pkg_build", got.Fix)
	}
}

// The command line is held to the registration: the container it names and the
// image it runs are the ones kitbashd recorded.
func TestStartRefusesAContainerTheRegistrationDoesNotName(t *testing.T) {
	h, _ := supervised(t)
	id := h.supervise(h.user, "kitbash-echo-echo")

	res, body := h.start(id, startRequest{Image: testDigest, Container: "kitbash-somebody-else"})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("start = %d %s, want 400", res.StatusCode, body)
	}
}

// The labels of a container are the Process record, so kitbashd writes the
// ones that name the Process rather than taking a caller's word for them.
func TestStartWritesTheIdentityLabelsItself(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervise(h.user, "kitbash-echo-echo")

	res, body := h.start(id, startRequest{
		Image: testDigest,
		Labels: map[string]string{
			podman.LabelID:       "forged",
			podman.LabelUser:     "root",
			podman.LabelEndpoint: "http://127.0.0.1:1",
		},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("start = %d %s, want 200", res.StatusCode, body)
	}
	labels := fake.Runs()[0].Options.Labels
	if labels[podman.LabelID] != id || labels[podman.LabelUser] != h.user {
		t.Errorf("labels are %v, want the Process id and its owner from the registration", labels)
	}
	// The endpoint is the registration's as well: it is where the fan out
	// delivers, so a container may not claim one of its own.
	if _, claimed := labels[podman.LabelEndpoint]; claimed {
		t.Errorf("labels are %v, want no endpoint on a Process registered without one", labels)
	}
}

// A limit the runtime would refuse is refused here, before a command line is
// built: the message names the field of the manifest it came from.
func TestStartRefusesLimitsTheRuntimeWouldNotTake(t *testing.T) {
	h, _ := supervised(t)
	id := h.supervise(h.user, "kitbash-echo-echo")

	cases := []struct {
		name string
		req  startRequest
	}{
		{name: "memory", req: startRequest{Image: testDigest, Memory: "lots"}},
		{name: "cpu", req: startRequest{Image: testDigest, CPU: "many"}},
		{name: "restart", req: startRequest{Image: testDigest, Restart: "sometimes"}},
		{name: "pids", req: startRequest{Image: testDigest, PidsLimit: 100000}},
		{name: "port", req: startRequest{Image: testDigest, Publish: []portMapping{{ContainerPort: 0}}}},
		{name: "environment name", req: startRequest{Image: testDigest, Env: map[string]string{"not a name": "x"}}},
		{name: "environment line break", req: startRequest{Image: testDigest, Env: map[string]string{"KEY": "a\nb"}}},
		{name: "environment count", req: startRequest{Image: testDigest, Env: manyEntries(MaxEnvEntries + 1)}},
		{name: "environment value size", req: startRequest{Image: testDigest,
			Env: map[string]string{"KEY": strings.Repeat("x", MaxEnvBytes+1)}}},
		{name: "label count", req: startRequest{Image: testDigest, Labels: manyEntries(MaxLabels + 1)}},
		{name: "label size", req: startRequest{Image: testDigest,
			Labels: map[string]string{"kitbash.note": strings.Repeat("x", MaxLabelBytes+1)}}},
		{name: "label name", req: startRequest{Image: testDigest, Labels: map[string]string{"not a name": "x"}}},
		{name: "port count", req: startRequest{Image: testDigest, Publish: tooManyPorts()}},
		{name: "host port", req: startRequest{Image: testDigest,
			Publish: []portMapping{{HostPort: 70000, ContainerPort: 8080}}}},
		{name: "another image", req: startRequest{Image: "sha256:" + strings.Repeat("b", 64)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, body := h.start(id, tc.req)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("start = %d %s, want 400", res.StatusCode, body)
			}
		})
	}
}

// Stopping and removing are the same rule as starting: kitbashd does them as
// the owner, because the container is in the member's own runtime.
func TestStopAndRemoveRunAsTheOwner(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervise(h.user, "kitbash-echo-echo")

	res, body := h.postJSON(http.MethodPost, processesPath+"/"+id+"/stop", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stop = %d %s, want 200", res.StatusCode, body)
	}
	stops := fake.Stops()
	if len(stops) != 1 || stops[0].Member != h.user || stops[0].Container != "kitbash-echo-echo" {
		t.Fatalf("stops = %+v, want one stop of the Process as its owner", stops)
	}
	if stops[0].Timeout != StopTimeout {
		t.Errorf("the container was given %d seconds, want %d", stops[0].Timeout, StopTimeout)
	}

	res, body = h.postJSON(http.MethodPost, processesPath+"/"+id+"/remove", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("remove = %d %s, want 200", res.StatusCode, body)
	}
	removals := fake.Removals()
	if len(removals) != 1 || !removals[0].Force || removals[0].Member != h.user {
		t.Errorf("removals = %+v, want one forced removal as the owner", removals)
	}
	// The cgroup goes with the container it was made for.
	cgroupRemovals := h.cgroups.Removals()
	if len(cgroupRemovals) != 1 || cgroupRemovals[0].ID != id || cgroupRemovals[0].Name != h.user {
		t.Errorf("the cgroups removed are %+v, want the one of this Process", cgroupRemovals)
	}
	// Neither touches the registration: proc_stop unregisters, which is what
	// revokes the token.
	if _, found, err := h.store.Process(context.Background(), id); err != nil || !found {
		t.Errorf("the registration went with the container (found %t, err %v)", found, err)
	}
}

// A container the runtime no longer has is not-found rather than internal: the
// Process can be run again.
func TestStopOfAMissingContainerIsNotFound(t *testing.T) {
	h, fake := supervised(t)
	fake.Missing = map[string]bool{"kitbash-echo-echo": true}
	id := h.supervise(h.user, "kitbash-echo-echo")

	res, body := h.postJSON(http.MethodPost, processesPath+"/"+id+"/stop", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("stop = %d %s, want 404", res.StatusCode, body)
	}
}

// Only POST, and only the three actions this API defines.
func TestTheActionsAreThreeAndPostOnly(t *testing.T) {
	h, _ := supervised(t)
	id := h.supervise(h.user, "kitbash-echo-echo")

	res, body := h.do(http.MethodGet, processesPath+"/"+id+"/start", "", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("GET start = %d %s, want 404", res.StatusCode, body)
	}
	res, body = h.postJSON(http.MethodPost, processesPath+"/"+id+"/restart", startRequest{})
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("a fourth action = %d %s, want 404", res.StatusCode, body)
	}
}

// Creating a member creates their cgroup subtree, so their first Process is
// placed without waiting for the next daemon start.
func TestCreatingAMemberPreparesTheirCgroup(t *testing.T) {
	fake := sysusers.NewFake()
	h := serveWith(t, Options{
		Admin:  func(*user.User) (bool, error) { return true, nil },
		Users:  fake,
		Runner: fake,
	})
	res, body := h.postJSON(http.MethodPost, usersPath, userRequest{Name: "alice", SSHKey: publicKey("alice")})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("users_create = %d %s, want 200", res.StatusCode, body)
	}
	calls := h.cgroups.Calls()
	if len(calls) != 1 || calls[0].Name != "alice" {
		t.Fatalf("the cgroups prepared are %+v, want alice's", calls)
	}
	if calls[0].UID == 0 {
		t.Errorf("alice's cgroup is %+v, want her uid", calls[0])
	}
}

// A session sshd started is outside the member's cgroup, so the kernel refuses
// to let it move a process into a container's. It asks kitbashd to place it,
// and the process it places is the peer of the connection, which the kernel
// says and no caller can claim.
func TestJoinPlacesTheCallingSessionInTheMemberLeaf(t *testing.T) {
	h, _ := supervised(t)

	res, body := h.postJSON(http.MethodPost, sessionsJoinPath, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("sessions_join = %d %s, want 200", res.StatusCode, body)
	}
	var answer sessionResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if answer.User != h.user || answer.PID != os.Getpid() {
		t.Errorf("the answer is %+v, want this process placed for %s", answer, h.user)
	}
	joined := h.cgroups.Sessions()
	if len(joined) != 1 {
		t.Fatalf("the sessions placed are %+v, want one", joined)
	}
	if joined[0].PID != os.Getpid() || joined[0].Name != h.user {
		t.Errorf("the session placed is %+v, want this process as %s", joined[0], h.user)
	}
	if joined[0].Cgroup != cgroups.LeafDir(h.cgroups.Base, h.user) {
		t.Errorf("the session went to %q, want the member's leaf", joined[0].Cgroup)
	}
	// The leaf is the one the podman children use, so exec has the common
	// ancestor the kernel asks for.
	if answer.Cgroup != joined[0].Cgroup {
		t.Errorf("the answer names %q and the fake placed it in %q", answer.Cgroup, joined[0].Cgroup)
	}
}

// A host that cannot place the session says so, and the session still works:
// everything but exec into a container kitbashd started is unaffected.
func TestJoinOnAHostThatCannotPlaceIsInternal(t *testing.T) {
	fake := sysusers.NewFake()
	h := serveWith(t, Options{Users: fake, Runner: fake, Cgroups: &cgroups.Fake{Off: true}})
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid()})

	res, body := h.postJSON(http.MethodPost, sessionsJoinPath, nil)
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("sessions_join = %d %s, want 500", res.StatusCode, body)
	}
	if got := h.problemOf(res, body).Detail; got != "this host could not place the session in its member's cgroup" {
		t.Errorf("detail is %q, want the placement named", got)
	}
}

// An account that is not a member has no cgroup to be placed in, and this is
// not the path that creates one.
func TestJoinRefusesAnAccountThatIsNotAMember(t *testing.T) {
	fake := sysusers.NewFake()
	h := serveWith(t, Options{Users: fake, Runner: fake})

	res, body := h.postJSON(http.MethodPost, sessionsJoinPath, nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("sessions_join = %d %s, want 403", res.StatusCode, body)
	}
	if len(h.cgroups.Sessions()) != 0 {
		t.Errorf("a session of a non member was placed: %+v", h.cgroups.Sessions())
	}
}

// The identity of a socket connection carries the peer's process id, which is
// what sessions_join places. It is the kernel's word, like the uid beside it.
func TestTheCallerCarriesThePeerProcessID(t *testing.T) {
	h, _ := supervised(t)
	res, body := h.postJSON(http.MethodPost, sessionsJoinPath, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("sessions_join = %d %s, want 200", res.StatusCode, body)
	}
	var answer sessionResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if answer.PID != os.Getpid() {
		t.Errorf("the peer is %d, want this test's own pid %d", answer.PID, os.Getpid())
	}
}

// A member who goes takes their cgroup with them, Processes of theirs that
// were left in it included.
func TestRemovingAMemberRemovesTheirCgroup(t *testing.T) {
	fake := sysusers.NewFake()
	h := serveWith(t, Options{
		Admin:  func(*user.User) (bool, error) { return true, nil },
		Users:  fake,
		Runner: fake,
	})
	fake.Add(sysusers.Member{Name: h.user, Admin: true})
	fake.Add(sysusers.Member{Name: "alice", UID: 1005})

	res, body := h.do(http.MethodDelete, usersPath+"/alice", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("users_remove = %d %s, want 200", res.StatusCode, body)
	}
	removals := h.cgroups.Removals()
	if len(removals) != 1 || removals[0].Name != "alice" || removals[0].ID != "" {
		t.Errorf("the removals are %+v, want alice's whole cgroup", removals)
	}
}

// PrepareCgroups is what the daemon runs at start: the root, then one subtree
// per member of the host.
func TestPrepareCgroupsCoversEveryMember(t *testing.T) {
	fake := sysusers.NewFake()
	h := serveWith(t, Options{Users: fake, Runner: fake})
	fake.Add(sysusers.Member{Name: "alice", UID: 1005})
	fake.Add(sysusers.Member{Name: "bob", UID: 1006})

	h.server.Prepare(context.Background())
	if h.cgroups.Roots != 1 {
		t.Errorf("the root was prepared %d times, want once", h.cgroups.Roots)
	}
	names := []string{}
	for _, call := range h.cgroups.Calls() {
		names = append(names, call.Name)
	}
	if !slices.Contains(names, "alice") || !slices.Contains(names, "bob") {
		t.Errorf("the subtrees prepared are %v, want one per member", names)
	}
}

// manyEntries is a map of n entries, for the bounds a request is held to.
func manyEntries(n int) map[string]string {
	entries := make(map[string]string, n)
	for i := range n {
		entries["KEY_"+strconv.Itoa(i)] = "value"
	}
	return entries
}

// tooManyPorts is one port mapping more than a Process may publish.
func tooManyPorts() []portMapping {
	ports := make([]portMapping, 0, MaxPublishPorts+1)
	for i := range MaxPublishPorts + 1 {
		ports = append(ports, portMapping{ContainerPort: 8080 + i})
	}
	return ports
}

// Two starts of one Process at once would race on its environment file, which
// is opened exclusively, and on its cgroup. They are serialised, so the second
// waits rather than failing.
func TestStartsOfOneProcessAreSerialised(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervise(h.user, "kitbash-echo-echo")

	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, _ := h.start(id, startRequest{Image: testDigest})
			codes[i] = res.StatusCode
		}()
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("start %d = %d, want 200: they are serialised, not refused", i, code)
		}
	}
	if runs := fake.Runs(); len(runs) != len(codes) {
		t.Errorf("the runtime was asked for %d runs, want %d", len(runs), len(codes))
	}
	// Every one of them left the host as it found it.
	entries, err := os.ReadDir(h.envDir)
	if err != nil {
		t.Fatalf("read the environment directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the environment directory holds %d files, want none", len(entries))
	}
}

// A daemon that was killed mid start left an environment file behind, and that
// file holds a Telemetry token. The next start of the daemon sweeps them.
func TestPrepareSweepsTheEnvironmentDirectory(t *testing.T) {
	h, _ := supervised(t)
	if err := os.MkdirAll(h.envDir, 0o711); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	stale := filepath.Join(h.envDir, "01a08654-1c5b-7828-9b6b-37044de254d4")
	if err := os.WriteFile(stale, []byte("KITBASH_TELEMETRY_TOKEN=leaked\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	h.server.Prepare(context.Background())
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale environment file is still there: %v", err)
	}
}

// hasArg reports whether a command line carries a flag, with its value when
// one is given.
func hasArg(args []string, flag, value string) bool {
	for i, arg := range args {
		if arg != flag {
			continue
		}
		if value == "" {
			return true
		}
		return i+1 < len(args) && args[i+1] == value
	}
	return false
}

// parseEnvFile reads back the KEY=value lines of one environment file.
func parseEnvFile(body string) map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		if key, value, found := strings.Cut(line, "="); found {
			env[key] = value
		}
	}
	return env
}
