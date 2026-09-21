package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// The M12 half of the daemon: a Package of more than one unit runs as one
// podman pod, see PLAN.md section 5.6. Every test here is about the pod path;
// the single unit path is the one every other test in this package drives, and
// TestASingleUnitStartIsTheCommandLineItAlwaysWas is what holds it still.

// cacheDigest is the second image of a two unit Package, the sidecar's.
const cacheDigest = "sha256:" + "cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12"

// superviseP registers one Process of two units for the caller, the way
// processes_register would, and answers its id. The face is the web unit and
// the sidecar is the cache, which is the shape of the Package M12 is for.
func (h *harness) supervisePod(owner string) string {
	h.t.Helper()
	_, hash, err := store.NewToken()
	if err != nil {
		h.t.Fatalf("NewToken: %v", err)
	}
	id := uuid.V7()
	if err := h.store.RegisterProcess(context.Background(), store.Process{
		ID:           id,
		Owner:        owner,
		Package:      "/home/" + owner + "/board",
		Name:         "board",
		Container:    "kitbash-board-board-web",
		Digest:       testDigest,
		Expose:       ExposeHTTP,
		Endpoint:     "http://127.0.0.1:40275",
		FanoutSecret: "the-secret",
		Composition: store.Composition{
			Pod: "kitbash-board-board",
			// The face is first in the manifest's order, so a start that
			// went through the units as they are declared would run it
			// first: the face going last is a decision and not the order.
			Units: []store.Unit{
				{Name: "web", Container: "kitbash-board-board-web", Digest: testDigest, Face: true},
				{Name: "cache", Container: "kitbash-board-board-cache", Digest: cacheDigest},
			},
		},
		RegisteredAt: time.Now().UTC(),
	}, hash, store.Quota{}); err != nil {
		h.t.Fatalf("RegisterProcess: %v", err)
	}
	return id
}

// podStart is the start request of that Process: one command line per unit.
func podStart() startRequest {
	return startRequest{
		Container: "kitbash-board-board-web",
		Image:     testDigest,
		Publish:   []portMapping{{HostPort: 40275, ContainerPort: 8080}},
		Units: []startUnit{
			{Name: "cache", Memory: "128Mi"},
			{Name: "web", Env: map[string]string{"LOG_LEVEL": "debug"}, Memory: "384Mi"},
		},
	}
}

// TestAPodIsOneProcessOfSeveralContainers is the acceptance sentence of issue
// 162: the pod owns the published port and the cgroup, every unit joins it,
// and the four steps run once per unit.
func TestAPodIsOneProcessOfSeveralContainers(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervisePod(h.user)

	res, body := h.start(id, podStart())
	if res.StatusCode != http.StatusOK {
		t.Fatalf("start = %d %s, want 200", res.StatusCode, body)
	}

	pods := fake.PodCalls()
	if len(pods) != 1 {
		t.Fatalf("the runtime was asked for %d pods, want one", len(pods))
	}
	pod := pods[0]
	if pod.Options.Name != "kitbash-board-board" {
		t.Errorf("the pod is %s, want kitbash-board-board", pod.Options.Name)
	}
	// The pod publishes the face unit's port, because it holds the network
	// namespace every unit is in.
	if len(pod.Options.Publish) != 1 || pod.Options.Publish[0].HostPort != 40275 {
		t.Errorf("the pod publishes %+v, want the face unit's 40275", pod.Options.Publish)
	}
	if !slices.Contains(pod.Args, "--publish") {
		t.Errorf("pod create is %v, want it to publish the port", pod.Args)
	}
	if !slices.Contains(pod.Args, "--cgroup-parent=/kitbash/"+h.user+"/"+id) {
		t.Errorf("pod create is %v, want the Process's cgroup as its parent", pod.Args)
	}

	runs := fake.Runs()
	if len(runs) != 2 {
		t.Fatalf("the runtime made %d containers, want one per unit", len(runs))
	}
	for _, run := range runs {
		if run.Options.Pod != "kitbash-board-board" {
			t.Errorf("%s was created with pod %q, want the pod", run.Options.Name, run.Options.Pod)
		}
		// podman refuses these three on a container of a pod, verified
		// against podman 5.7.0, so they must not be on the command line.
		if slices.Contains(run.Args, "--publish") {
			t.Errorf("%s was created with --publish beside --pod: %v", run.Options.Name, run.Args)
		}
		for _, arg := range run.Args {
			if strings.HasPrefix(arg, "--cgroup-parent") {
				t.Errorf("%s was created with %s beside --pod, and the pod owns the cgroup",
					run.Options.Name, arg)
			}
		}
		if !slices.Contains(run.Args, "--pod") {
			t.Errorf("%s was created without --pod: %v", run.Options.Name, run.Args)
		}
	}
	// Every unit is prepared and read before any of them runs.
	if n := len(fake.Inits()); n != 2 {
		t.Errorf("the runtime prepared %d containers, want one per unit", n)
	}
	if n := len(fake.Calls()); n != 2 {
		t.Errorf("the runtime started %d containers, want one per unit", n)
	}
}

// The face is started last, so a sidecar the face talks to is up before the
// unit that answers the address runs.
func TestTheFaceUnitIsStartedLast(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervisePod(h.user)

	if res, body := h.start(id, podStart()); res.StatusCode != http.StatusOK {
		t.Fatalf("start = %d %s, want 200", res.StatusCode, body)
	}
	started := fake.Calls()
	if len(started) != 2 {
		t.Fatalf("the runtime started %d containers, want two", len(started))
	}
	if started[0].Container != "kitbash-board-board-cache" ||
		started[1].Container != "kitbash-board-board-web" {
		t.Errorf("the units were started %s then %s, want the sidecar first and the face last",
			started[0].Container, started[1].Container)
	}
}

// Every unit is given the name of the unit it is, which is what a record it
// exports carries and what kitbashd holds it to, see PLAN.md section 2.4.
func TestEveryUnitOfAPodIsGivenItsName(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervisePod(h.user)

	if res, body := h.start(id, podStart()); res.StatusCode != http.StatusOK {
		t.Fatalf("start = %d %s, want 200", res.StatusCode, body)
	}
	for _, run := range fake.Runs() {
		want := "KITBASH_UNIT=" + strings.TrimPrefix(run.Options.Name, "kitbash-board-board-")
		if !strings.Contains(run.Env, want+"\n") {
			t.Errorf("the environment of %s is %q, want it to carry %s", run.Options.Name, run.Env, want)
		}
	}
}

// A Process of one unit is given none: there is one container and the Process
// names it, so nothing about a single unit start changed.
func TestASingleUnitIsGivenNoUnitName(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervise(h.user, "kitbash-echo-echo")

	if res, body := h.start(id, startRequest{Image: testDigest}); res.StatusCode != http.StatusOK {
		t.Fatalf("start = %d %s, want 200", res.StatusCode, body)
	}
	runs := fake.Runs()
	if len(runs) != 1 {
		t.Fatalf("the runtime made %d containers, want one", len(runs))
	}
	if strings.Contains(runs[0].Env, EnvUnit) {
		t.Errorf("the environment of a single unit Process is %q, want no %s in it", runs[0].Env, EnvUnit)
	}
}

// TestASingleUnitStartIsTheCommandLineItAlwaysWas is the guard on every other
// Process on a kitbash host: pods are new and a Package of one unit must be
// created by exactly the command line it was created by before they existed.
// The list is written out rather than derived, so a change to the argument
// builder fails here whatever else it passes.
func TestASingleUnitStartIsTheCommandLineItAlwaysWas(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervise(h.user, "kitbash-echo-echo")

	res, body := h.start(id, startRequest{
		Image:   testDigest,
		Restart: "always",
		CPU:     "1",
		Memory:  "512Mi",
		Publish: []portMapping{{HostPort: 40275, ContainerPort: 8080}},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("start = %d %s, want 200", res.StatusCode, body)
	}
	runs := fake.Runs()
	if len(runs) != 1 {
		t.Fatalf("the runtime made %d containers, want one", len(runs))
	}
	args := runs[0].Args
	envFile := runs[0].Options.EnvFile
	want := []string{
		"create", "--name", "kitbash-echo-echo", "--interactive",
		"--label", "kitbash.digest=" + testDigest,
		"--label", "kitbash.expose=none",
		"--label", "kitbash.id=" + id,
		"--label", "kitbash.name=echo",
		"--label", "kitbash.package=/home/" + h.user + "/echo",
		"--label", "kitbash.user=" + h.user,
		"--env-file", envFile,
		"--restart", "always",
		"--cpus", "1",
		"--memory", "512m",
		"--pids-limit", "512",
		"--cgroups=enabled", "--cgroup-parent=/kitbash/" + h.user + "/" + id,
		"--publish", "127.0.0.1:40275:8080",
		testDigest,
	}
	if !slices.Equal(args, want) {
		t.Errorf("a single unit is created with\n  %v\nwant\n  %v", args, want)
	}
	if len(fake.PodCalls()) != 0 {
		t.Error("a Package of one unit was routed through a pod")
	}
}

// A start that fails on one unit takes the whole pod down. Half a pod is a
// registration naming containers that never ran and an address nothing
// answers on, see PLAN.md section 5.6.
func TestAUnitThatWillNotStartTakesThePodDown(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervisePod(h.user)
	fake.StartErrs = map[string]error{
		"kitbash-board-board-cache": errors.New("the runtime refused this container"),
	}

	res, body := h.start(id, podStart())
	if res.StatusCode == http.StatusOK {
		t.Fatalf("start = %d %s, want a refusal", res.StatusCode, body)
	}
	removed := fake.PodRemovals()
	if len(removed) != 1 || removed[0].Container != "kitbash-board-board" {
		t.Fatalf("the pod removals are %+v, want the pod taken down", removed)
	}
	// The containers go with the pod, which is what podman pod rm does.
	config, err := fake.ContainerConfig(context.Background(),
		sysusers.Member{Name: h.user}, "kitbash-board-board-web")
	if err == nil && config.Image != "" {
		t.Errorf("the face container is still there after a start that failed: %+v", config)
	}
	// And the face was never started, because the sidecar goes first.
	for _, call := range fake.Calls() {
		if call.Container == "kitbash-board-board-web" {
			t.Error("the face ran after a sidecar that would not start")
		}
	}
}

// A create that fails takes the pod down too, before anything has run.
func TestAUnitThatCannotBeCreatedTakesThePodDown(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervisePod(h.user)
	fake.Missing = map[string]bool{cacheDigest: true}

	res, body := h.start(id, podStart())
	if res.StatusCode == http.StatusOK {
		t.Fatalf("start = %d %s, want a refusal", res.StatusCode, body)
	}
	if n := len(fake.PodRemovals()); n != 1 {
		t.Errorf("the pod was removed %d times, want once", n)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Errorf("%d containers were started for a pod that could not be made whole", n)
	}
}

// The pod is stopped and removed as one, so nothing of the Process is left
// running or holding its name.
func TestStoppingAPodStopsEveryUnit(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervisePod(h.user)
	if res, body := h.start(id, podStart()); res.StatusCode != http.StatusOK {
		t.Fatalf("start = %d %s, want 200", res.StatusCode, body)
	}

	if res, body := h.postJSON(http.MethodPost, processesPath+"/"+id+"/stop", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("stop = %d %s, want 200", res.StatusCode, body)
	}
	if n := len(fake.PodStops()); n != 1 {
		t.Errorf("the pod was stopped %d times, want once", n)
	}
	// The pod goes with the stop: a stopped pod holds its name and its
	// published port, and podman refuses a pod under a name that is taken,
	// so the next run of this Package could not make it again.
	removals := fake.PodRemovals()
	if len(removals) != 1 || !removals[0].Force {
		t.Errorf("the pod removals are %+v, want one forced removal", removals)
	}
	state, err := fake.PodState(context.Background(), sysusers.Member{Name: h.user}, "kitbash-board-board")
	if !errors.Is(err, sysusers.ErrNoPod) {
		t.Errorf("the pod is %q after a stop, want it gone: %v", state, err)
	}
	for _, name := range []string{"kitbash-board-board-web", "kitbash-board-board-cache"} {
		config, err := fake.ContainerConfig(context.Background(), sysusers.Member{Name: h.user}, name)
		if err == nil && config.Image != "" {
			t.Errorf("%s is still on the host after a stop: %+v", name, config)
		}
	}
}

// The ceiling of a pod is what its units together asked for, so two units
// cannot between them spend more than the Process was given.
func TestThePodCeilingIsTheSumOfItsUnits(t *testing.T) {
	h, _ := supervised(t)
	id := h.supervisePod(h.user)
	if res, body := h.start(id, podStart()); res.StatusCode != http.StatusOK {
		t.Fatalf("start = %d %s, want 200", res.StatusCode, body)
	}
	held, found, err := h.store.Process(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("Process: %v found=%v", err, found)
	}
	// 128Mi and 384Mi, in the bytes both the runtime and the cgroup files
	// take.
	if held.Limits.Memory != "536870912" {
		t.Errorf("the ceiling is %q, want the sum of 128Mi and 384Mi in bytes", held.Limits.Memory)
	}
	if !held.Limits.Ceiling {
		t.Error("the registration does not record that the Process was placed under a ceiling")
	}
}

// A unit no registration names is refused: the registry is what says which
// containers a Process has, the same rule the container name and the image
// already follow.
func TestAStartThatNamesAnUnknownUnitIsRefused(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervisePod(h.user)
	req := podStart()
	req.Units = append(req.Units, startUnit{Name: "stowaway"})

	res, body := h.start(id, req)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("start = %d %s, want 400", res.StatusCode, body)
	}
	if !strings.Contains(string(body), "stowaway") {
		t.Errorf("the refusal is %s, want it to name the unit", body)
	}
	if len(fake.PodCalls()) != 0 {
		t.Error("a pod was made for a start that was refused")
	}
}

// And a start that leaves one of them out, because a pod is every unit or
// none of them.
func TestAStartThatLeavesAUnitOutIsRefused(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervisePod(h.user)
	req := podStart()
	req.Units = req.Units[:1]

	res, body := h.start(id, req)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("start = %d %s, want 400", res.StatusCode, body)
	}
	if !strings.Contains(string(body), "web") {
		t.Errorf("the refusal is %s, want it to name the unit with no command line", body)
	}
	if len(fake.PodCalls()) != 0 {
		t.Error("a pod was made for a start that was refused")
	}
}

// Each unit carries its own image and its own name as labels, and only the
// face carries the exposure: a sidecar is not a second face.
func TestEachUnitCarriesItsOwnImageAndExposure(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervisePod(h.user)
	if res, body := h.start(id, podStart()); res.StatusCode != http.StatusOK {
		t.Fatalf("start = %d %s, want 200", res.StatusCode, body)
	}
	for _, run := range fake.Runs() {
		unit := run.Options.Labels[podman.LabelUnit]
		switch unit {
		case "cache":
			if run.Options.Image != cacheDigest {
				t.Errorf("the cache runs %s, want its own image", run.Options.Image)
			}
			if got := run.Options.Labels[podman.LabelExpose]; got != ExposeNone {
				t.Errorf("the cache is exposed %q, want none", got)
			}
		case "web":
			if run.Options.Image != testDigest {
				t.Errorf("the face runs %s, want its own image", run.Options.Image)
			}
			if got := run.Options.Labels[podman.LabelExpose]; got != ExposeHTTP {
				t.Errorf("the face is exposed %q, want http", got)
			}
		default:
			t.Errorf("a container carries the unit label %q", unit)
		}
		if run.Options.Labels[podman.LabelPod] != "kitbash-board-board" {
			t.Errorf("%s carries no pod label", run.Options.Name)
		}
	}
}

// The manifest schema bounds deploy.units at the same number the daemon does.
// Two bounds that can drift are one bound that does not hold: a manifest the
// schema accepts and the registration refuses is a Package a member can write
// and cannot run.
func TestTheSchemaBoundsTheUnitsAtTheSameNumber(t *testing.T) {
	var schema struct {
		Properties struct {
			Deploy struct {
				Properties struct {
					Units struct {
						MinItems int `json:"minItems"`
						MaxItems int `json:"maxItems"`
					} `json:"units"`
				} `json:"properties"`
			} `json:"deploy"`
		} `json:"properties"`
	}
	body, err := os.ReadFile(filepath.Join("..", "..", "spec", "manifest.schema.json"))
	if err != nil {
		t.Fatalf("reading the manifest schema: %v", err)
	}
	if err := json.Unmarshal(body, &schema); err != nil {
		t.Fatalf("decoding the manifest schema: %v", err)
	}
	units := schema.Properties.Deploy.Properties.Units
	if units.MaxItems != MaxUnits {
		t.Errorf("spec/manifest.schema.json bounds deploy.units at %d and this daemon at %d",
			units.MaxItems, MaxUnits)
	}
	if units.MinItems != 1 {
		t.Errorf("spec/manifest.schema.json requires %d units, want at least one", units.MinItems)
	}
}

// The environment file of one unit holds this Process's live Telemetry token,
// and it is removed as soon as the container has been made with it rather than
// at the end of the start: a pod writes one file per unit, so a removal
// deferred to the end would leave the first unit's token on the host for as
// long as the rest of the pod takes to come up.
func TestAUnitsEnvironmentFileIsGoneBeforeTheNextUnitIsMade(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervisePod(h.user)
	var held []int
	fake.OnCreate = func(opts podman.RunOptions) {
		entries, err := os.ReadDir(filepath.Dir(opts.EnvFile))
		if err != nil {
			t.Errorf("reading the environment directory: %v", err)
			return
		}
		held = append(held, len(entries))
	}

	if res, body := h.start(id, podStart()); res.StatusCode != http.StatusOK {
		t.Fatalf("start = %d %s, want 200", res.StatusCode, body)
	}
	if len(held) != 2 {
		t.Fatalf("%d containers were made, want one per unit", len(held))
	}
	for i, n := range held {
		if n != 1 {
			t.Errorf("the host held %d environment files when unit %d was made, want only its own", n, i)
		}
	}
	// And nothing is left when the start answers.
	entries, err := os.ReadDir(h.server.envDir)
	if err != nil {
		t.Fatalf("reading the environment directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("%d environment files were left behind by a start that finished", len(entries))
	}
}
