package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/user"
	"strings"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/cgroups"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// registered writes one registration straight to the store, which is what a
// daemon that has just started finds there.
func (h *harness) registered(owner, container string) string {
	return h.registeredWith(owner, container, store.Limits{})
}

// registeredWith is registered with the ceiling that Process was started
// under, which is what restore writes into its cgroup again.
func (h *harness) registeredWith(owner, container string, limits store.Limits) string {
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
		Digest:       "sha256:" + "ab12cd34" + "00000000000000000000000000000000000000000000000000000000",
		Expose:       ExposeNone,
		Limits:       limits,
		RegisteredAt: time.Now().UTC(),
	}, hash, 0); err != nil {
		h.t.Fatalf("RegisterProcess: %v", err)
	}
	return id
}

// TestRestoreStartsEveryProcessAsItsOwner is the acceptance sentence of M5's
// last clause: every Process that was running before a reboot is running after
// it, see PLAN.md section 2.3.
func TestRestoreStartsEveryProcessAsItsOwner(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: "alice", UID: 1005})
	fake.Add(sysusers.Member{Name: "bob", UID: 1006})

	h.registered("alice", "kitbash-echo-one")
	h.registered("alice", "kitbash-echo-two")
	h.registered("bob", "kitbash-observe-count")

	counts := h.server.Restore(context.Background())
	if counts != (RestoreCounts{Started: 3}) {
		t.Fatalf("counts = %+v, want three started", counts)
	}

	started := map[string]sysusers.StartCall{}
	for _, call := range fake.Calls() {
		started[call.Container] = call
	}
	if len(started) != 3 {
		t.Fatalf("started %+v, want three containers", fake.Calls())
	}
	if got := started["kitbash-echo-one"]; got.Member != "alice" || got.UID != 1005 {
		t.Errorf("kitbash-echo-one ran as %+v, want alice with her uid", got)
	}
	if got := started["kitbash-observe-count"]; got.Member != "bob" || got.UID != 1006 {
		t.Errorf("kitbash-observe-count ran as %+v, want bob with his uid", got)
	}
	// The cgroup filesystem does not survive a reboot, so the ceiling of every
	// Process is created again and the child is placed in the member's leaf: a
	// container whose cgroup parent is gone does not start at all.
	placed := map[string]string{}
	for _, call := range h.cgroups.Placed() {
		placed[call.ID] = call.Leaf
	}
	if len(placed) != 3 {
		t.Fatalf("the cgroups prepared are %+v, want one per Process", h.cgroups.Placed())
	}
	for _, call := range fake.Calls() {
		if call.Cgroup == "" {
			t.Errorf("%s was started outside a cgroup of its own", call.Container)
		}
	}
	// One member cgroup per owner, not one per Process.
	if len(h.cgroups.Calls()) != 2 {
		t.Errorf("the member cgroups prepared are %+v, want one per owner", h.cgroups.Calls())
	}
}

// The ceiling survives a reboot: the cgroup filesystem is empty by then, so
// restore writes the limits of the registration back into each Process's
// cgroup before its container starts. A registration written before limits
// were recorded has none to write, and comes back without a ceiling.
func TestRestoreWritesTheCeilingAgain(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: "alice", UID: 1005})

	limited := h.registeredWith("alice", "kitbash-echo-limited",
		store.Limits{Memory: "512Mi", CPU: "0.5", Pids: 512})
	legacy := h.registered("alice", "kitbash-echo-legacy")

	if counts := h.server.Restore(context.Background()); counts.Started != 2 {
		t.Fatalf("counts = %+v, want two started", counts)
	}
	placed := map[string]cgroups.Limits{}
	for _, call := range h.cgroups.Placed() {
		placed[call.ID] = call.Limits
	}
	want := cgroups.Limits{Memory: "536870912", CPU: "50000 100000", Pids: 512}
	if placed[limited] != want {
		t.Errorf("the ceiling of the limited Process is %+v, want %+v", placed[limited], want)
	}
	if (placed[legacy] != cgroups.Limits{}) {
		t.Errorf("the ceiling of the legacy Process is %+v, want none to write", placed[legacy])
	}
	// Both containers still come back: a Process with no ceiling runs
	// unlimited rather than not at all.
	if len(fake.Calls()) != 2 {
		t.Errorf("the containers started are %+v, want both", fake.Calls())
	}
}

// A daemon that restarts without the host finds its Processes still running.
// Restore promises that every registered Process is running afterwards, not
// that it started each one, so a container that was up already is counted with
// the rest and named only in the line at the end.
func TestRestoreCountsAContainerThatIsAlreadyRunning(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: "alice", UID: 1005})
	fake.Running = map[string]bool{"kitbash-echo-up": true}

	kept := h.registered("alice", "kitbash-echo-up")
	h.registered("alice", "kitbash-echo-down")

	counts := h.server.Restore(context.Background())
	if counts.Started != 2 || counts.Running != 1 || counts.Failed != 0 {
		t.Fatalf("counts = %+v, want two started of which one was already running", counts)
	}
	// The registration of a Process that never stopped is left alone: it is
	// running, and its token is the one its container holds.
	if _, found, err := h.store.Process(context.Background(), kept); err != nil || !found {
		t.Errorf("the registration of a running Process went (found %t, err %v)", found, err)
	}
}

// TestRestoreUnregistersAMissingContainer is what keeps the registrations
// honest: a token that names a container the runtime no longer has belongs to
// no Process at all.
func TestRestoreUnregistersAMissingContainer(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: "alice", UID: 1005})
	fake.Missing = map[string]bool{"kitbash-gone": true}

	kept := h.registered("alice", "kitbash-echo")
	gone := h.registered("alice", "kitbash-gone")

	counts := h.server.Restore(context.Background())
	if counts != (RestoreCounts{Started: 1, Missing: 1}) {
		t.Fatalf("counts = %+v, want one started and one missing", counts)
	}

	ctx := context.Background()
	if _, found, err := h.store.Process(ctx, gone); err != nil || found {
		t.Errorf("the missing container is still registered (%t, %v)", found, err)
	}
	if _, found, err := h.store.Process(ctx, kept); err != nil || !found {
		t.Errorf("the container that started is no longer registered (%t, %v)", found, err)
	}
}

// TestRestoreSkipsWhatItCannotStart covers the two registrations restore
// cannot act on: one with no container name, written before M5, and one whose
// owner is no longer a member of this host. The first is unregistered, because
// no later boot could ever start it; the second is left alone.
func TestRestoreSkipsWhatItCannotStart(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: "alice", UID: 1005})

	legacy := h.registered("alice", "")
	stranger := h.registered("carol", "kitbash-echo")

	counts := h.server.Restore(context.Background())
	if counts != (RestoreCounts{Failed: 1, Legacy: 1}) {
		t.Fatalf("counts = %+v, want one failed and one legacy, nothing started", counts)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("started %+v, want nothing", fake.Calls())
	}

	ctx := context.Background()
	if _, found, err := h.store.Process(ctx, legacy); err != nil || found {
		t.Errorf("the registration with no container name is still registered (%t, %v)", found, err)
	}
	// A registration whose owner is gone keeps its record: the owner may come
	// back, and running it as somebody else is not an option.
	if _, found, err := h.store.Process(ctx, stranger); err != nil || !found {
		t.Errorf("the registration of a stranger was removed (%t, %v)", found, err)
	}
}

// TestRestoreOff is the operator's switch, KITBASH_NO_RESTORE: a host comes up
// without its Processes.
func TestRestoreOff(t *testing.T) {
	fake := sysusers.NewFake()
	h := serveWith(t, Options{
		Admin:     func(*user.User) (bool, error) { return true, nil },
		Users:     fake,
		Runner:    fake,
		NoRestore: true,
	})
	fake.Add(sysusers.Member{Name: "alice", UID: 1005})
	id := h.registered("alice", "kitbash-echo")

	if counts := h.server.Restore(context.Background()); counts != (RestoreCounts{}) {
		t.Fatalf("counts = %+v, want nothing done", counts)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("started %+v, want nothing", fake.Calls())
	}
	if _, found, err := h.store.Process(context.Background(), id); err != nil || !found {
		t.Errorf("the registration was removed (%t, %v)", found, err)
	}
}

// TestRegistrationCarriesContainerAndDigest is the field M5 adds: restore
// needs the container name and the digest, and processes_list answers them.
func TestRegistrationCarriesContainerAndDigest(t *testing.T) {
	h := serve(t, false)
	digest := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	req := registration("")
	req.Container = "kitbash-echo-echo"
	req.Digest = digest
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}

	res, body := h.do(http.MethodGet, processesPath, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, body %s", res.StatusCode, body)
	}
	var list processList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(list.Processes) != 1 {
		t.Fatalf("processes = %+v, want the one just registered", list.Processes)
	}
	if got := list.Processes[0]; got.Container != "kitbash-echo-echo" || got.Digest != digest {
		t.Errorf("process = %+v, want the container and the digest back", got)
	}

	// Both fields are optional, for the session that has not been changed yet.
	if _, res, body := h.register(registration("")); res.StatusCode != http.StatusOK {
		t.Fatalf("register without them status = %d, body %s", res.StatusCode, body)
	}
}

// TestRegistrationRefusesABadContainerOrDigest keeps a command line argument
// from carrying anything but a container name: restore passes it to podman.
func TestRegistrationRefusesABadContainerOrDigest(t *testing.T) {
	h := serve(t, false)
	cases := map[string]processRequest{
		"a container that is not kitbash's": {Container: "postgres"},
		"a container with a shell in it":    {Container: "kitbash-echo; rm -rf /"},
		"a container with a slash":          {Container: "kitbash-echo/../root"},
		"a digest that is not a digest":     {Digest: "abc"},
		"a digest of the wrong length":      {Digest: "sha256:abcdef"},
	}
	for name, fields := range cases {
		t.Run(name, func(t *testing.T) {
			req := registration("")
			req.Container = fields.Container
			req.Digest = fields.Digest
			_, res, body := h.register(req)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body %s", res.StatusCode, body)
			}
		})
	}
}

// legacyConfig is what the runtime holds for a container created before
// kitbashd gave each Process a cgroup of its own: its parent is the member's
// cgroup, which belongs to root, and starting it there is permission denied at
// every boot.
func legacyConfig(owner, digest string) sysusers.ContainerConfig {
	return sysusers.ContainerConfig{
		CgroupParent: "/kitbash/" + owner,
		Image:        digest,
		Env: map[string]string{
			"GREETING": "hello",
			// The runtime writes these two itself and kitbashd speaks for the
			// third, so none of them is carried into the new container.
			"HOSTNAME":                "5f2a1c",
			"container":               "podman",
			"KITBASH_TELEMETRY_TOKEN": "the token of the old container",
		},
		Labels: map[string]string{
			"team": "lab",
			// A label the registry owns, which is written from the
			// registration and not from the container.
			podman.LabelID: "the id the container claims",
		},
		Restart: "always",
		Publish: []podman.PortMapping{{HostPort: 40275, ContainerPort: 8080}},
	}
}

// TestRestoreHealsARegistrationWithoutACeiling is issue 95: a Process
// registered before limits were recorded has a container whose cgroup parent
// is its member's cgroup, which root owns, so the member's runtime cannot
// create the container's own cgroup under it and the start fails at every
// boot. Restore makes the ceiling anyway, unlimited, and creates the container
// again under it rather than failing for ever.
func TestRestoreHealsARegistrationWithoutACeiling(t *testing.T) {
	h, fake := serveUsers(t, true)
	captured := captureDaemonLog(t)
	// The heal writes the environment file of the new container and chowns it
	// to the owner, which only the user this test runs as can be given.
	owner := h.user
	fake.Add(sysusers.Member{Name: owner, UID: os.Getuid(), GID: os.Getgid()})

	id := h.registered(owner, "kitbash-echo-legacy")
	ctx := context.Background()
	p, _, err := h.store.Process(ctx, id)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	fake.AddImage(owner, p.Digest, 1024, nil)
	fake.Configs = map[string]sysusers.ContainerConfig{
		"kitbash-echo-legacy": legacyConfig(owner, p.Digest),
	}
	// What the host does today: the member's runtime cannot make the
	// container's cgroup under a directory that belongs to root.
	fake.StartErr = errors.New(
		"crun: create `/sys/fs/cgroup/kitbash/alice/libpod-fd4b88f9`: Permission denied: OCI permission denied")

	counts := h.server.Restore(ctx)
	if counts.Started != 1 || counts.Healed != 1 || counts.Failed != 0 {
		t.Fatalf("counts = %+v, want one started and healed", counts)
	}

	// The ceiling exists and limits nothing: what the Process gains is a
	// cgroup of its own, not a bound it never had.
	placed := h.cgroups.Placed()
	if len(placed) != 1 || placed[0].ID != id || (placed[0].Limits != cgroups.Limits{}) {
		t.Fatalf("the cgroups prepared are %+v, want one unlimited ceiling for %s", placed, id)
	}
	// The old container cannot be moved, so it is removed and made again.
	removals := fake.Removals()
	if len(removals) != 1 || removals[0].Container != "kitbash-echo-legacy" || !removals[0].Force {
		t.Fatalf("the removals are %+v, want the legacy container removed", removals)
	}
	runs := fake.Runs()
	if len(runs) != 1 {
		t.Fatalf("the containers created are %+v, want the legacy one made again", runs)
	}
	run := runs[0]
	if run.Member != owner || run.Options.Name != "kitbash-echo-legacy" || run.Options.Image != p.Digest {
		t.Errorf("the container was made as %+v, want the owner's own, by name and digest", run.Options)
	}
	if want := cgroups.Parent(owner, id); run.Options.CgroupParent != want {
		t.Errorf("the new container's cgroup parent is %q, want %q", run.Options.CgroupParent, want)
	}
	// Nothing the unit declared is lost: the ports, the restart policy and the
	// labels that are not the registry's come back with it.
	if run.Options.Restart != "always" {
		t.Errorf("the restart policy is %q, want the one the container had", run.Options.Restart)
	}
	if len(run.Options.Publish) != 1 || run.Options.Publish[0].HostPort != 40275 {
		t.Errorf("the published ports are %+v, want the one the container had", run.Options.Publish)
	}
	if run.Options.Labels["team"] != "lab" {
		t.Errorf("the labels are %+v, want the container's own kept", run.Options.Labels)
	}
	// The six that name the Process are the registration's, whatever the old
	// container claimed.
	if run.Options.Labels[podman.LabelID] != id {
		t.Errorf("kitbash.id is %q, want the registered id %q", run.Options.Labels[podman.LabelID], id)
	}
	if !strings.Contains(run.Env, "GREETING=hello") {
		t.Errorf("the environment is %q, want the variable the unit declared", run.Env)
	}
	for _, dropped := range []string{"HOSTNAME=", "container=", "the token of the old container"} {
		if strings.Contains(run.Env, dropped) {
			t.Errorf("the environment carries %q, want it dropped: %q", dropped, run.Env)
		}
	}
	if !strings.Contains(run.Env, EnvTelemetryToken+"=") {
		t.Errorf("the environment is %q, want the token this start minted", run.Env)
	}

	// The registration remembers that the ceiling exists, so the next boot
	// takes the ordinary path, and it still records no limits.
	healed, found, err := h.store.Process(ctx, id)
	if err != nil || !found {
		t.Fatalf("the healed registration is gone (found %t, err %v)", found, err)
	}
	if !healed.Limits.Ceiling || !healed.Limits.Empty() {
		t.Errorf("the limits of the healed Process are %+v, want an empty ceiling that is written", healed.Limits)
	}
	want := "was registered without limits; it now runs under an unlimited ceiling, run it again to write limits"
	if !strings.Contains(captured.String(), want) {
		t.Errorf("the daemon log is %q, want it to carry %q", captured.String(), want)
	}
}

// The second boot is the ordinary one: the registration says the ceiling is
// there, so restore starts the container it has rather than making it again.
func TestRestoreTakesTheOrdinaryPathAfterAHeal(t *testing.T) {
	h, fake := serveUsers(t, true)
	owner := h.user
	fake.Add(sysusers.Member{Name: owner, UID: os.Getuid(), GID: os.Getgid()})

	id := h.registered(owner, "kitbash-echo-legacy")
	ctx := context.Background()
	p, _, err := h.store.Process(ctx, id)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	fake.AddImage(owner, p.Digest, 1024, nil)
	fake.Configs = map[string]sysusers.ContainerConfig{
		"kitbash-echo-legacy": legacyConfig(owner, p.Digest),
	}
	fake.StartErr = errors.New("crun: Permission denied: OCI permission denied")

	if counts := h.server.Restore(ctx); counts.Healed != 1 {
		t.Fatalf("the first boot healed %+v, want one", counts)
	}
	// The container the heal made is under the ceiling, so it starts.
	fake.StartErr = nil
	fake.Configs["kitbash-echo-legacy"] = sysusers.ContainerConfig{
		CgroupParent: cgroups.Parent(owner, id),
		Image:        p.Digest,
	}

	counts := h.server.Restore(ctx)
	if counts.Started != 1 || counts.Healed != 0 || counts.Failed != 0 {
		t.Fatalf("the second boot is %+v, want one started and nothing healed", counts)
	}
	if len(fake.Runs()) != 1 || len(fake.Removals()) != 1 {
		t.Errorf("the second boot made the container again (%d runs, %d removals), want the first boot's only",
			len(fake.Runs()), len(fake.Removals()))
	}
	started := fake.Calls()
	if len(started) != 1 || started[0].Container != "kitbash-echo-legacy" {
		t.Errorf("the containers started are %+v, want the healed one started once", started)
	}
}

// TestRestoreReportsAProcessItCannotStart is the other half of issue 95: a
// registration restore still cannot start is reported through proc_list as
// failed with a reason its owner can read, rather than sitting in the runtime
// as a container that looks like it is on its way up.
func TestRestoreReportsAProcessItCannotStart(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: h.user, UID: 1005})
	fake.StartErr = errors.New("podman: cannot start the container")

	id := h.registeredWith(h.user, "kitbash-echo-stuck",
		store.Limits{Memory: "512Mi", CPU: "0.5", Pids: 512})

	if counts := h.server.Restore(context.Background()); counts.Failed != 1 || counts.Started != 0 {
		t.Fatalf("counts = %+v, want one failed", counts)
	}

	res, body := h.do(http.MethodGet, processesPath, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, body %s", res.StatusCode, body)
	}
	var list struct {
		Processes []struct {
			ID      string `json:"id"`
			Problem string `json:"problem"`
			Fix     string `json:"problemFix"`
		} `json:"processes"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(list.Processes) != 1 || list.Processes[0].ID != id {
		t.Fatalf("the listed Processes are %+v, want the one that did not come back", list.Processes)
	}
	if !strings.Contains(list.Processes[0].Problem, "kitbash-echo-stuck") {
		t.Errorf("the problem is %q, want it to name the container that did not start", list.Processes[0].Problem)
	}
	if list.Processes[0].Fix == "" {
		t.Errorf("the Process is reported with no fix, want one its owner can act on")
	}

	// A start that works clears it: the Process is running, so the last boot
	// is over.
	fake.StartErr = nil
	h.server.clearProcessProblem(id)
	if prob := h.server.processProblem(id); prob.Detail != "" {
		t.Errorf("the problem is %+v, want it cleared once the Process runs", prob)
	}
}

// A heal that cannot finish leaves the container where it is: an image the
// member no longer holds is a Process that cannot be created again, and
// removing its container first would lose it for good.
func TestRestoreKeepsAContainerItCannotCreateAgain(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: h.user, UID: 1005})

	id := h.registered(h.user, "kitbash-echo-legacy")
	ctx := context.Background()
	p, _, err := h.store.Process(ctx, id)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	// The image is not in the member's store, which is the one failure that
	// would leave the Process with no container at all.
	fake.Configs = map[string]sysusers.ContainerConfig{
		"kitbash-echo-legacy": legacyConfig(h.user, p.Digest),
	}
	fake.StartErr = errors.New("crun: Permission denied: OCI permission denied")

	if counts := h.server.Restore(ctx); counts.Failed != 1 || counts.Healed != 0 {
		t.Fatalf("counts = %+v, want one failed and nothing healed", counts)
	}
	if len(fake.Removals()) != 0 || len(fake.Runs()) != 0 {
		t.Errorf("the container was touched (%d removals, %d runs), want it left where it is",
			len(fake.Removals()), len(fake.Runs()))
	}
	prob := h.server.processProblem(id)
	if !strings.Contains(prob.Detail, "registered before kitbashd gave each Process a cgroup of its own") || prob.Fix == "" {
		t.Errorf("the problem is %+v, want the owner told why it did not come back", prob)
	}
}

// A container created before kitbashd had cgroups at all names no parent. It
// starts as it always has, so restore leaves it where it is: taking a working
// container away to give it a ceiling it never had is not what a boot is for.
func TestRestoreLeavesALegacyContainerWithNoCgroupParent(t *testing.T) {
	h, fake := serveUsers(t, true)
	captured := captureDaemonLog(t)
	fake.Add(sysusers.Member{Name: "alice", UID: 1005})

	id := h.registered("alice", "kitbash-echo-unplaced")
	ctx := context.Background()
	fake.Configs = map[string]sysusers.ContainerConfig{
		"kitbash-echo-unplaced": {CgroupParent: ""},
	}

	counts := h.server.Restore(ctx)
	if counts.Started != 1 || counts.Healed != 0 || counts.Failed != 0 {
		t.Fatalf("counts = %+v, want one started and nothing healed", counts)
	}
	if len(fake.Runs()) != 0 || len(fake.Removals()) != 0 {
		t.Errorf("the container was made again (%d runs, %d removals), want it started as it is",
			len(fake.Runs()), len(fake.Removals()))
	}
	// The registration is not marked as placed, because it is not.
	after, _, err := h.store.Process(ctx, id)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if after.Limits.Written() {
		t.Errorf("the limits are %+v, want no ceiling recorded for a Process that has none", after.Limits)
	}
	// One line for the owner, not one per Process.
	if want := "1 Process(es) of alice came back without a ceiling"; !strings.Contains(captured.String(), want) {
		t.Errorf("the daemon log is %q, want it to carry %q", captured.String(), want)
	}
}
