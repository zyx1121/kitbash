package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os/user"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// registered writes one registration straight to the store, which is what a
// daemon that has just started finds there.
func (h *harness) registered(owner, container string) string {
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
// leaves alone: one with no container name, from before M5, and one whose
// owner is no longer a member of this host.
func TestRestoreSkipsWhatItCannotStart(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: "alice", UID: 1005})

	noContainer := h.registered("alice", "")
	stranger := h.registered("carol", "kitbash-echo")

	counts := h.server.Restore(context.Background())
	if counts != (RestoreCounts{Failed: 1}) {
		t.Fatalf("counts = %+v, want one failed and nothing started", counts)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("started %+v, want nothing", fake.Calls())
	}

	// Neither registration is unregistered: only a container the runtime says
	// is gone loses its record.
	ctx := context.Background()
	for _, id := range []string{noContainer, stranger} {
		if _, found, err := h.store.Process(ctx, id); err != nil || !found {
			t.Errorf("the registration %s was removed (%t, %v)", id, found, err)
		}
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
