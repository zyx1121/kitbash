package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	token, hash, err := store.NewToken()
	if err != nil {
		h.t.Fatalf("NewToken: %v", err)
	}
	id := uuid.V7()
	if err := h.store.RegisterProcess(context.Background(), store.Process{
		ID:        id,
		Owner:     owner,
		Package:   "/home/" + owner + "/echo",
		Name:      "echo",
		Container: container,
		Digest:    "sha256:" + "ab12cd34" + "00000000000000000000000000000000000000000000000000000000",
		Expose:    ExposeNone,
		Limits:    limits,
		// The secret is part of the record rather than minted with the token:
		// it is kept in the clear, so a Process that is created again is given
		// the one the registration already carries.
		FanoutSecret: "fanout-secret-of-" + id,
		RegisteredAt: time.Now().UTC(),
	}, hash, store.Quota{}); err != nil {
		h.t.Fatalf("RegisterProcess: %v", err)
	}
	// The store keeps only the hash, so the token itself is kept here: a test
	// that has to prove a token was not revoked needs the one it wrote.
	h.tokens[id] = token
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

// registeredByKit writes one registration of a Process a run kit owns: no
// container name, because the Process runs wherever the kit put it, and the
// runner that says so, see PLAN.md section 3.
func (h *harness) registeredByKit(owner, runner string) string {
	h.t.Helper()
	token, hash, err := store.NewToken()
	if err != nil {
		h.t.Fatalf("NewToken: %v", err)
	}
	id := uuid.V7()
	if err := h.store.RegisterProcess(context.Background(), store.Process{
		ID:           id,
		Owner:        owner,
		Package:      "/home/" + owner + "/trainer",
		Name:         "trainer",
		Runner:       runner,
		Digest:       "sha256:" + "ab12cd34" + "00000000000000000000000000000000000000000000000000000000",
		Expose:       ExposeNone,
		FanoutSecret: "fanout-secret-of-" + id,
		RegisteredAt: time.Now().UTC(),
	}, hash, store.Quota{}); err != nil {
		h.t.Fatalf("RegisterProcess: %v", err)
	}
	h.tokens[id] = token
	return id
}

// TestRestoreLeavesARunnerOwnedRegistrationAlone is the guard that keeps the
// run hook working across a reboot. A Process a run kit owns carries no
// container name, which is exactly the shape restore unregisters as a legacy
// row: without the runner check it would be deleted at every boot, its token
// revoked and its fan out dropped, while the Process kept running wherever the
// kit put it.
func TestRestoreLeavesARunnerOwnedRegistrationAlone(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: "alice", UID: 1005})

	owned := h.registeredByKit("alice", "/org/pve-runner")
	ordinary := h.registered("alice", "kitbash-echo-one")

	counts := h.server.Restore(context.Background())
	if counts != (RestoreCounts{Started: 1, Kit: 1}) {
		t.Fatalf("counts = %+v, want the ordinary Process started and the kit owned one counted", counts)
	}

	ctx := context.Background()
	p, found, err := h.store.Process(ctx, owned)
	if err != nil || !found {
		t.Fatalf("the registration of a Process a run kit owns was removed (%t, %v)", found, err)
	}
	if p.Runner != "/org/pve-runner" {
		t.Errorf("the registration came back as %+v, want the runner it was written with", p)
	}
	if _, found, err := h.store.Process(ctx, ordinary); err != nil || !found {
		t.Errorf("the ordinary Process is no longer registered (%t, %v)", found, err)
	}
	// Nothing of it was started here, and no cgroup was made for a Process
	// this host does not run.
	for _, call := range fake.Calls() {
		if call.Container == "" {
			t.Errorf("restore started %+v, want nothing for a Process a run kit owns", call)
		}
	}
	if len(fake.Calls()) != 1 {
		t.Fatalf("restore started %+v, want only the ordinary Process", fake.Calls())
	}
	for _, placed := range h.cgroups.Placed() {
		if placed.ID == owned {
			t.Errorf("a cgroup was made for %s, which runs wherever its kit put it", owned)
		}
	}
}

// TestRestoreNeverHealsARunnerOwnedRegistration is the same guard against the
// other end of restore: a registration with no limits and no ceiling is the
// shape the heal acts on, and a Process a run kit owns has neither. It must
// not be read back, renamed aside or created again, because the container the
// heal would work on is not on this host at all.
func TestRestoreNeverHealsARunnerOwnedRegistration(t *testing.T) {
	h, fake, owner, _ := legacyHarness(t)
	owned := h.registeredByKit(owner, "/org/pve-runner")
	// The runtime refuses every start in this harness, which is what makes the
	// legacy Process healable. The kit owned one never reaches the runtime.
	counts := h.server.Restore(context.Background())
	if counts.Kit != 1 {
		t.Fatalf("counts = %+v, want the kit owned registration counted as one", counts)
	}
	for _, rename := range fake.Renames() {
		if strings.Contains(rename.From, "trainer") {
			t.Errorf("the heal renamed %+v, which belongs to a run kit", rename)
		}
	}
	for _, run := range fake.Runs() {
		if run.Options.Name == "" || strings.Contains(run.Options.Name, "trainer") {
			t.Errorf("the heal created %+v again, which belongs to a run kit", run.Options)
		}
	}
	if _, found, err := h.store.Process(context.Background(), owned); err != nil || !found {
		t.Fatalf("the registration of a Process a run kit owns was removed (%t, %v)", found, err)
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

// legacyHarness is one member and one Process registered before limits were
// recorded, with an image in the member's store and a container whose cgroup
// parent is the member's own cgroup: what a host that has been upgraded across
// the release that gave each Process a ceiling actually holds.
//
// The owner is the user this test runs as, because the heal writes the
// environment file of the new container and chowns it to the owner, which only
// that user can be given.
func legacyHarness(t *testing.T) (*harness, *sysusers.Fake, string, string) {
	t.Helper()
	h, fake := serveUsers(t, true)
	owner := h.user
	fake.Add(sysusers.Member{Name: owner, UID: os.Getuid(), GID: os.Getgid()})
	id := h.registered(owner, "kitbash-echo-legacy")
	p, found, err := h.store.Process(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("Process: found %t, err %v", found, err)
	}
	fake.AddImage(owner, p.Digest, 1024, nil)
	fake.Configs = map[string]sysusers.ContainerConfig{
		"kitbash-echo-legacy": legacyConfig(owner, p.Digest),
	}
	// What the host does today: the member's runtime cannot make the
	// container's cgroup under a directory that belongs to root.
	fake.StartErr = errors.New(
		"crun: create `/sys/fs/cgroup/kitbash/loki/libpod-fd4b88f9`: Permission denied: OCI permission denied")
	return h, fake, owner, id
}

// legacyConfig is what the runtime holds for a container created before
// kitbashd gave each Process a cgroup of its own: its parent is the member's
// cgroup, which belongs to root, and starting it there is permission denied at
// every boot.
func legacyConfig(owner, digest string) sysusers.ContainerConfig {
	return sysusers.ContainerConfig{
		CgroupParent: cgroups.MemberParent(owner),
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
	captured := captureDaemonLog(t)
	h, fake, owner, id := legacyHarness(t)
	ctx := context.Background()
	before, _, err := h.store.Process(ctx, id)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

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
	// The old container cannot be moved, so it is renamed aside, the new one
	// takes its name, and only then is the old one removed.
	aside := "kitbash-echo-legacy" + AsideSuffix
	renames := fake.Renames()
	if len(renames) != 1 || renames[0].From != "kitbash-echo-legacy" || renames[0].To != aside {
		t.Fatalf("the renames are %+v, want the old container moved aside", renames)
	}
	removals := fake.Removals()
	if len(removals) != 1 || removals[0].Container != aside || !removals[0].Force {
		t.Fatalf("the removals are %+v, want the container that was replaced removed", removals)
	}
	runs := fake.Runs()
	if len(runs) != 1 {
		t.Fatalf("the containers created are %+v, want the legacy one made again", runs)
	}
	run := runs[0]
	if run.Member != owner || run.Options.Name != "kitbash-echo-legacy" || run.Options.Image != before.Digest {
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
	// Container level limits are not carried over: the ceiling holds them.
	if run.Options.Memory != "" || run.Options.CPUs != "" || run.Options.PidsLimit != 0 {
		t.Errorf("the command line carries limits %+v, want none, the ceiling holds them", run.Options)
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
	// The fan out secret is kept, not minted again: it is stored in the clear,
	// so the new container is given the one the registration carries.
	if !strings.Contains(run.Env, EnvFanoutSecret+"="+before.FanoutSecret) {
		t.Errorf("the environment is %q, want the fan out secret of the registration", run.Env)
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
	if healed.FanoutSecret != before.FanoutSecret {
		t.Errorf("the fan out secret changed, so the container was given one the registration does not carry")
	}
	want := "was registered without limits; it now runs under an unlimited ceiling, run it again to write limits"
	if !strings.Contains(captured.String(), want) {
		t.Errorf("the daemon log is %q, want it to carry %q", captured.String(), want)
	}
}

// The second boot is the ordinary one: the registration says the ceiling is
// there, so restore starts the container it has rather than making it again.
func TestRestoreTakesTheOrdinaryPathAfterAHeal(t *testing.T) {
	h, fake, owner, id := legacyHarness(t)
	ctx := context.Background()

	if counts := h.server.Restore(ctx); counts.Healed != 1 {
		t.Fatalf("the first boot healed %+v, want one", counts)
	}
	// The container the heal made is under the ceiling, so it starts.
	fake.StartErr = nil
	fake.Configs["kitbash-echo-legacy"] = sysusers.ContainerConfig{
		CgroupParent: cgroups.Parent(owner, id),
	}

	counts := h.server.Restore(ctx)
	if counts.Started != 1 || counts.Healed != 0 || counts.Failed != 0 {
		t.Fatalf("the second boot is %+v, want one started and nothing healed", counts)
	}
	if len(fake.Runs()) != 1 || len(fake.Renames()) != 1 {
		t.Errorf("the second boot made the container again (%d runs, %d renames), want the first boot's only",
			len(fake.Runs()), len(fake.Renames()))
	}
	// Two starts, one per boot: the heal starts the container it made, once it
	// has read what that container holds, and the second boot starts the same
	// one again. Neither made it a second time.
	started := fake.Calls()
	if len(started) != 2 {
		t.Errorf("the containers started are %+v, want one start per boot", started)
	}
	for _, call := range started {
		if call.Container != "kitbash-echo-legacy" {
			t.Errorf("the containers started are %+v, want the healed one", started)
		}
	}
}

// A registration that carries limits was written by a release that placed its
// container, so a start of it that fails is not a legacy cgroup parent and
// nothing is taken apart to find out.
func TestRestoreDoesNotHealAProcessThatHasLimits(t *testing.T) {
	h, fake, owner, _ := legacyHarness(t)
	limited := h.registeredWith(owner, "kitbash-echo-limited",
		store.Limits{Memory: "512Mi", CPU: "0.5", Pids: 512})
	ctx := context.Background()
	p, _, err := h.store.Process(ctx, limited)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	// Its container looks exactly like the legacy one, so only the
	// registration tells them apart.
	fake.Configs["kitbash-echo-limited"] = legacyConfig(owner, p.Digest)

	counts := h.server.Restore(ctx)
	if counts.Failed != 1 {
		t.Fatalf("counts = %+v, want the Process with limits failed", counts)
	}
	for _, call := range fake.Renames() {
		if call.From == "kitbash-echo-limited" {
			t.Errorf("the Process with limits was healed: %+v", call)
		}
	}
	for _, call := range fake.Runs() {
		if call.Options.Name == "kitbash-echo-limited" {
			t.Errorf("the container of the Process with limits was made again: %+v", call.Options)
		}
	}
	if prob := h.server.processProblem(limited); !strings.Contains(prob.Detail, "kitbash-echo-limited") {
		t.Errorf("the problem is %+v, want the ordinary report of a start that failed", prob)
	}
}

// The heal is for one cgroup parent and one only: the member's own cgroup,
// which is the directory a rootless runtime is refused by. Every other parent
// is a container restore leaves where it is.
func TestRestoreDoesNotHealAContainerUnderAnotherCgroup(t *testing.T) {
	for name, parent := range map[string]string{
		"no cgroup parent at all":  "",
		"already under a ceiling":  cgroups.Parent("someone", "01a08693-5c34-7748-a2d9-a35fd5bc07cd"),
		"another member's cgroup":  cgroups.MemberParent("bob"),
		"a cgroup of its own name": "/kitbash",
	} {
		t.Run(name, func(t *testing.T) {
			h, fake, owner, id := legacyHarness(t)
			config := fake.Configs["kitbash-echo-legacy"]
			config.CgroupParent = parent
			fake.Configs["kitbash-echo-legacy"] = config

			counts := h.server.Restore(context.Background())
			if counts.Failed != 1 || counts.Healed != 0 {
				t.Fatalf("counts = %+v, want one failed and nothing healed", counts)
			}
			if len(fake.Renames()) != 0 || len(fake.Runs()) != 0 || len(fake.Removals()) != 0 {
				t.Errorf("the container was touched (%d renames, %d runs, %d removals), want it left where it is",
					len(fake.Renames()), len(fake.Runs()), len(fake.Removals()))
			}
			after, _, err := h.store.Process(context.Background(), id)
			if err != nil {
				t.Fatalf("Process: %v", err)
			}
			if after.Limits.Written() {
				t.Errorf("the limits are %+v, want no ceiling recorded for a container that is not under one", after.Limits)
			}
			_ = owner
		})
	}
}

// A host that could not make the ceiling heals nothing: there would be nowhere
// to create the container, and a container taken apart for that is a Process
// lost.
func TestRestoreDoesNotHealWhenTheCeilingCannotBeMade(t *testing.T) {
	h, fake, _, id := legacyHarness(t)
	h.cgroups.ProcessErr = errors.New("cgroups: this host does not delegate cgroups")

	counts := h.server.Restore(context.Background())
	if counts.Failed != 1 || counts.Healed != 0 {
		t.Fatalf("counts = %+v, want one failed and nothing healed", counts)
	}
	if len(fake.Renames()) != 0 || len(fake.Runs()) != 0 || len(fake.Removals()) != 0 {
		t.Errorf("the container was touched (%d renames, %d runs, %d removals), want it left where it is",
			len(fake.Renames()), len(fake.Runs()), len(fake.Removals()))
	}
	if prob := h.server.processProblem(id); prob.Detail == "" {
		t.Errorf("the Process is reported with no problem, want the owner told it did not come back")
	}
}

// A start that ran out of its budget says nothing about the cgroup parent: the
// container may yet be coming up, so it is reported failed and left alone.
func TestRestoreDoesNotHealAStartThatDidNotFinish(t *testing.T) {
	for name, staged := range map[string]error{
		"the runtime ran out of its budget": fmt.Errorf("%w: podman start after 60s", sysusers.ErrTimeout),
		"the daemon was stopping":           fmt.Errorf("starting: %w", context.Canceled),
		"the budget of the call passed":     fmt.Errorf("starting: %w", context.DeadlineExceeded),
	} {
		t.Run(name, func(t *testing.T) {
			h, fake, _, id := legacyHarness(t)
			fake.StartErr = staged

			counts := h.server.Restore(context.Background())
			if counts.Failed != 1 || counts.Healed != 0 {
				t.Fatalf("counts = %+v, want one failed and nothing healed", counts)
			}
			if len(fake.Renames()) != 0 || len(fake.Runs()) != 0 || len(fake.Removals()) != 0 {
				t.Errorf("the container was touched (%d renames, %d runs, %d removals), want it left alone",
					len(fake.Renames()), len(fake.Runs()), len(fake.Removals()))
			}
			if prob := h.server.processProblem(id); prob.Detail == "" {
				t.Errorf("the Process is reported with no problem, want the owner told it did not come back")
			}
		})
	}
}

// A container the runtime will not describe is not one to take apart: the heal
// answers nothing and the ordinary report of a start that failed stands.
func TestRestoreDoesNotHealAContainerTheRuntimeCannotDescribe(t *testing.T) {
	h, fake, _, id := legacyHarness(t)
	fake.ConfigErr = fmt.Errorf("%w: kitbash-echo-legacy", sysusers.ErrNoContainer)

	counts := h.server.Restore(context.Background())
	if counts.Failed != 1 || counts.Healed != 0 || counts.Missing != 0 {
		t.Fatalf("counts = %+v, want one failed and nothing healed", counts)
	}
	if len(fake.Renames()) != 0 || len(fake.Runs()) != 0 {
		t.Errorf("the container was touched (%d renames, %d runs), want it left alone",
			len(fake.Renames()), len(fake.Runs()))
	}
	if _, found, err := h.store.Process(context.Background(), id); err != nil || !found {
		t.Errorf("the registration went (found %t, err %v), want it kept", found, err)
	}
	// The owner reads the ordinary report of a start that failed, not one
	// about a ceiling nothing tried to give it.
	prob := h.server.processProblem(id)
	if !strings.Contains(prob.Detail, "did not start kitbash-echo-legacy") {
		t.Errorf("the problem is %+v, want the ordinary report of a start that failed", prob)
	}
}

// TestRestoreKeepsTheRegistrationWhenTheNewContainerCannotBeCreated is the
// invariant that makes the heal safe to run at boot: the container being
// replaced is renamed rather than removed, so a creation that fails puts the
// name back. The registration names a container that exists all along, and the
// next boot reports the Process failed with a reason rather than unregistering
// it as one whose container is gone.
func TestRestoreKeepsTheRegistrationWhenTheNewContainerCannotBeCreated(t *testing.T) {
	h, fake, _, id := legacyHarness(t)
	fake.RunErr = errors.New("podman run: exit status 125")
	ctx := context.Background()
	before, _, err := h.store.Process(ctx, id)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	counts := h.server.Restore(ctx)
	if counts.Failed != 1 || counts.Healed != 0 {
		t.Fatalf("counts = %+v, want one failed and nothing healed", counts)
	}
	// The name went aside and came back, and nothing was removed.
	aside := "kitbash-echo-legacy" + AsideSuffix
	renames := fake.Renames()
	if len(renames) != 2 || renames[0].To != aside || renames[1].To != "kitbash-echo-legacy" {
		t.Fatalf("the renames are %+v, want the name moved aside and put back", renames)
	}
	if len(fake.Removals()) != 0 {
		t.Errorf("the removals are %+v, want the container that holds this Process kept", fake.Removals())
	}
	// The registration is untouched: it still names that container, it still
	// carries no ceiling, and the token the old container holds still works.
	after, found, err := h.store.Process(ctx, id)
	if err != nil || !found {
		t.Fatalf("the registration went (found %t, err %v)", found, err)
	}
	if after.Limits.Written() {
		t.Errorf("the limits are %+v, want no ceiling recorded for a heal that did not finish", after.Limits)
	}
	if after.FanoutSecret != before.FanoutSecret {
		t.Errorf("the fan out secret changed on a heal that did not finish")
	}
	held, found, err := h.store.ProcessByToken(ctx, h.tokens[id])
	if err != nil || !found || held.ID != id {
		t.Errorf("the token the old container holds was revoked by a heal that did not finish (found %t, err %v)",
			found, err)
	}

	// The next boot finds the container where it was: the Process is failed
	// with a reason, not missing, and the registration is still there.
	next := h.server.Restore(ctx)
	if next.Failed != 1 || next.Missing != 0 {
		t.Fatalf("the next boot is %+v, want the Process failed and not missing", next)
	}
	if _, found, err := h.store.Process(ctx, id); err != nil || !found {
		t.Errorf("the registration went at the next boot (found %t, err %v)", found, err)
	}
	if prob := h.server.processProblem(id); prob.Detail == "" || prob.Fix == "" {
		t.Errorf("the problem is %+v, want the owner told why it did not come back", prob)
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
	if counts := h.server.Restore(context.Background()); counts.Started != 1 {
		t.Fatalf("counts = %+v, want the Process started", counts)
	}
	if prob := h.server.processProblem(id); prob.Detail != "" {
		t.Errorf("the problem is %+v, want it cleared once the Process runs", prob)
	}
}

// A heal that cannot finish leaves the container where it is: an image the
// member no longer holds is a Process that cannot be created again, and moving
// its container aside for that would gain nothing.
func TestRestoreKeepsAContainerItCannotCreateAgain(t *testing.T) {
	h, fake, owner, id := legacyHarness(t)
	// The image is not in the member's store, which is the one failure that
	// would leave the Process with no container at all.
	fake.DropImages(owner)

	if counts := h.server.Restore(context.Background()); counts.Failed != 1 || counts.Healed != 0 {
		t.Fatalf("counts = %+v, want one failed and nothing healed", counts)
	}
	if len(fake.Renames()) != 0 || len(fake.Removals()) != 0 || len(fake.Runs()) != 0 {
		t.Errorf("the container was touched (%d renames, %d removals, %d runs), want it left where it is",
			len(fake.Renames()), len(fake.Removals()), len(fake.Runs()))
	}
	prob := h.server.processProblem(id)
	if !strings.Contains(prob.Detail, "registered before kitbashd gave each Process a cgroup of its own") || prob.Fix == "" {
		t.Errorf("the problem is %+v, want the owner told why it did not come back", prob)
	}
}

// A Process that came back without a ceiling is counted once for its owner
// rather than named once each: a host with a dozen of them would otherwise
// print a dozen lines at every boot.
func TestRestoreCountsProcessesThatCameBackWithoutACeiling(t *testing.T) {
	h, fake := serveUsers(t, true)
	captured := captureDaemonLog(t)
	fake.Add(sysusers.Member{Name: "alice", UID: 1005})

	h.registered("alice", "kitbash-echo-one")
	h.registered("alice", "kitbash-echo-two")
	h.registeredWith("alice", "kitbash-echo-limited", store.Limits{Pids: 512})

	if counts := h.server.Restore(context.Background()); counts.Started != 3 {
		t.Fatalf("counts = %+v, want three started", counts)
	}
	if len(fake.Runs()) != 0 || len(fake.Renames()) != 0 {
		t.Errorf("a container that started was made again (%d runs, %d renames), want none",
			len(fake.Runs()), len(fake.Renames()))
	}
	// Two of the three, not the one that carries limits.
	if want := "2 Process(es) of alice came back without a ceiling"; !strings.Contains(captured.String(), want) {
		t.Errorf("the daemon log is %q, want it to carry %q", captured.String(), want)
	}
}
