package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// Boot restore of a Package of more than one unit: the pod comes back and
// every container in it, through the same four steps, see PLAN.md 5.6.

// registeredPod writes one two unit registration straight to the store, which
// is what a daemon that has just started finds there.
func (h *harness) registeredPod(owner string) string {
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
		FanoutSecret: "fanout-secret-of-" + id,
		Limits:       store.Limits{Memory: "536870912", Ceiling: true},
		Composition: store.Composition{
			Pod: "kitbash-board-board",
			// The face first, so the face going last at a restore is the
			// order this makes and not the order it was given.
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

// stagePod puts a pod and its two containers on the fake host in the state a
// reboot leaves them: the pod is there and nothing in it is running.
func stagePod(fake *sysusers.Fake, owner, state string) {
	m := sysusers.Member{Name: owner, UID: os.Getuid(), GID: os.Getgid()}
	ctx := context.Background()
	_, _ = fake.CreatePod(ctx, m, podman.PodOptions{
		Name:    "kitbash-board-board",
		Publish: []podman.PortMapping{{HostPort: 40275, ContainerPort: 8080}},
	}, "")
	for _, unit := range []struct{ name, image string }{
		{"kitbash-board-board-web", testDigest},
		{"kitbash-board-board-cache", cacheDigest},
	} {
		_, _ = fake.CreateContainer(ctx, m, podman.RunOptions{
			Name: unit.name, Image: unit.image, Pod: "kitbash-board-board",
			Env: map[string]string{"LOG_LEVEL": "debug"},
		}, "")
		fake.SetState(unit.name, state)
	}
}

// A pod a reboot left exited is made again, whole: the pod, every container in
// it and the port the face publishes, through create, prepare, verify, start.
func TestRestoreMakesThePodAgain(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: "alice", UID: os.Getuid(), GID: os.Getgid()})
	id := h.registeredPod("alice")
	stagePod(fake, "alice", podman.StateExited)

	counts := h.server.Restore(context.Background())
	if counts.Started != 1 || counts.Failed != 0 || counts.Missing != 0 {
		t.Fatalf("counts = %+v, want one Process started", counts)
	}
	pods := fake.PodCalls()
	if len(pods) != 2 {
		t.Fatalf("the runtime was asked for %d pods, want the staged one and the one restore made", len(pods))
	}
	made := pods[1]
	if made.Options.Name != "kitbash-board-board" {
		t.Errorf("restore made the pod %s", made.Options.Name)
	}
	// The port comes back from what the face container published, not from
	// anything remembered: what a Process answers on is what its container
	// says it answers on, see routePort.
	if len(made.Options.Publish) != 1 || made.Options.Publish[0].HostPort != 40275 {
		t.Errorf("the pod restore made publishes %+v, want the port the face had", made.Options.Publish)
	}
	if made.Options.CgroupParent != "/kitbash/alice/"+id {
		t.Errorf("the pod's cgroup parent is %q, want the Process's ceiling", made.Options.CgroupParent)
	}
	if n := len(fake.PodRemovals()); n != 1 {
		t.Errorf("the old pod was removed %d times, want once before it was made again", n)
	}
	started := fake.Calls()
	if len(started) != 2 {
		t.Fatalf("restore started %d containers, want one per unit", len(started))
	}
	if started[1].Container != "kitbash-board-board-web" {
		t.Errorf("the face was started %s, want it last", started[1].Container)
	}
	// What each container was made to run comes back with it: the
	// registration does not carry an environment and the old container did.
	for _, run := range fake.Runs() {
		if run.Options.Pod != "kitbash-board-board" {
			t.Errorf("%s was made outside the pod", run.Options.Name)
		}
	}
}

// A pod whose units are all running is read where it stands: its namespaces
// are the only witness of what it holds and this daemon did not make it.
func TestRestoreLeavesARunningPodAlone(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: "alice", UID: os.Getuid(), GID: os.Getgid()})
	h.registeredPod("alice")
	stagePod(fake, "alice", podman.StateRunning)
	for _, name := range []string{"kitbash-board-board-cache", "kitbash-board-board-web"} {
		config, _ := fake.ContainerConfig(context.Background(), sysusers.Member{Name: "alice"}, name)
		config.PID = 4242
		config.State = podman.StateRunning
		fake.PinConfig(name, config)
	}

	counts := h.server.Restore(context.Background())
	if counts.Started != 1 || counts.Running != 1 {
		t.Fatalf("counts = %+v, want one Process that was already running", counts)
	}
	if n := len(fake.PodRemovals()); n != 0 {
		t.Errorf("a running pod was removed %d times", n)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Errorf("a running pod had %d containers started again", n)
	}
}

// A pod the runtime no longer has is a registration that names nothing, so it
// is unregistered and its token revoked, the same answer a container that is
// gone gets.
func TestRestoreUnregistersAPodThatIsGone(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: "alice", UID: os.Getuid(), GID: os.Getgid()})
	id := h.registeredPod("alice")

	counts := h.server.Restore(context.Background())
	if counts.Missing != 1 || counts.Started != 0 {
		t.Fatalf("counts = %+v, want one Process unregistered", counts)
	}
	if _, found, err := h.store.Process(context.Background(), id); err != nil || found {
		t.Errorf("the registration of a pod that is gone is still there: found=%v err=%v", found, err)
	}
}

// A pod that has lost one of its containers cannot be made again from what is
// left: the environment and the command of that unit were the container's.
// The Process is reported failed with something its owner can act on rather
// than made half.
func TestRestoreFailsAPodThatLostAUnit(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: "alice", UID: os.Getuid(), GID: os.Getgid()})
	id := h.registeredPod("alice")
	stagePod(fake, "alice", podman.StateExited)
	fake.Missing = map[string]bool{"kitbash-board-board-cache": true}

	counts := h.server.Restore(context.Background())
	if counts.Failed != 1 {
		t.Fatalf("counts = %+v, want one Process failed", counts)
	}
	if n := len(fake.PodRemovals()); n != 0 {
		t.Errorf("a pod that could not be made whole was removed %d times", n)
	}
	prob := h.server.processProblem(id)
	if prob.Detail == "" {
		t.Fatal("the owner is told nothing about a pod that did not come back")
	}
	if !strings.Contains(prob.Detail, "cache") {
		t.Errorf("the problem is %q, want it to name the unit that is gone", prob.Detail)
	}
}
