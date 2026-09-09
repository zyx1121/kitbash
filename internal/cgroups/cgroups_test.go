package cgroups_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/zyx1121/kitbash/internal/cgroups"
)

// process is one Process id, the shape a cgroup is named by.
const process = "01a08654-1c5b-7828-9b6b-37044de254d4"

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
	if err := f.EnsureMember(ctx, "alice", 1005, 1005); err != nil {
		t.Fatalf("EnsureMember: %v", err)
	}
	limits := cgroups.Limits{Memory: "536870912", CPU: "100000 100000", Pids: 512}
	leaf, err := f.EnsureProcess(ctx, "alice", process, 1005, 1005, limits)
	if err != nil {
		t.Fatalf("EnsureProcess: %v", err)
	}
	if leaf != "/sys/fs/cgroup/kitbash/alice/"+process+"/run" {
		t.Errorf("the leaf is %q, want the run cgroup of the Process", leaf)
	}
	placed := f.Placed()
	if len(placed) != 1 || placed[0].Limits != limits {
		t.Fatalf("the cgroups placed are %+v, want one carrying the ceiling", placed)
	}
	if _, err := f.EnsureProcess(ctx, "alice", "../root", 1005, 1005, limits); !errors.Is(err, cgroups.ErrName) {
		t.Errorf("an id that is a path = %v, want ErrName", err)
	}
	if err := f.EnsureMember(ctx, "../root", 1005, 1005); !errors.Is(err, cgroups.ErrName) {
		t.Errorf("a name that is a path = %v, want ErrName", err)
	}
}

// A host that cannot delegate says so on every call. It does not turn itself
// off: the caller runs the Process unplaced and says so on that start, and the
// next start may be for a member whose cgroup is fine.
func TestAFakeWithoutDelegationRefusesEveryCall(t *testing.T) {
	f := &cgroups.Fake{Off: true}
	ctx := context.Background()
	if err := f.EnsureRoot(ctx); err == nil {
		t.Error("EnsureRoot answered a host that cannot delegate")
	}
	if _, err := f.EnsureProcess(ctx, "alice", process, 1005, 1005, cgroups.Limits{}); err == nil {
		t.Error("EnsureProcess answered a host that cannot delegate")
	}
}

// Parent is what the container is created under: the Process's own cgroup,
// which carries the ceiling, and not the leaf, which holds the podman child.
func TestTheContainerIsCreatedUnderTheProcessCgroup(t *testing.T) {
	if got := cgroups.Parent("alice", process); got != "/kitbash/alice/"+process {
		t.Errorf("the cgroup parent is %q, want the Process's own cgroup", got)
	}
	if got := cgroups.LeafDir("/sys/fs/cgroup", "alice", process); got !=
		"/sys/fs/cgroup/kitbash/alice/"+process+"/run" {
		t.Errorf("the leaf is %q, want the run cgroup under the Process's", got)
	}
}

// The limits of a manifest reach the cgroup files in the spelling those files
// take, which is bytes for memory and a quota with a period for cpu.
func TestTheLimitsAreConvertedForTheCgroupFiles(t *testing.T) {
	memory := []struct {
		in   string
		want string
		ok   bool
	}{
		{in: "512Mi", want: "536870912", ok: true},
		{in: "512m", want: "536870912", ok: true},
		{in: "256Ki", want: "262144", ok: true},
		{in: "2Gi", want: "2147483648", ok: true},
		{in: "1024", want: "1024", ok: true},
		{in: "", ok: false},
		{in: "lots", ok: false},
		{in: "0", ok: false},
	}
	for _, tc := range memory {
		got, ok := cgroups.MemoryMax(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("MemoryMax(%q) = %q, %t, want %q, %t", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	cpu := []struct {
		in   string
		want string
		ok   bool
	}{
		{in: "1", want: "100000 100000", ok: true},
		{in: "0.5", want: "50000 100000", ok: true},
		{in: "2", want: "200000 100000", ok: true},
		{in: "", ok: false},
		{in: "many", ok: false},
	}
	for _, tc := range cpu {
		got, ok := cgroups.CPUMax(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("CPUMax(%q) = %q, %t, want %q, %t", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// A directory that is not a cgroup v2 mount is a host where limits are
// recorded and not enforced: nothing is created and the caller is told why.
func TestARootThatIsNotCgroupV2IsRefused(t *testing.T) {
	dir := t.TempDir()
	c := cgroups.New(dir)
	if err := c.EnsureRoot(context.Background()); err == nil {
		t.Fatal("a directory that is not a cgroup filesystem was accepted")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("the root holds %v, want nothing created", entries)
	}
}

// The real tree, on a host that has one. It is built under a cgroup of its
// own, so the host's own root is never written, and the whole of it goes
// again at the end.
func TestTheRealTreeOnAWritableCgroupV2Mount(t *testing.T) {
	root := testRoot(t)
	c := cgroups.New(root)
	ctx := context.Background()
	if err := c.EnsureRoot(ctx); err != nil {
		t.Skipf("this host does not delegate the controllers kitbash needs: %v", err)
	}
	const name = "kitbash-test-member"
	uid, gid := nobody(t)
	limits := cgroups.Limits{Memory: "536870912", CPU: "50000 100000", Pids: 64}
	leaf, err := c.EnsureProcess(ctx, name, process, uid, gid, limits)
	if err != nil {
		t.Fatalf("EnsureProcess: %v", err)
	}
	if leaf != cgroups.LeafDir(root, name, process) {
		t.Fatalf("the leaf is %q, want %q", leaf, cgroups.LeafDir(root, name, process))
	}
	if _, err := os.Stat(filepath.Join(leaf, "cgroup.procs")); err != nil {
		t.Fatalf("the leaf is not a cgroup: %v", err)
	}

	dir := cgroups.ProcessDir(root, name, process)
	// The ceiling is written, and it is the manifest's.
	for file, want := range map[string]string{
		"memory.max": "536870912",
		"cpu.max":    "50000 100000",
		"pids.max":   "64",
	} {
		if got := read(t, filepath.Join(dir, file)); got != want {
			t.Errorf("%s is %q, want %q", file, got, want)
		}
	}
	// The controllers are delegated, so the container's own cgroup can carry
	// limits too.
	enabled := read(t, filepath.Join(dir, "cgroup.subtree_control"))
	for _, controller := range cgroups.Controllers {
		if !strings.Contains(enabled, controller) {
			t.Errorf("the Process cgroup delegates %q, want %s", enabled, controller)
		}
	}
	// The member is given the directory and the three delegation files, so
	// their rootless podman can create the container's cgroup and move the
	// container into it.
	for _, path := range []string{dir, filepath.Join(dir, "cgroup.procs"),
		filepath.Join(dir, "cgroup.subtree_control"), leaf} {
		if owner := ownerOf(t, path); owner != uid {
			t.Errorf("%s belongs to uid %d, want the member's %d", path, owner, uid)
		}
	}
	// And nothing else. The ceiling stays root's, which is the whole point of
	// a Process cgroup: the member cannot raise what they may spend.
	for _, file := range []string{"memory.max", "cpu.max", "pids.max"} {
		if owner := ownerOf(t, filepath.Join(dir, file)); owner != 0 {
			t.Errorf("%s belongs to uid %d, want root", file, owner)
		}
	}
	// The member cgroup above it is root's in full, so nobody can make a
	// Process cgroup beside this one with no ceiling in it.
	if owner := ownerOf(t, cgroups.MemberDir(root, name)); owner != 0 {
		t.Errorf("the member cgroup belongs to uid %d, want root", owner)
	}

	if err := c.RemoveProcess(ctx, name, process); err != nil {
		t.Errorf("RemoveProcess: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the Process cgroup is still there: %v", err)
	}
	if err := c.RemoveMember(ctx, name); err != nil {
		t.Errorf("RemoveMember: %v", err)
	}
	if _, err := os.Stat(cgroups.MemberDir(root, name)); !os.IsNotExist(err) {
		t.Errorf("the member cgroup is still there: %v", err)
	}
}

// The blocker this design answers: a member who owns their Process cgroup
// could raise their own ceiling. They own the directory, and the limit files
// in it are still root's, so the write is refused by the kernel.
func TestAMemberCannotRaiseTheCeiling(t *testing.T) {
	root := testRoot(t)
	c := cgroups.New(root)
	ctx := context.Background()
	if err := c.EnsureRoot(ctx); err != nil {
		t.Skipf("this host does not delegate the controllers kitbash needs: %v", err)
	}
	const name = "kitbash-test-member"
	uid, gid := nobody(t)
	if _, err := c.EnsureProcess(ctx, name, process, uid, gid, cgroups.Limits{Memory: "536870912"}); err != nil {
		t.Fatalf("EnsureProcess: %v", err)
	}
	t.Cleanup(func() { c.RemoveMember(ctx, name) })

	limit := filepath.Join(cgroups.ProcessDir(root, name, process), "memory.max")
	raise := exec.Command("/bin/sh", "-c", "echo max > "+limit)
	raise.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
	}
	out, err := raise.CombinedOutput()
	if err == nil {
		t.Fatalf("uid %d raised its own ceiling: %s", uid, out)
	}
	if !strings.Contains(strings.ToLower(string(out)), "permission denied") {
		t.Errorf("the write failed with %q, want permission denied", strings.TrimSpace(string(out)))
	}
	if got := read(t, limit); got != "536870912" {
		t.Errorf("memory.max is %q, want the ceiling kitbashd wrote", got)
	}
}

// testRoot is a cgroup of this test's own under the host's hierarchy, removed
// at the end. Building the tree under it means the host's own root cgroup is
// never written and nothing of kitbash's is left behind.
func testRoot(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("delegating a cgroup needs root")
	}
	if _, err := os.Stat(filepath.Join(cgroups.DefaultRoot, "cgroup.subtree_control")); err != nil {
		t.Skipf("%s is not a cgroup v2 mount: %v", cgroups.DefaultRoot, err)
	}
	dir := filepath.Join(cgroups.DefaultRoot, "kitbash-test-"+strconv.Itoa(os.Getpid()))
	if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
		t.Skipf("%s could not be created: %v", dir, err)
	}
	t.Cleanup(func() {
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			if entry.IsDir() {
				removeAll(filepath.Join(dir, entry.Name()))
			}
		}
		if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
			t.Errorf("the test cgroup %s was left behind: %v", dir, err)
		}
	})
	return dir
}

// removeAll takes one cgroup subtree away, deepest first, the way the kernel
// allows: rmdir on empty cgroups and nothing recursive over its own files.
func removeAll(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if entry.IsDir() {
			removeAll(filepath.Join(dir, entry.Name()))
		}
	}
	os.Remove(dir)
}

// nobody is an unprivileged account to delegate to. It is never root: root
// owning a delegated cgroup would prove nothing about what a member may do.
func nobody(t *testing.T) (uid, gid int) {
	t.Helper()
	const unprivileged = 65534
	return unprivileged, unprivileged
}

// read is the trimmed content of one cgroup file.
func read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(body))
}

// ownerOf is the uid one path belongs to.
func ownerOf(t *testing.T, path string) int {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s has no owner", path)
	}
	return int(stat.Uid)
}
