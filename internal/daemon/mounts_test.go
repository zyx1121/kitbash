package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/cgroups"
	"github.com/zyx1121/kitbash/internal/mounts"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// testPID is the process id the fake runtime gives every prepared container,
// which is what the daemon reads its mount namespace through.
const testPID = 4242

// stageNamespace writes what one container holds at one target into a tree
// standing in for /proc. /proc/<pid>/root is a magic link the kernel resolves
// in the container's own mount namespace; here it is an ordinary link to the
// folder that is mounted there, so the daemon's own stat and its own comparison
// of device and inode are what run, against a real folder on a real
// filesystem.
func stageNamespace(procRoot string, pid int, target, folder string) error {
	at := filepath.Join(procRoot, strconv.Itoa(pid), "root", target)
	if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(at); err != nil {
		return err
	}
	return os.Symlink(folder, at)
}

// filesHost is a daemon whose members are a fake host with real folders behind
// them: the caller's home and an /org of the test's own, each holding one
// visible folder, so the resolution kitbashd makes as root has a tree to make
// it in. The caller's own uid is what the folders are owned by, which is what
// makes "owned by the member" a question this test can answer.
//
// The fourth thing it answers is the tree standing in for /proc, which is where
// a test says what a prepared container is actually holding.
func filesHost(t *testing.T) (*harness, *sysusers.Fake, string, string) {
	h, fake, home, org, _ := filesHostWithProc(t)
	return h, fake, home, org
}

func filesHostWithProc(t *testing.T) (*harness, *sysusers.Fake, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home", "member")
	org := filepath.Join(dir, "org")
	procRoot := filepath.Join(dir, "proc")
	visible := func(root, name string) {
		t.Helper()
		folder := filepath.Join(root, name)
		if err := os.MkdirAll(folder, 0o755); err != nil {
			t.Fatalf("creating %s: %v", folder, err)
		}
		body := "name: " + name + "\ndescription: >-\n  The folder " + name + ", visible to the surface.\n"
		if err := os.WriteFile(filepath.Join(folder, "kitbash.yaml"), []byte(body), 0o644); err != nil {
			t.Fatalf("writing the manifest of %s: %v", folder, err)
		}
	}
	visible(home, "notes")
	visible(org, "handbook")

	fake := sysusers.NewFake()
	fake.NextPID = testPID
	fake.ProcRoot = procRoot
	h := serveWith(t, Options{
		Users:           fake,
		Runner:          fake,
		OrgRoot:         org,
		ProcRoot:        procRoot,
		ProcessEndpoint: "http://127.0.0.1:4318",
	})
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid(), Home: home})
	return h, fake, home, org, procRoot
}

// registerWithMounts sends one registration carrying the mounts given and
// answers the response, so a test can read the problem or the token.
func (h *harness) registerWithMounts(id string, declared []mounts.Declared) (*http.Response, []byte) {
	h.t.Helper()
	return h.postJSON(http.MethodPost, processesPath, processRequest{
		ID:        id,
		Package:   "/home/" + h.user + "/reader",
		Name:      "reader",
		Container: "kitbash-reader-reader",
		Digest:    testDigest,
		Expose:    ExposeNone,
		Mounts:    declared,
	})
}

// TestRegistrationResolvesTheMountsAsRoot is the registration half of the
// check: what the member declared is resolved by the daemon and what is stored
// is what it resolved, not what was claimed.
func TestRegistrationResolvesTheMountsAsRoot(t *testing.T) {
	h, _, home, org := filesHost(t)
	id := uuid.V7()
	res, body := h.registerWithMounts(id, []mounts.Declared{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes"},
		{Source: filepath.Join(org, "handbook"), Target: "/files/handbook", Mode: "ro"},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the registration answered %d: %s", res.StatusCode, body)
	}
	p, found, err := h.store.Process(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("Process: %v, found %v", err, found)
	}
	if len(p.Mounts) != 2 {
		t.Fatalf("the registration holds %+v, want two mounts", p.Mounts)
	}
	if p.Mounts[0].Source != filepath.Join(home, "notes") || p.Mounts[0].Mode != mounts.ModeRO {
		t.Errorf("the first mount is %+v, want the notes folder read only", p.Mounts[0])
	}
	if p.Mounts[1].Target != "/files/handbook" {
		t.Errorf("the second mount is %+v, want it at /files/handbook", p.Mounts[1])
	}
}

// TestRegistrationRefusesAMountTheMemberMayNotHave is the same check saying
// no. Every case of the rule set is covered in internal/mounts; what this
// proves is that the daemon runs it at all, answers the problem's own status
// and writes no registration.
func TestRegistrationRefusesAMountTheMemberMayNotHave(t *testing.T) {
	h, _, home, org := filesHost(t)
	elsewhere := filepath.Join(filepath.Dir(home), "other", "notes")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatalf("creating the other member's folder: %v", err)
	}
	for _, c := range []struct {
		name  string
		mount mounts.Declared
		slug  string
	}{
		{"another member's home", mounts.Declared{Source: elsewhere, Target: "/files/theirs"}, problem.SlugNotPermitted},
		{"/org read write", mounts.Declared{Source: filepath.Join(org, "handbook"), Target: "/files/handbook", Mode: "rw"}, problem.SlugNotPermitted},
		{"a folder with no manifest", mounts.Declared{Source: home, Target: "/files/home"}, problem.SlugNotVisible},
		{"a target under /etc", mounts.Declared{Source: filepath.Join(home, "notes"), Target: "/etc/notes"}, problem.SlugBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			id := uuid.V7()
			res, body := h.registerWithMounts(id, []mounts.Declared{c.mount})
			prob := h.problemOf(res, body)
			if prob.Slug() != c.slug {
				t.Fatalf("the registration answered %s (%s), want %s", prob.Slug(), prob.Detail, c.slug)
			}
			if _, found, err := h.store.Process(context.Background(), id); err != nil || found {
				t.Errorf("a refused registration was written: found %v, %v", found, err)
			}
		})
	}
}

// TestRegistrationRefusesMoreThanFourMounts is the bound held before anything
// is opened, so a manifest with a hundred mounts is one refusal.
func TestRegistrationRefusesMoreThanFourMounts(t *testing.T) {
	h, _, home, _ := filesHost(t)
	var declared []mounts.Declared
	for i := 0; i < mounts.Max+1; i++ {
		declared = append(declared, mounts.Declared{
			Source: filepath.Join(home, "notes"),
			Target: "/files/notes" + string(rune('a'+i)),
		})
	}
	res, body := h.registerWithMounts(uuid.V7(), declared)
	if prob := h.problemOf(res, body); prob.Slug() != problem.SlugBadRequest {
		t.Fatalf("five mounts answered %s, want bad-request", prob.Slug())
	}
}

// superviseWithMounts writes one registration carrying mounts straight to the
// store, the way a registration that already checked out would have left it.
func (h *harness) superviseWithMounts(container string, resolved []mounts.Resolved) string {
	h.t.Helper()
	_, hash, err := store.NewToken()
	if err != nil {
		h.t.Fatalf("NewToken: %v", err)
	}
	id := uuid.V7()
	if err := h.store.RegisterProcess(context.Background(), store.Process{
		ID:           id,
		Owner:        h.user,
		Package:      "/home/" + h.user + "/reader",
		Name:         "reader",
		Container:    container,
		Digest:       testDigest,
		Expose:       ExposeNone,
		Mounts:       resolved,
		RegisteredAt: time.Now().UTC(),
	}, hash, store.Quota{}); err != nil {
		h.t.Fatalf("RegisterProcess: %v", err)
	}
	return id
}

// TestStartMountsWhatTheRegistrationHolds is the start half: the command line
// podman is given carries the registration's mounts, with the options a mount
// of someone's Files has to have.
func TestStartMountsWhatTheRegistrationHolds(t *testing.T) {
	h, fake, home, org := filesHost(t)
	id := h.superviseWithMounts("kitbash-reader-reader", []mounts.Resolved{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: mounts.ModeRW},
		{Source: filepath.Join(org, "handbook"), Target: "/files/handbook", Mode: mounts.ModeRO},
	})
	res, body := h.start(id, startRequest{Image: testDigest})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the start answered %d: %s", res.StatusCode, body)
	}
	runs := fake.Runs()
	if len(runs) != 1 {
		t.Fatalf("the runtime was asked to run %d containers, want one", len(runs))
	}
	line := strings.Join(runs[0].Args, " ")
	for _, want := range []string{
		"--mount type=bind,src=" + filepath.Join(home, "notes") + ",dst=/files/notes,ro=false,bind-nonrecursive,nosuid,nodev,noexec",
		"--mount type=bind,src=" + filepath.Join(org, "handbook") + ",dst=/files/handbook,ro=true,bind-nonrecursive,nosuid,nodev,noexec",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the command line is\n%s\nwant it to carry\n%s", line, want)
		}
	}
}

// TestStartRefusesAMountThatStoppedBeingLegal is why the check runs twice. The
// registration checked out when it was written; the folder lost its manifest
// afterwards, which makes it invisible, and the container is not created.
func TestStartRefusesAMountThatStoppedBeingLegal(t *testing.T) {
	h, fake, home, _ := filesHost(t)
	id := h.superviseWithMounts("kitbash-reader-reader", []mounts.Resolved{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: mounts.ModeRO},
	})
	if err := os.Remove(filepath.Join(home, "notes", "kitbash.yaml")); err != nil {
		t.Fatalf("removing the manifest: %v", err)
	}
	res, body := h.start(id, startRequest{Image: testDigest})
	if prob := h.problemOf(res, body); prob.Slug() != problem.SlugNotVisible {
		t.Fatalf("the start answered %s, want not-visible", prob.Slug())
	}
	if runs := fake.Runs(); len(runs) != 0 {
		t.Fatalf("the runtime ran %d containers for a refused start, want none", len(runs))
	}
}

// TestRestoreFailsAProcessWhoseMountIsGone is the same rule at boot: the
// Process is not started, it stays registered, and proc_list reads why.
func TestRestoreFailsAProcessWhoseMountIsGone(t *testing.T) {
	h, fake, home, _ := filesHost(t)
	id := h.superviseWithMounts("kitbash-reader-reader", []mounts.Resolved{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: mounts.ModeRO},
	})
	if err := os.RemoveAll(filepath.Join(home, "notes")); err != nil {
		t.Fatalf("removing the folder: %v", err)
	}
	counts := h.server.Restore(context.Background())
	if counts.Failed != 1 || counts.Started != 0 {
		t.Fatalf("the restore is %+v, want one failure and no start", counts)
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Fatalf("the runtime was asked to start %d containers, want none", len(calls))
	}
	if _, found, err := h.store.Process(context.Background(), id); err != nil || !found {
		t.Fatalf("the registration is gone: found %v, %v", found, err)
	}
	got := h.server.processProblem(id)
	if !strings.Contains(got.Detail, "no longer legal") {
		t.Errorf("proc_list would report %q, want it to say the mount is no longer legal", got.Detail)
	}
	if got.Fix == "" {
		t.Errorf("the problem carries no fix")
	}
}

// staged is what the runtime holds for a container that was created with these
// mounts, which is what a restore reads back off a host that has been up
// before. The fake runs no process, so the container has no PID and the check
// falls back to resolving the sources once more, see verifyMounts.
func staged(fake *sysusers.Fake, container string, mounted []mounts.Resolved) {
	if fake.Configs == nil {
		fake.Configs = map[string]sysusers.ContainerConfig{}
	}
	config := fake.Configs[container]
	config.Mounts = mounts.Podman(mounted)
	fake.Configs[container] = config
}

// TestRestoreStartsAProcessWhoseMountsAreStillLegal is the other half of the
// same test: nothing about this changes a Process whose folders are where they
// were.
func TestRestoreStartsAProcessWhoseMountsAreStillLegal(t *testing.T) {
	h, fake, home, _ := filesHost(t)
	mounted := []mounts.Resolved{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: mounts.ModeRO},
	}
	h.superviseWithMounts("kitbash-reader-reader", mounted)
	staged(fake, "kitbash-reader-reader", mounted)
	counts := h.server.Restore(context.Background())
	if counts.Started != 1 || counts.Failed != 0 {
		t.Fatalf("the restore is %+v, want one start", counts)
	}
	if calls := fake.Calls(); len(calls) != 1 {
		t.Fatalf("the runtime was asked to start %d containers, want one", len(calls))
	}
}

// TestRestoreMakesAStoppedProcessAgainWithTheRegistrationsMounts is what a boot
// does with a Process that declares mounts and is not running: it is not
// started by name, because a start makes the bind mounts again and the
// entrypoint would be running before anything could read them. It is made
// again, with the mounts the registration holds and not with whatever the old
// container happened to carry, and it is read before it runs.
func TestRestoreMakesAStoppedProcessAgainWithTheRegistrationsMounts(t *testing.T) {
	h, fake, home, _ := filesHost(t)
	const container = "kitbash-reader-reader"
	id := h.superviseWithMounts(container, []mounts.Resolved{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: mounts.ModeRO},
	})
	// The container the host holds is not running, so it has no pid, and it
	// carries a mount nobody resolved. Neither survives: the registration is
	// the record, and the cgroup parent of a container made before kitbashd
	// gave each Process one of its own is replaced with the ceiling.
	fake.Configs = map[string]sysusers.ContainerConfig{
		container: {
			CgroupParent: cgroups.MemberParent(h.user),
			Image:        testDigest,
			Mounts:       []podman.Mount{{Source: "/etc", Target: "/etc", ReadOnly: true}},
		},
	}
	fake.AddImage(h.user, testDigest, 1, nil)

	counts := h.server.Restore(context.Background())
	if counts.Started != 1 || counts.Failed != 0 {
		t.Fatalf("the restore is %+v, want the Process made again and started", counts)
	}
	runs := fake.Runs()
	if len(runs) != 1 {
		t.Fatalf("the runtime was asked to make %d containers, want the one", len(runs))
	}
	line := strings.Join(runs[0].Args, " ")
	want := "--mount type=bind,src=" + filepath.Join(home, "notes") + ",dst=/files/notes,ro=true"
	if !strings.Contains(line, want) {
		t.Errorf("the command line is\n%s\nwant it to carry\n%s", line, want)
	}
	if strings.Contains(line, "src=/etc") {
		t.Errorf("the command line carries the mount of the container it replaced:\n%s", line)
	}
	// Made, prepared, read, then started, in that order.
	if len(fake.Inits()) != 1 || len(fake.Calls()) != 1 {
		t.Errorf("the container was prepared %d times and started %d, want one of each",
			len(fake.Inits()), len(fake.Calls()))
	}
	if _, found, err := h.store.Process(context.Background(), id); err != nil || !found {
		t.Fatalf("the registration is gone: found %v, %v", found, err)
	}
}

// TestStartRefusesWhenTheRuntimeMountedAnotherFolder is the check that closes
// the window between the validation and podman resolving the source. podman
// resolves the path in its own process, after kitbashd has looked at it, so a
// source replaced by a symlink in between is followed by podman and the
// container gets whatever it pointed at. Here the runtime is made to report a
// mount of another folder, which is what that looks like from kitbashd.
func TestStartRefusesWhenTheRuntimeMountedAnotherFolder(t *testing.T) {
	h, fake, home, org := filesHost(t)
	id := h.superviseWithMounts("kitbash-reader-reader", []mounts.Resolved{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: mounts.ModeRO},
	})
	// The container the runtime answers with holds the shared folder read
	// write, which is what a member of kitbash-admin would gain by the swap
	// and the one thing the approval queue exists to be the trail of.
	fake.PinConfig("kitbash-reader-reader", sysusers.ContainerConfig{
		Mounts: []podman.Mount{{Source: filepath.Join(org, "handbook"), Target: "/files/notes"}},
	})
	res, body := h.start(id, startRequest{Image: testDigest})
	prob := h.problemOf(res, body)
	if prob.Slug() != problem.SlugNotPermitted || prob.Detail != MountSwapped {
		t.Fatalf("the start answered %s (%s), want not-permitted with %q", prob.Slug(), prob.Detail, MountSwapped)
	}
	// Nothing ran. The container was made and prepared, never started, and
	// then removed, so the image's entrypoint executed no instruction at all.
	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("the runtime was asked to start %+v, want nothing started", calls)
	}
	removals := fake.Removals()
	if len(removals) != 1 || removals[0].Container != "kitbash-reader-reader" || !removals[0].Force {
		t.Errorf("the removals are %+v, want the container removed by force", removals)
	}
}

// TestStartRefusesWhenTheRuntimeMountedNothingAtTheTarget is the same check
// with the mount missing rather than wrong, which is a runtime that did not do
// what it was asked.
func TestStartRefusesWhenTheRuntimeMountedNothingAtTheTarget(t *testing.T) {
	h, fake, home, _ := filesHost(t)
	id := h.superviseWithMounts("kitbash-reader-reader", []mounts.Resolved{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: mounts.ModeRO},
	})
	fake.PinConfig("kitbash-reader-reader", sysusers.ContainerConfig{})
	res, body := h.start(id, startRequest{Image: testDigest})
	if prob := h.problemOf(res, body); prob.Detail != MountSwapped {
		t.Fatalf("the start answered %q, want %q", prob.Detail, MountSwapped)
	}
}

// TestStartRefusesWhenTheRuntimeMountedItReadWrite is the mode: a ro mount that
// came back rw is a Process that can write a folder its owner said it may only
// read, and for /org that is the approval trail gone.
func TestStartRefusesWhenTheRuntimeMountedItReadWrite(t *testing.T) {
	h, fake, home, _ := filesHost(t)
	id := h.superviseWithMounts("kitbash-reader-reader", []mounts.Resolved{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: mounts.ModeRO},
	})
	fake.PinConfig("kitbash-reader-reader", sysusers.ContainerConfig{
		Mounts: []podman.Mount{{Source: filepath.Join(home, "notes"), Target: "/files/notes"}},
	})
	res, body := h.start(id, startRequest{Image: testDigest})
	if prob := h.problemOf(res, body); prob.Detail != MountSwapped {
		t.Fatalf("the start answered %q, want %q", prob.Detail, MountSwapped)
	}
}

// TestRestoreFailsAProcessWhoseContainerHoldsAnotherFolder is the same check at
// boot: a container this daemon did not create is read back before its Process
// is counted as restored.
func TestRestoreFailsAProcessWhoseContainerHoldsAnotherFolder(t *testing.T) {
	h, fake, home, org := filesHost(t)
	id := h.superviseWithMounts("kitbash-reader-reader", []mounts.Resolved{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: mounts.ModeRO},
	})
	// A container that is running has a pid, and that pid is the only witness
	// of what it holds. This one is holding the shared root.
	fake.PinConfig("kitbash-reader-reader", sysusers.ContainerConfig{
		State:  podman.StateRunning,
		PID:    testPID,
		Mounts: []podman.Mount{{Source: filepath.Join(org, "handbook"), Target: "/files/notes"}},
	})
	counts := h.server.Restore(context.Background())
	if counts.Failed != 1 || counts.Started != 0 {
		t.Fatalf("the restore is %+v, want one failure and no start", counts)
	}
	if removals := fake.Removals(); len(removals) != 1 {
		t.Errorf("the removals are %+v, want the container removed", removals)
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("the runtime was asked to start %+v, want nothing started", calls)
	}
	if got := h.server.processProblem(id); !strings.Contains(got.Detail, MountSwapped) {
		t.Errorf("proc_list would report %q, want it to carry %q", got.Detail, MountSwapped)
	}
}

// swappingRunner is the race itself, made repeatable: it replaces the source
// folder with a symlink to somewhere else at the moment the real runtime would
// be resolving it, which is when it prepares the container, and points the
// container's own namespace at what the link pointed to.
//
// It is what happens on a real host between kitbashd validating a source and
// podman resolving it: podman follows the link and the container is given
// whatever it pointed at, while the runtime still reports the path it was asked
// for. The Fake reports the same, so the mount it answers with looks right and
// only the reading of the namespace catches it.
type swappingRunner struct {
	*sysusers.Fake
	source string
	to     string
	// procRoot is the tree standing in for /proc, where this runner writes
	// what the container's namespace holds.
	procRoot string
	pid      int
}

func (r *swappingRunner) InitContainer(ctx context.Context, m sysusers.Member, container, cgroup string) error {
	if err := os.RemoveAll(r.source); err != nil {
		return err
	}
	if err := os.Symlink(r.to, r.source); err != nil {
		return err
	}
	if err := r.Fake.InitContainer(ctx, m, container, cgroup); err != nil {
		return err
	}
	// What the container holds at each target is what the source pointed at
	// when the runtime resolved it, which is the folder the link names.
	config, _ := r.Fake.ContainerConfig(ctx, m, container)
	for _, mount := range config.Mounts {
		if err := stageNamespace(r.procRoot, r.pid, mount.Target, r.to); err != nil {
			return err
		}
	}
	return nil
}

// TestStartRefusesASourceSwappedWhileTheContainerWasPrepared is the window this
// check exists for, with a real swap on a real filesystem: the folder is a
// folder when kitbashd validates it and a link to the shared root by the time
// the runtime resolves it, which is when it prepares the container. The runtime
// reports the path it was asked for, so what catches it is the reading of the
// container's own namespace, and nothing had run when it did.
func TestStartRefusesASourceSwappedWhileTheContainerWasPrepared(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home", "member")
	org := filepath.Join(dir, "org")
	procRoot := filepath.Join(dir, "proc")
	for _, folder := range []struct{ root, name string }{{home, "notes"}, {org, "handbook"}} {
		path := filepath.Join(folder.root, folder.name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("creating %s: %v", path, err)
		}
		body := "name: " + folder.name + "\ndescription: >-\n  The folder " + folder.name + ".\n"
		if err := os.WriteFile(filepath.Join(path, "kitbash.yaml"), []byte(body), 0o644); err != nil {
			t.Fatalf("writing the manifest of %s: %v", path, err)
		}
	}
	notes := filepath.Join(home, "notes")
	fake := sysusers.NewFake()
	fake.NextPID = testPID
	runner := &swappingRunner{
		Fake: fake, source: notes, to: filepath.Join(org, "handbook"),
		procRoot: procRoot, pid: testPID,
	}
	h := serveWith(t, Options{
		Users:           fake,
		Runner:          runner,
		OrgRoot:         org,
		ProcRoot:        procRoot,
		ProcessEndpoint: "http://127.0.0.1:4318",
	})
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid(), Home: home})

	id := h.superviseWithMounts("kitbash-reader-reader", []mounts.Resolved{
		{Source: notes, Target: "/files/notes", Mode: mounts.ModeRO},
	})
	// The registration holds the folder as it was, so the check before the
	// start passes: that is the window, and the swap happens inside it.
	res, body := h.start(id, startRequest{Image: testDigest})
	prob := h.problemOf(res, body)
	if prob.Slug() != problem.SlugNotPermitted || prob.Detail != MountSwapped {
		t.Fatalf("the start answered %s (%s), want not-permitted with %q", prob.Slug(), prob.Detail, MountSwapped)
	}
	// Nothing ever ran: the container was made and prepared, never started,
	// and removed.
	if calls := fake.Calls(); len(calls) != 0 {
		t.Fatalf("the runtime was asked to start %+v, want nothing started", calls)
	}
	if removals := fake.Removals(); len(removals) != 1 || !removals[0].Force {
		t.Errorf("the removals are %+v, want the container removed by force", removals)
	}
	// The Process stays registered: the folder is what is wrong, and its owner
	// runs it again once it is a folder they meant.
	if _, found, err := h.store.Process(context.Background(), id); err != nil || !found {
		t.Fatalf("the registration is gone: found %v, %v", found, err)
	}
}

// TestStartRefusesWhenTheNamespaceHoldsAnotherFolder is the comparison of
// device and inode on its own. Everything the runtime says is right: it reports
// the source the registration names, at the target it names, read only. What is
// wrong is what the container actually has at that target, which is the only
// question the mount spec cannot answer, because podman reports the path it was
// asked for and not what it resolved.
func TestStartRefusesWhenTheNamespaceHoldsAnotherFolder(t *testing.T) {
	h, fake, home, org, procRoot := filesHostWithProc(t)
	notes := filepath.Join(home, "notes")
	id := h.superviseWithMounts("kitbash-reader-reader", []mounts.Resolved{
		{Source: notes, Target: "/files/notes", Mode: mounts.ModeRO},
	})
	// The runtime is honest about its command line and the container holds
	// something else, which is what following a swapped link produces.
	fake.PinConfig("kitbash-reader-reader", sysusers.ContainerConfig{
		PID:    testPID,
		Mounts: []podman.Mount{{Source: notes, Target: "/files/notes", ReadOnly: true}},
	})
	if err := stageNamespace(procRoot, testPID, "/files/notes", filepath.Join(org, "handbook")); err != nil {
		t.Fatalf("staging the namespace: %v", err)
	}
	res, body := h.start(id, startRequest{Image: testDigest})
	if prob := h.problemOf(res, body); prob.Detail != MountSwapped {
		t.Fatalf("the start answered %q, want %q: the mount spec was right and the folder was not",
			prob.Detail, MountSwapped)
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("the runtime was asked to start %+v, want nothing started", calls)
	}
}

// TestStartAcceptsWhenTheNamespaceHoldsTheValidatedFolder is the other half of
// that comparison, so a check that always refused would fail here: the same
// container, with the target holding the folder the validation opened, starts.
func TestStartAcceptsWhenTheNamespaceHoldsTheValidatedFolder(t *testing.T) {
	h, fake, home, _, procRoot := filesHostWithProc(t)
	notes := filepath.Join(home, "notes")
	id := h.superviseWithMounts("kitbash-reader-reader", []mounts.Resolved{
		{Source: notes, Target: "/files/notes", Mode: mounts.ModeRO},
	})
	fake.PinConfig("kitbash-reader-reader", sysusers.ContainerConfig{
		PID:    testPID,
		Mounts: []podman.Mount{{Source: notes, Target: "/files/notes", ReadOnly: true}},
	})
	if err := stageNamespace(procRoot, testPID, "/files/notes", notes); err != nil {
		t.Fatalf("staging the namespace: %v", err)
	}
	res, body := h.start(id, startRequest{Image: testDigest})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the start answered %d: %s", res.StatusCode, body)
	}
	if calls := fake.Calls(); len(calls) != 1 || calls[0].Container != "kitbash-reader-reader" {
		t.Fatalf("the containers started are %+v, want the one", calls)
	}
}

// TestStartRefusesWhenTheContainerHasNoProcessAfterItWasPrepared is a container
// the runtime did not prepare: there is no namespace to read, so there is
// nothing kitbashd can say about what it holds, and a Process it cannot verify
// does not start. There is no weaker check to fall back to.
func TestStartRefusesWhenTheContainerHasNoProcessAfterItWasPrepared(t *testing.T) {
	h, fake, home, _ := filesHost(t)
	id := h.superviseWithMounts("kitbash-reader-reader", []mounts.Resolved{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: mounts.ModeRO},
	})
	fake.PIDs = map[string]int{"kitbash-reader-reader": 0}
	res, body := h.start(id, startRequest{Image: testDigest})
	if prob := h.problemOf(res, body); prob.Detail != MountSwapped {
		t.Fatalf("the start answered %q, want %q", prob.Detail, MountSwapped)
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("the runtime was asked to start %+v, want nothing started", calls)
	}
	if removals := fake.Removals(); len(removals) != 1 {
		t.Errorf("the removals are %+v, want the container removed", removals)
	}
}

// TestStartRefusesWhenThePreparationFails is the source that stopped existing
// between the create and the preparation, which is what podman init answers
// with an error. Verified on podman 5.7.0: the container is left created with
// no process. It is the same refusal, and nothing ran.
func TestStartRefusesWhenThePreparationFails(t *testing.T) {
	h, fake, home, _ := filesHost(t)
	id := h.superviseWithMounts("kitbash-reader-reader", []mounts.Resolved{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: mounts.ModeRO},
	})
	fake.InitErr = errors.New("crun: mount: No such file or directory")
	res, body := h.start(id, startRequest{Image: testDigest})
	if prob := h.problemOf(res, body); prob.Detail != MountSwapped {
		t.Fatalf("the start answered %q, want %q", prob.Detail, MountSwapped)
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("the runtime was asked to start %+v, want nothing started", calls)
	}
}

// TestStartPreparesBeforeItStarts is the order itself, which is the whole
// guarantee: the container is made, then prepared, then started, and the check
// sits between the last two.
func TestStartPreparesBeforeItStarts(t *testing.T) {
	h, fake, home, _ := filesHost(t)
	id := h.superviseWithMounts("kitbash-reader-reader", []mounts.Resolved{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: mounts.ModeRO},
	})
	res, body := h.start(id, startRequest{Image: testDigest})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the start answered %d: %s", res.StatusCode, body)
	}
	if len(fake.Runs()) != 1 || len(fake.Inits()) != 1 || len(fake.Calls()) != 1 {
		t.Fatalf("the runtime was asked for %d creates, %d preparations and %d starts, want one of each",
			len(fake.Runs()), len(fake.Inits()), len(fake.Calls()))
	}
	if args := fake.Runs()[0].Args; len(args) == 0 || args[0] != "create" {
		t.Errorf("the command line is %v, want a podman create", args)
	}
}

// TestRestoreStartsAContainerLeftInitialized is issue #119. A daemon killed
// between preparing a container and starting it, and a start the runtime
// refused, both leave a container that has a pid, a mount namespace and no
// entrypoint running. A pid alone would read as running, and that Process would
// be counted restored and never started, which proc_list reports as starting
// for ever.
//
// It is the state a start pauses in, so it is read the same way and then
// started: start after prepare runs the entrypoint in the namespace that was
// just read, verified on podman 5.7.0.
func TestRestoreStartsAContainerLeftInitialized(t *testing.T) {
	h, fake, home, _, procRoot := filesHostWithProc(t)
	notes := filepath.Join(home, "notes")
	const container = "kitbash-reader-reader"
	h.superviseWithMounts(container, []mounts.Resolved{
		{Source: notes, Target: "/files/notes", Mode: mounts.ModeRO},
	})
	fake.Configs = map[string]sysusers.ContainerConfig{
		container: {
			State:  podman.StateInitialized,
			PID:    testPID,
			Mounts: []podman.Mount{{Source: notes, Target: "/files/notes", ReadOnly: true}},
		},
	}
	if err := stageNamespace(procRoot, testPID, "/files/notes", notes); err != nil {
		t.Fatalf("staging the namespace: %v", err)
	}

	counts := h.server.Restore(context.Background())
	if counts.Started != 1 || counts.Running != 0 || counts.Failed != 0 {
		t.Fatalf("the restore is %+v, want the prepared container started and not counted as already running", counts)
	}
	calls := fake.Calls()
	if len(calls) != 1 || calls[0].Container != container {
		t.Fatalf("the containers started are %+v, want the prepared one", calls)
	}
	// It was not made again: the container that was there is the one that runs.
	if runs := fake.Runs(); len(runs) != 0 {
		t.Errorf("the runtime made %+v, want the prepared container used as it stood", runs)
	}
}

// TestRestoreRefusesAContainerLeftInitializedHoldingAnotherFolder is the same
// state with the mount wrong: it is read before it is started, so it never runs.
func TestRestoreRefusesAContainerLeftInitializedHoldingAnotherFolder(t *testing.T) {
	h, fake, home, org, procRoot := filesHostWithProc(t)
	notes := filepath.Join(home, "notes")
	const container = "kitbash-reader-reader"
	id := h.superviseWithMounts(container, []mounts.Resolved{
		{Source: notes, Target: "/files/notes", Mode: mounts.ModeRO},
	})
	fake.Configs = map[string]sysusers.ContainerConfig{
		container: {
			State:  podman.StateInitialized,
			PID:    testPID,
			Mounts: []podman.Mount{{Source: notes, Target: "/files/notes", ReadOnly: true}},
		},
	}
	if err := stageNamespace(procRoot, testPID, "/files/notes", filepath.Join(org, "handbook")); err != nil {
		t.Fatalf("staging the namespace: %v", err)
	}

	counts := h.server.Restore(context.Background())
	if counts.Failed != 1 || counts.Started != 0 {
		t.Fatalf("the restore is %+v, want one failure and no start", counts)
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Fatalf("the runtime was asked to start %+v, want nothing started", calls)
	}
	if got := h.server.processProblem(id); !strings.Contains(got.Detail, MountSwapped) {
		t.Errorf("proc_list would report %q, want it to carry %q", got.Detail, MountSwapped)
	}
}

// TestRestoreMakesACreatedContainerAgain is the other half of the state: a
// container that was made and never prepared has no namespace, so there is
// nothing to read and starting it by name would build the bind mounts with the
// entrypoint already running. It is made again instead.
func TestRestoreMakesACreatedContainerAgain(t *testing.T) {
	h, fake, home, _ := filesHost(t)
	const container = "kitbash-reader-reader"
	h.superviseWithMounts(container, []mounts.Resolved{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: mounts.ModeRO},
	})
	fake.Configs = map[string]sysusers.ContainerConfig{
		container: {State: podman.StateCreated, Image: testDigest},
	}
	fake.AddImage(h.user, testDigest, 1, nil)

	counts := h.server.Restore(context.Background())
	if counts.Started != 1 || counts.Failed != 0 {
		t.Fatalf("the restore is %+v, want the container made again and started", counts)
	}
	if len(fake.Runs()) != 1 || len(fake.Inits()) != 1 || len(fake.Calls()) != 1 {
		t.Errorf("the runtime was asked for %d creates, %d preparations and %d starts, want one of each",
			len(fake.Runs()), len(fake.Inits()), len(fake.Calls()))
	}
}

// TestStartRemovesTheContainerWhenTheStartFails is the other half of issue
// #119. The container is made and prepared, its mounts check out, and the
// runtime refuses to start it: leaving it there would leave a Process holding a
// mount namespace and running nothing, which proc_list reports as starting.
func TestStartRemovesTheContainerWhenTheStartFails(t *testing.T) {
	h, fake, home, _ := filesHost(t)
	id := h.superviseWithMounts("kitbash-reader-reader", []mounts.Resolved{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: mounts.ModeRO},
	})
	fake.StartErrs = map[string]error{
		"kitbash-reader-reader": errors.New("crun: starting container process caused: exec format error"),
	}

	res, body := h.start(id, startRequest{Image: testDigest})
	if res.StatusCode == http.StatusOK {
		t.Fatalf("the start answered %d: %s", res.StatusCode, body)
	}
	removals := fake.Removals()
	if len(removals) != 1 || removals[0].Container != "kitbash-reader-reader" || !removals[0].Force {
		t.Fatalf("the removals are %+v, want the prepared container removed by force", removals)
	}
	// The registration stays: the Process is what the member runs again.
	if _, found, err := h.store.Process(context.Background(), id); err != nil || !found {
		t.Fatalf("the registration is gone: found %v, %v", found, err)
	}
}
