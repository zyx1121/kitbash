package cgroups_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/cgroups"
)

// TestFakeIsACgroups keeps the fake honest: internal/daemon is tested against
// it, so it answers the leaf a real host would and refuses what a real one
// refuses.
func TestFakeIsACgroups(t *testing.T) {
	var _ cgroups.Cgroups = cgroups.NewFake()

	f := &cgroups.Fake{Base: "/sys/fs/cgroup"}
	ctx := context.Background()
	if err := f.EnsureRoot(ctx); err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}
	leaf, err := f.EnsureMember(ctx, "alice", 1005, 1005)
	if err != nil {
		t.Fatalf("EnsureMember: %v", err)
	}
	if leaf != "/sys/fs/cgroup/kitbash/alice/run" {
		t.Errorf("the leaf is %q, want the member's run cgroup", leaf)
	}
	calls := f.Calls()
	if len(calls) != 1 || calls[0].Name != "alice" || calls[0].UID != 1005 {
		t.Fatalf("calls = %+v, want one for alice with her uid", calls)
	}
	if _, err := f.EnsureMember(ctx, "../root", 1005, 1005); !errors.Is(err, cgroups.ErrName) {
		t.Errorf("a name that is a path = %v, want ErrName", err)
	}
	if !f.Enabled() {
		t.Error("the fake places nothing, want placement on")
	}
}

// A host that enforces no limits answers no leaf and no error: the Process
// still runs, and its limits are recorded rather than enforced.
func TestFakeWithoutDelegationAnswersNoLeaf(t *testing.T) {
	f := &cgroups.Fake{Off: true}
	leaf, err := f.EnsureMember(context.Background(), "alice", 1005, 1005)
	if err != nil || leaf != "" {
		t.Errorf("EnsureMember = %q, %v, want no leaf and no error", leaf, err)
	}
	if f.Enabled() {
		t.Error("a host with placement off reports it on")
	}
}

// Parent is what the container is created under, and it is the member's own
// cgroup rather than the leaf: a cgroup that holds processes cannot be the
// parent of another one.
func TestParentIsTheMemberAndNotTheLeaf(t *testing.T) {
	if got := cgroups.Parent("alice"); got != "/kitbash/alice" {
		t.Errorf("the cgroup parent is %q, want /kitbash/alice", got)
	}
	if got := cgroups.LeafDir("/sys/fs/cgroup", "alice"); got != "/sys/fs/cgroup/kitbash/alice/run" {
		t.Errorf("the leaf is %q, want the run cgroup under the member's", got)
	}
}

// A directory that is not a cgroup v2 mount is a host where limits are
// recorded and not enforced, which is the answer everywhere but a kitbash
// host: nothing is created and nothing fails.
func TestARootThatIsNotCgroupV2TurnsPlacementOff(t *testing.T) {
	dir := t.TempDir()
	c := cgroups.New(dir)
	ctx := context.Background()
	if err := c.EnsureRoot(ctx); err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}
	leaf, err := c.EnsureMember(ctx, "alice", 1005, 1005)
	if err != nil {
		t.Fatalf("EnsureMember: %v", err)
	}
	if leaf != "" {
		t.Errorf("the leaf is %q, want none on a host without cgroup v2", leaf)
	}
	if c.Enabled() {
		t.Error("placement is on for a directory that is not a cgroup filesystem")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("the root holds %v, want nothing created on a host without cgroup v2", entries)
	}
}

// The real thing, on a host that has one: the layout of the package comment,
// with the controllers delegated and the member's subtree given to them. It
// needs a writable cgroup v2 mount, which means root, so it skips everywhere
// else rather than failing.
func TestTheRealTreeOnAWritableCgroupV2Mount(t *testing.T) {
	const root = cgroups.DefaultRoot
	if os.Geteuid() != 0 {
		t.Skip("delegating a cgroup needs root")
	}
	if _, err := os.Stat(filepath.Join(root, "cgroup.subtree_control")); err != nil {
		t.Skipf("%s is not a cgroup v2 mount: %v", root, err)
	}
	c := cgroups.New(root)
	ctx := context.Background()
	if err := c.EnsureRoot(ctx); err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}
	if !c.Enabled() {
		t.Skip("this host does not delegate the controllers kitbash needs")
	}
	name := "kitbash-test-member"
	leaf, err := c.EnsureMember(ctx, name, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("EnsureMember: %v", err)
	}
	t.Cleanup(func() {
		os.Remove(leaf)
		os.Remove(cgroups.MemberDir(root, name))
	})
	if leaf != cgroups.LeafDir(root, name) {
		t.Fatalf("the leaf is %q, want %q", leaf, cgroups.LeafDir(root, name))
	}
	if _, err := os.Stat(filepath.Join(leaf, "cgroup.procs")); err != nil {
		t.Fatalf("the leaf is not a cgroup: %v", err)
	}
	enabled, err := os.ReadFile(filepath.Join(cgroups.MemberDir(root, name), "cgroup.subtree_control"))
	if err != nil {
		t.Fatalf("reading the delegation: %v", err)
	}
	for _, controller := range cgroups.Controllers {
		if !strings.Contains(string(enabled), controller) {
			t.Errorf("the subtree delegates %q, want %s", strings.TrimSpace(string(enabled)), controller)
		}
	}
	info, err := os.Stat(cgroups.MemberDir(root, name))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !ownedBy(info, os.Getuid()) {
		t.Error("the subtree was not given to the member")
	}
	// The member's own cgroup carries no limit: a limit here would be a cap on
	// everything they run at once, which is not what a manifest declares. The
	// limits are the container's, and podman writes them into the container's
	// own cgroup under this one.
	for _, file := range []string{"memory.max", "pids.max"} {
		limit, err := os.ReadFile(filepath.Join(cgroups.MemberDir(root, name), file))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(limit)) != "max" {
			t.Errorf("%s of the member's cgroup is %q, want max: only containers are limited",
				file, strings.TrimSpace(string(limit)))
		}
	}
}
