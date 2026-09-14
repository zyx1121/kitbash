package sysusers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// TestFakeIsASystem keeps the fake honest: internal/daemon is tested against
// it, so it has to refuse what a host refuses and record what a host does.
func TestFakeIsASystem(t *testing.T) {
	var _ sysusers.System = sysusers.NewFake()
	var _ sysusers.Runner = sysusers.NewFake()

	f := sysusers.NewFake()
	ctx := context.Background()

	m, err := f.Create(ctx, sysusers.Spec{Name: "alice", SSHKey: key("ssh-ed25519"), Admin: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if m.UID == 0 || m.Home != "/home/alice" || !m.Admin || m.Keys != 1 {
		t.Errorf("member = %+v, want a uid, a home, the admin flag and one key", m)
	}
	if len(f.Created) != 1 || f.Created[0].Name != "alice" {
		t.Errorf("created = %+v, want the call recorded", f.Created)
	}

	if _, err := f.Create(ctx, sysusers.Spec{Name: "alice", SSHKey: key("ssh-rsa")}); !errors.Is(err, sysusers.ErrExists) {
		t.Errorf("second Create = %v, want ErrExists", err)
	}
	if _, err := f.Create(ctx, sysusers.Spec{Name: "Alice", SSHKey: key("ssh-rsa")}); !errors.Is(err, sysusers.ErrName) {
		t.Errorf("Create with a bad name = %v, want ErrName", err)
	}
	if _, err := f.Create(ctx, sysusers.Spec{Name: "bob", SSHKey: "not a key"}); !errors.Is(err, sysusers.ErrKey) {
		t.Errorf("Create with a bad key = %v, want ErrKey", err)
	}

	// The same key twice is one key.
	if m, err = f.AddKey(ctx, "alice", key("ssh-ed25519")); err != nil || m.Keys != 1 {
		t.Errorf("AddKey with the same key = %+v, %v, want one key", m, err)
	}
	if m, err = f.AddKey(ctx, "alice", key("ssh-rsa")); err != nil || m.Keys != 2 {
		t.Errorf("AddKey with a second key = %+v, %v, want two keys", m, err)
	}
	if _, err := f.AddKey(ctx, "carol", key("ssh-rsa")); !errors.Is(err, sysusers.ErrNotFound) {
		t.Errorf("AddKey on a stranger = %v, want ErrNotFound", err)
	}

	archived, err := f.Remove(ctx, "alice")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if archived != "/org/.archive/alice" {
		t.Errorf("archived = %q, want /org/.archive/alice", archived)
	}
	if _, found, err := f.Lookup(ctx, "alice"); err != nil || found {
		t.Errorf("Lookup after Remove = %t, %v, want gone", found, err)
	}
	if _, err := f.Remove(ctx, "alice"); !errors.Is(err, sysusers.ErrNotFound) {
		t.Errorf("second Remove = %v, want ErrNotFound", err)
	}
}

// TestFakeRunner is the half of the fake restore is tested against.
func TestFakeRunner(t *testing.T) {
	f := sysusers.NewFake()
	f.Missing = map[string]bool{"kitbash-gone": true}
	ctx := context.Background()
	m := f.Add(sysusers.Member{Name: "alice"})

	const leaf = "/sys/fs/cgroup/kitbash/alice/run"
	if err := f.Start(ctx, m, "kitbash-echo", leaf); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := f.Start(ctx, m, "kitbash-gone", leaf); !errors.Is(err, sysusers.ErrNoContainer) {
		t.Errorf("Start of a missing container = %v, want ErrNoContainer", err)
	}
	calls := f.Calls()
	if len(calls) != 1 || calls[0].Container != "kitbash-echo" || calls[0].Member != "alice" {
		t.Errorf("calls = %+v, want one start of kitbash-echo as alice", calls)
	}
	// The leaf is recorded because it is what the child is placed in, which is
	// the whole reason kitbashd starts a container instead of the session.
	if calls[0].Cgroup != leaf {
		t.Errorf("the child was placed in %q, want %q", calls[0].Cgroup, leaf)
	}

	// The three the daemon added with the supervisor: a run, a stop and a
	// removal, each as the member.
	image := "sha256:" + strings.Repeat("a", 64)
	id, err := f.CreateContainer(ctx, m, podman.RunOptions{Name: "kitbash-echo", Image: image}, leaf)
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if id == "" {
		t.Error("CreateContainer answered no container id")
	}
	runs := f.Runs()
	if len(runs) != 1 || runs[0].Cgroup != leaf || runs[0].Member != "alice" {
		t.Fatalf("runs = %+v, want one container made as alice in the leaf", runs)
	}
	if len(runs[0].Args) == 0 || runs[0].Args[0] != "create" {
		t.Errorf("the command line is %v, want a podman create", runs[0].Args)
	}
	f.Missing[image] = true
	if _, err := f.CreateContainer(ctx, m, podman.RunOptions{Name: "kitbash-echo", Image: image}, leaf); !errors.Is(err, sysusers.ErrNoImage) {
		t.Errorf("CreateContainer of a missing image = %v, want ErrNoImage", err)
	}
	if err := f.Stop(ctx, m, "kitbash-echo", 10); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stops := f.Stops(); len(stops) != 1 || stops[0].Timeout != 10 {
		t.Errorf("stops = %+v, want one stop with the ten second timeout", stops)
	}
	if err := f.RemoveContainer(ctx, m, "kitbash-echo", true); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if removed := f.Removals(); len(removed) != 1 || !removed[0].Force {
		t.Errorf("removals = %+v, want one forced removal", removed)
	}
	if err := f.RemoveAll(ctx, m); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if len(f.RemovedFor) != 1 || f.RemovedFor[0] != "alice" {
		t.Errorf("removed = %v, want alice", f.RemovedFor)
	}
}
