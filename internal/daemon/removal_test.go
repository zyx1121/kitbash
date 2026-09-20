package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/cgroups"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// Removing a member is a job kitbashd runs for itself, see issue #152. The
// tests here are about the two halves of that: the call is the length of a
// call whatever the member holds, and the job finishes whatever the caller
// did with their deadline.

// heldProcesses registers one member with the given number of Processes, each
// with a container of its own. The first is a job and the second declares a
// health path, so what the removal untracks is not one kind of thing. It
// answers the ids.
func (h *harness) heldProcesses(fake *sysusers.Fake, owner string, n int) []string {
	h.t.Helper()
	ids := make([]string, 0, n)
	for i := range n {
		_, hash, err := store.NewToken()
		if err != nil {
			h.t.Fatalf("NewToken: %v", err)
		}
		id := uuid.V7()
		container := "kitbash-held-" + strconv.Itoa(i) + "-" + owner
		port := 30000 + i
		fake.Publish(container, port)
		fake.SetState(container, podman.StateRunning)
		p := store.Process{
			ID: id, Owner: owner, Package: "/org/sensorium", Name: "held" + strconv.Itoa(i),
			Container: container, Digest: testDigest, Expose: ExposeHTTP,
			Endpoint: "http://127.0.0.1:" + strconv.Itoa(port), RegisteredAt: time.Now().UTC(),
		}
		switch i {
		case 0:
			p.Schedule = store.Schedule{Cron: "0 8 * * *"}
		case 1:
			p.Health = store.Health{HTTP: "/healthz", Interval: "30s"}
		}
		if err := h.store.RegisterProcess(context.Background(), p, hash, store.Quota{}); err != nil {
			h.t.Fatalf("RegisterProcess: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

// TestUsersRemoveAnswersBeforeItHasDoneTheWork is the deadline the issue is
// about: a member holding twenty Processes used to have them stopped inside
// the handler, which outran the caller's deadline and left the account half
// deleted. The call is now the length of a call, and what is left afterwards
// is nothing of theirs.
func TestUsersRemoveAnswersBeforeItHasDoneTheWork(t *testing.T) {
	h, fake := serveRemoving(t)
	fake.Add(sysusers.Member{Name: "alice", UID: os.Getuid(), GID: os.Getgid()})
	ids := h.heldProcesses(fake, "alice", 20)
	h.server.LoadRoutes(context.Background())
	h.server.loadJobs(context.Background())
	all, err := h.store.Processes(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Processes: %v", err)
	}
	h.server.loadProbes(context.Background(), all)
	if h.server.jobs.count() != 1 || h.server.probes.count() != 1 {
		t.Fatalf("the daemon holds %d jobs and probes %d Processes, want one of each",
			h.server.jobs.count(), h.server.probes.count())
	}
	h.seedSecret("alice", "ANTHROPIC_API_KEY", "alice-"+secretValue)
	if err := h.store.CreateApproval(context.Background(), store.Approval{
		ID: uuid.V7(), Requester: "alice", Tool: store.ToolFSWrite,
		Input: json.RawMessage(`{"path":"/org/handbook/x.md"}`), RequestedAt: time.Now().UTC(),
	}, 0); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	if n := h.server.proxy.count(); n != len(ids) {
		t.Fatalf("the routing table holds %d names, want %d", n, len(ids))
	}

	start := time.Now()
	res, body := h.do(http.MethodDelete, usersPath+"/alice", "", nil)
	answered := time.Since(start)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body %s", res.StatusCode, body)
	}
	// The handler does the refusals and the mark and nothing else, so it
	// answers in the time one database write takes. A second is far longer
	// than that and far shorter than twenty container stops.
	if answered > time.Second {
		t.Errorf("users_remove answered after %s, want the handler to start the job and return", answered)
	}
	var got removeResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if got.User != "alice" || got.State != store.Removing {
		t.Errorf("response = %+v, want alice removing", got)
	}

	removal := h.removalOf("alice")
	if removal.State != store.Removed || removal.Step != "" {
		t.Fatalf("the removal is %+v, want it removed with no step left", removal)
	}

	// Every container was stopped and removed as its owner.
	if len(fake.Stopped) != len(ids) || len(fake.RemovedContainers) != len(ids) {
		t.Errorf("the host stopped %d and removed %d containers, want %d of each",
			len(fake.Stopped), len(fake.RemovedContainers), len(ids))
	}
	// Every registration, and with it every token.
	for _, id := range ids {
		if _, found, err := h.store.Process(context.Background(), id); err != nil || found {
			t.Fatalf("the Process %s is still registered (%t, %v)", id, found, err)
		}
	}
	// Every name, job and probe.
	if n := h.server.proxy.count(); n != 0 {
		t.Errorf("the routing table holds %d names of a member this host no longer has", n)
	}
	if n := h.server.jobs.count(); n != 0 {
		t.Errorf("the daemon holds %d jobs of a member this host no longer has", n)
	}
	if n := h.server.probes.count(); n != 0 {
		t.Errorf("the daemon probes %d Processes of a member this host no longer has", n)
	}
	// Their approvals and their secrets.
	left, err := h.store.Approvals(context.Background(), "alice", "")
	if err != nil || len(left) != 0 {
		t.Errorf("approvals = %d, %v, want none left", len(left), err)
	}
	if _, err := os.Stat(filepath.Join(h.secretsDir, "alice")); !os.IsNotExist(err) {
		t.Errorf("the secrets of a removed member are still there: %v", err)
	}
	// And the account.
	if len(fake.Removed) != 1 || fake.Removed[0] != "alice" {
		t.Errorf("the host removed %v, want alice", fake.Removed)
	}
}

// TestASecondUsersRemoveDuringTheJobIsTheSameAnswer: the call describes the
// state the caller asked for, so a second one while the job runs is 202 with
// the same state rather than a second job over the same containers.
func TestASecondUsersRemoveDuringTheJobIsTheSameAnswer(t *testing.T) {
	h, fake := serveRemoving(t)
	fake.Add(sysusers.Member{Name: "alice", UID: os.Getuid(), GID: os.Getgid()})
	ids := h.heldProcesses(fake, "alice", 3)
	gate := make(chan struct{})
	fake.StopGate = gate

	res, body := h.do(http.MethodDelete, usersPath+"/alice", "", nil)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202, body %s", res.StatusCode, body)
	}
	waitFor(t, "the job to reach the containers", func() bool {
		return h.server.isRemoving("alice")
	})

	res, body = h.do(http.MethodDelete, usersPath+"/alice", "", nil)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("second status = %d, want 202, body %s", res.StatusCode, body)
	}
	var got removeResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if got.User != "alice" || got.State != store.Removing {
		t.Errorf("the second call answered %+v, want alice removing", got)
	}
	close(gate)
	removal := h.removalOf("alice")
	if removal.State != store.Removed {
		t.Errorf("the removal is %+v, want it removed once the containers went", removal)
	}
	// One job ran, not two: each container was stopped once.
	if len(fake.Stopped) != len(ids) {
		t.Errorf("the host stopped %d containers, want %d, so a second job ran over the same ones",
			len(fake.Stopped), len(ids))
	}
}

// A removal that has already deleted the account is still a removal: the
// second call is answered from the mark and not from the host, so an admin
// who asks again mid job reads the state rather than a 404 about a member
// they have just removed.
func TestASecondUsersRemoveAfterTheAccountIsGoneIsStillTheState(t *testing.T) {
	h, _ := serveRemoving(t)
	// The job is past the account step and has not finished: the host no
	// longer has the name and the mark is still there.
	h.server.markRemoving("alice")
	t.Cleanup(func() { h.server.doneRemoving("alice") })

	res, body := h.do(http.MethodDelete, usersPath+"/alice", "", nil)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body %s", res.StatusCode, body)
	}
	var got removeResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if got.User != "alice" || got.State != store.Removing {
		t.Errorf("response = %+v, want alice removing", got)
	}
}

// TestAMemberBeingRemovedTakesNoNewWork: a container started while the job is
// stopping the containers it read is a container the job has already walked
// past, and a registration written after it deleted them is a Process with a
// live token owned by an account that is about to be gone.
func TestAMemberBeingRemovedTakesNoNewWork(t *testing.T) {
	h, fake := serveUsers(t, false)
	ids := h.heldProcesses(fake, h.user, 1)
	gate := make(chan struct{})
	fake.StopGate = gate
	h.server.startRemoval(h.user, "admin")
	waitFor(t, "the job to reach the containers", func() bool { return h.server.isRemoving(h.user) })

	res, body := h.do(http.MethodPost, processesPath+"/"+ids[0]+"/start", "", nil)
	if res.StatusCode != http.StatusConflict {
		t.Errorf("starting a Process = %d, want 409, body %s", res.StatusCode, body)
	}
	_, res, body = h.register(registration(""))
	if res.StatusCode != http.StatusConflict {
		t.Errorf("registering a Process = %d, want 409, body %s", res.StatusCode, body)
	}

	close(gate)
	waitFor(t, "the removal to finish", func() bool { return !h.server.isRemoving(h.user) })
}

// TestARestartResumesARemoval: the mark is a row in the store, so a daemon
// that stopped in the middle of a removal takes it up again at its next start
// rather than leaving half a member behind for good.
func TestARestartResumesARemoval(t *testing.T) {
	h, fake := serveRemoving(t)
	fake.Add(sysusers.Member{Name: "alice", UID: os.Getuid(), GID: os.Getgid()})
	ids := h.heldProcesses(fake, "alice", 2)
	// The daemon that stopped got as far as the mark and no further.
	if _, err := h.store.BeginRemoval(context.Background(), "alice", time.Now().UTC()); err != nil {
		t.Fatalf("BeginRemoval: %v", err)
	}

	resumed := New(h.store, Options{
		Admin:      func(*user.User) (bool, error) { return true, nil },
		Users:      fake,
		Runner:     fake,
		Cgroups:    &cgroups.Fake{Base: filepath.Join(t.TempDir(), "cgroup")},
		EnvDir:     filepath.Join(t.TempDir(), "env"),
		SecretsDir: h.secretsDir,
	})
	t.Cleanup(resumed.Close)
	resumed.Restore(context.Background())

	waitFor(t, "the resumed removal to finish", func() bool {
		removal, held, err := h.store.Removal(context.Background(), "alice")
		return err == nil && held && removal.State != store.Removing
	})
	removal, _, err := h.store.Removal(context.Background(), "alice")
	if err != nil || removal.State != store.Removed {
		t.Errorf("the resumed removal is %+v, %v, want it removed", removal, err)
	}
	if len(fake.Removed) != 1 || fake.Removed[0] != "alice" {
		t.Errorf("the host removed %v, want alice", fake.Removed)
	}
	for _, id := range ids {
		if _, found, err := h.store.Process(context.Background(), id); err != nil || found {
			t.Errorf("the Process %s survived the resumed removal (%t, %v)", id, found, err)
		}
	}
	// The boot did not start their containers again on the way past: the job
	// is what those containers belong to.
	if len(fake.Started) != 0 {
		t.Errorf("restore started %+v of a member it was removing", fake.Started)
	}
}

// TestUsersMeIsNotFoundOnceTheAccountIsGone: a session open across a removal
// keeps its uid for as long as it runs, and this host has answered the removal
// already, so the honest answer about the member is that there is none.
func TestUsersMeIsNotFoundOnceTheAccountIsGone(t *testing.T) {
	h, fake := serveUsers(t, false)
	// The caller is the member, and their account is gone: their removal is
	// over and the host no longer knows the name.
	fake.Remove(context.Background(), h.user)
	if err := h.store.FinishRemoval(context.Background(), h.user, store.Removed, "",
		"/org/.archive/"+h.user, time.Now().UTC()); err != nil {
		t.Fatalf("FinishRemoval: %v", err)
	}
	if _, err := h.store.BeginRemoval(context.Background(), h.user, time.Now().UTC()); err != nil {
		t.Fatalf("BeginRemoval: %v", err)
	}
	if err := h.store.FinishRemoval(context.Background(), h.user, store.Removed, "",
		"/org/.archive/"+h.user, time.Now().UTC()); err != nil {
		t.Fatalf("FinishRemoval: %v", err)
	}

	res, body := h.do(http.MethodGet, usersPath+"/me", "", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("users_me status = %d, want 404, body %s", res.StatusCode, body)
	}
}

// A member the host does not describe but never removed is still the peer's
// identity, which is the rule users_me had before removals were a state: an
// account the group database says nothing about still gets an answer.
func TestUsersMeStillAnswersForAnAccountNobodyRemoved(t *testing.T) {
	h, fake := serveUsers(t, false)
	fake.Remove(context.Background(), h.user)

	res, body := h.do(http.MethodGet, usersPath+"/me", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("users_me status = %d, want 200, body %s", res.StatusCode, body)
	}
	var got meResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if got.User != h.user {
		t.Errorf("users_me answered %+v, want the peer", got)
	}
}

// A name removed once and created again is a member of its own, so what this
// host remembers about the removal of the account that held it before is not
// about them.
func TestCreatingAMemberAgainForgetsTheOldRemoval(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: "alice", UID: 1005})
	h.removeMember("alice")

	res, body := h.postJSON(http.MethodPost, usersPath, userRequest{
		Name: "alice", SSHKey: publicKey("alice@laptop"),
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("users_create status = %d, body %s", res.StatusCode, body)
	}
	if _, held, err := h.store.Removal(context.Background(), "alice"); err != nil || held {
		t.Errorf("the removal of the old account is still remembered (%t, %v)", held, err)
	}
}

// serveRemoving is a daemon with a domain, an admin caller and a fake host,
// which is what a removal needs: the names of a removed member come off the
// routing table with them.
func serveRemoving(t *testing.T) (*harness, *sysusers.Fake) {
	t.Helper()
	fake := sysusers.NewFake()
	h := serveWith(t, Options{
		Admin:         func(*user.User) (bool, error) { return true, nil },
		Users:         fake,
		Runner:        fake,
		Domain:        testDomain,
		TLS:           TLSGateway,
		CertDir:       filepath.Join(t.TempDir(), "certs"),
		PublicAddress: testPublicAddress,
		Resolver:      newZone(),
	})
	fake.Add(sysusers.Member{Name: h.user, Admin: true, UID: os.Getuid(), GID: os.Getgid()})
	return h, fake
}
