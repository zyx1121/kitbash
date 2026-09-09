// Package cgroups places a member's Processes in a delegated cgroup, which is
// what makes the manifest's limits an enforcement rather than a record, see
// PLAN.md section 2.3.
//
// A member cannot move their own process into a delegated cgroup: that needs
// write access to the common ancestor, which a rootless session does not have.
// kitbashd is root, so it creates the tree, hands the member's subtree to
// them, and starts every podman child inside their leaf. The container itself
// lands in a child of the same subtree through --cgroup-parent.
//
// The layout, one per member:
//
//	/sys/fs/cgroup/kitbash                 root only, memory cpu pids delegated
//	/sys/fs/cgroup/kitbash/<member>        owned by the member, same delegation
//	/sys/fs/cgroup/kitbash/<member>/run    the leaf every podman child runs in
//
// Processes live in a leaf because a cgroup that holds processes may not
// enable controllers for its children, and the containers of one member are
// children of <member>, not of the leaf.
//
// Everything that touches the cgroup filesystem lives behind Cgroups, so the
// daemon is tested on a machine that is not root and has no writable cgroup
// mount. The real implementation is Linux only; anywhere else, and on a host
// whose cgroup filesystem is not version 2 or not writable, limits are
// recorded and not enforced, which is said once in the daemon log.
package cgroups

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sync"
)

// ErrName reports a name that is not a member name. It becomes a directory
// under the cgroup filesystem, so nothing else is created.
var ErrName = errors.New("cgroups: not a member name")

func errName(name string) error { return fmt.Errorf("%w: %q", ErrName, name) }

// DefaultRoot is where the unified hierarchy is mounted on a kitbash host.
const DefaultRoot = "/sys/fs/cgroup"

// Dir is the cgroup kitbashd owns under the root, and Leaf the child of one
// member's cgroup that holds their processes.
const (
	Dir  = "kitbash"
	Leaf = "run"
)

// DirMode is the mode of every cgroup directory kitbashd creates. The member's
// own subtree is chowned to them afterwards; the mode is what the kernel
// expects of a cgroup directory either way.
const DirMode = 0o755

// Controllers are what kitbashd delegates to a member, in the order they are
// written to cgroup.subtree_control. They are the three the manifest's limits
// map onto: memory for limits.memory, cpu for limits.cpu and pids for the
// bound kitbashd puts on every Process.
var Controllers = []string{"memory", "cpu", "pids"}

// member is the name shape a cgroup directory may carry. It is the member name
// spec/mcp-surface.yaml declares, checked again here because this name becomes
// a path under the cgroup filesystem.
var member = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// Cgroups is the cgroup tree kitbashd places Processes in. Every method is
// safe to call again: the tree is a desired state, not a transaction.
type Cgroups interface {
	// EnsureRoot creates the kitbash cgroup and delegates the controllers to
	// it. The daemon calls it once at start.
	EnsureRoot(ctx context.Context) error
	// EnsureMember creates one member's subtree and their leaf, owned by
	// them, and answers the leaf directory their podman children are started
	// in. An empty leaf means this host enforces no limits: the caller runs
	// the container without placing it, and the manifest's limits are
	// recorded rather than enforced.
	EnsureMember(ctx context.Context, name string, uid, gid int) (leaf string, err error)
	// Enabled reports whether placement is on. A caller that only wants to
	// know whether limits hold does not have to create anything to find out.
	Enabled() bool
}

// Parent is the --cgroup-parent one member's containers are started under. It
// is the member's own cgroup and not the leaf: the leaf holds processes, and a
// cgroup that holds processes cannot be a parent of another one.
//
// The path is the one the container runtime takes, relative to the mount
// point, which is what podman means by an absolute cgroup path.
func Parent(name string) string { return "/" + Dir + "/" + name }

// MemberDir is where one member's subtree lives under a mount point.
func MemberDir(root, name string) string { return filepath.Join(root, Dir, name) }

// LeafDir is where one member's processes are placed.
func LeafDir(root, name string) string { return filepath.Join(MemberDir(root, name), Leaf) }

// New returns the Cgroups of this host, rooted at the cgroup mount point.
// An empty root means DefaultRoot.
func New(root string) Cgroups {
	if root == "" {
		root = DefaultRoot
	}
	return newHost(root)
}

// Fake is an in memory Cgroups. It records what was ensured and answers a leaf
// under Base, so a test asserts on the directory a child would have been
// placed in without a cgroup filesystem anywhere near it.
type Fake struct {
	mu sync.Mutex

	// Base stands in for the mount point. Empty means /fake/cgroup.
	Base string
	// Off makes this a host that enforces no limits, which is what a machine
	// without cgroup v2 delegation looks like to the daemon.
	Off bool
	// RootErr and MemberErr fail on demand.
	RootErr   error
	MemberErr error

	// Roots is how many times the root was ensured, and Members every member
	// call in order.
	Roots   int
	Members []MemberCall
}

// MemberCall is one recorded EnsureMember.
type MemberCall struct {
	Name string
	UID  int
	GID  int
	Leaf string
}

// NewFake returns a fake with placement on.
func NewFake() *Fake { return &Fake{} }

// EnsureRoot records the call.
func (f *Fake) EnsureRoot(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RootErr != nil {
		return f.RootErr
	}
	f.Roots++
	return nil
}

// EnsureMember records the call and answers the leaf it would have created.
func (f *Fake) EnsureMember(_ context.Context, name string, uid, gid int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.MemberErr != nil {
		return "", f.MemberErr
	}
	if !member.MatchString(name) {
		return "", errName(name)
	}
	leaf := ""
	if !f.Off {
		leaf = LeafDir(f.base(), name)
	}
	f.Members = append(f.Members, MemberCall{Name: name, UID: uid, GID: gid, Leaf: leaf})
	return leaf, nil
}

// Enabled reports whether this fake places anything.
func (f *Fake) Enabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.Off
}

// Calls answers the recorded member calls, newest last.
func (f *Fake) Calls() []MemberCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]MemberCall(nil), f.Members...)
}

func (f *Fake) base() string {
	if f.Base == "" {
		return "/fake/cgroup"
	}
	return f.Base
}

// disabled is the Cgroups of a host that places nothing: it creates no
// directory and answers no leaf, so every caller runs the container as it did
// before and the limits are a record.
type disabled struct{}

func (disabled) EnsureRoot(context.Context) error { return nil }

func (disabled) EnsureMember(context.Context, string, int, int) (string, error) { return "", nil }

func (disabled) Enabled() bool { return false }
