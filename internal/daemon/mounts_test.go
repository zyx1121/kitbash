package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
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

// filesHost is a daemon whose members are a fake host with real folders behind
// them: the caller's home and an /org of the test's own, each holding one
// visible folder, so the resolution kitbashd makes as root has a tree to make
// it in. The caller's own uid is what the folders are owned by, which is what
// makes "owned by the member" a question this test can answer.
func filesHost(t *testing.T) (*harness, *sysusers.Fake, string, string) {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home", "member")
	org := filepath.Join(dir, "org")
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
	h := serveWith(t, Options{
		Users:           fake,
		Runner:          fake,
		OrgRoot:         org,
		ProcessEndpoint: "http://127.0.0.1:4318",
	})
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid(), Home: home})
	return h, fake, home, org
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
	}, hash, 0); err != nil {
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

// TestRestoreStartsAProcessWhoseMountsAreStillLegal is the other half of the
// same test: nothing about this changes a Process whose folders are where they
// were.
func TestRestoreStartsAProcessWhoseMountsAreStillLegal(t *testing.T) {
	h, fake, home, _ := filesHost(t)
	h.superviseWithMounts("kitbash-reader-reader", []mounts.Resolved{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: mounts.ModeRO},
	})
	counts := h.server.Restore(context.Background())
	if counts.Started != 1 || counts.Failed != 0 {
		t.Fatalf("the restore is %+v, want one start", counts)
	}
	if calls := fake.Calls(); len(calls) != 1 {
		t.Fatalf("the runtime was asked to start %d containers, want one", len(calls))
	}
}

// TestHealCreatesTheContainerAgainWithTheRegistrationsMounts is the heal path:
// the container is created again, so its mounts come from the registration,
// which is the record kitbashd resolved itself, and not from whatever the old
// container happened to carry.
func TestHealCreatesTheContainerAgainWithTheRegistrationsMounts(t *testing.T) {
	h, fake, home, _ := filesHost(t)
	const container = "kitbash-reader-reader"
	id := h.superviseWithMounts(container, []mounts.Resolved{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: mounts.ModeRO},
	})
	// The container of a Process registered before kitbashd wrote a ceiling
	// per Process names its member's own cgroup, which is what the heal is
	// for, and it carries a mount nobody resolved.
	fake.Configs = map[string]sysusers.ContainerConfig{
		container: {
			CgroupParent: cgroups.MemberParent(h.user),
			Image:        testDigest,
			// A mount the old container carries that nobody resolved, which
			// the heal must not carry over: the registration is the record.
			Mounts: []podman.Mount{{Source: "/etc", Target: "/etc", ReadOnly: true}},
		},
	}
	fake.AddImage(h.user, testDigest, 1, nil)
	// What the host does today: the member's runtime cannot make the
	// container's cgroup under a directory that belongs to root.
	fake.StartErr = errors.New(
		"crun: create `/sys/fs/cgroup/kitbash/member/libpod-fd4b88f9`: Permission denied: OCI permission denied")

	counts := h.server.Restore(context.Background())
	if counts.Healed != 1 {
		t.Fatalf("the restore is %+v, want one heal", counts)
	}
	runs := fake.Runs()
	if len(runs) != 1 {
		t.Fatalf("the runtime was asked to run %d containers, want the healed one", len(runs))
	}
	line := strings.Join(runs[0].Args, " ")
	want := "--mount type=bind,src=" + filepath.Join(home, "notes") + ",dst=/files/notes,ro=true"
	if !strings.Contains(line, want) {
		t.Errorf("the healed command line is\n%s\nwant it to carry\n%s", line, want)
	}
	if strings.Contains(line, "src=/etc") {
		t.Errorf("the healed command line carries the old container's own mount:\n%s", line)
	}
	if _, found, err := h.store.Process(context.Background(), id); err != nil || !found {
		t.Fatalf("the healed registration is gone: found %v, %v", found, err)
	}
}
