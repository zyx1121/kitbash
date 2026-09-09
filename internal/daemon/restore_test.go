package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os/user"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/cgroups"
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
	// The cgroup filesystem does not survive a reboot, so the cgroup of every
	// Process is created again and the child is placed in its leaf: a
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
