// Package cgroups places a member's Processes in a delegated cgroup, which is
// what makes the manifest's limits an enforcement rather than a record, see
// PLAN.md section 2.3.
//
// A member cannot move their own process into a delegated cgroup: that needs
// write access to the common ancestor, which a rootless session does not have.
// kitbashd is root, so it builds the tree, writes the limits, hands the member
// only what they need to start a container in it, and starts every podman
// child inside it.
//
// The layout, one ceiling per Process and one leaf per member:
//
//	/sys/fs/cgroup/kitbash                     root, memory cpu pids delegated
//	/sys/fs/cgroup/kitbash/<member>            root, same delegation
//	/sys/fs/cgroup/kitbash/<member>/run        the member's own processes
//	/sys/fs/cgroup/kitbash/<member>/<id>       root, and the limits are here
//	/sys/fs/cgroup/kitbash/<member>/<id>/<ctr> the container, made by crun
//
// The ceiling is the Process's own cgroup and it belongs to root: memory.max,
// cpu.max and pids.max there are files the member cannot write. What they are
// given is the directory of that cgroup and its three delegation files
// (cgroup.procs, cgroup.threads, cgroup.subtree_control), which is exactly
// enough for their rootless podman to create the container's cgroup beneath
// the ceiling and move the container into it. The member's own cgroup keeps
// its directory root's, so nobody can put a Process cgroup beside the ones
// kitbashd made; only its cgroup.procs and cgroup.threads are handed over, and
// that is what lets a process already inside the member's subtree move between
// the cgroups in it, because the kernel checks the common ancestor.
//
// The leaf holds what belongs to the member rather than to one Process: their
// MCP sessions, and the podman child that starts, stops or removes a
// container. Neither is the workload. A podman child under the Process's own
// ceiling would spend the memory the manifest meant for the container, and a
// small limit would kill the starter instead of the thing being started.
//
// The leaf is also what makes podman exec work. Moving a process between
// cgroups needs write access to cgroup.procs of the common ancestor of where
// it is and where it is going, and a member's SSH session starts in sshd's
// cgroup, whose only ancestor in common with anything of kitbash's is the root
// cgroup. So a session asks kitbashd to place it in the leaf, and from there
// the ancestor is /kitbash/<member>, whose cgroup.procs the member has.
//
// Everything that touches the cgroup filesystem lives behind Cgroups, so the
// daemon is tested on a machine that is not root and has no writable cgroup
// mount. The real implementation is Linux only. Nothing here decides what a
// failure means: it reports, and the caller runs the Process with its limits
// recorded rather than enforced and says so, once per start.
package cgroups

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// DefaultRoot is where the unified hierarchy is mounted on a kitbash host.
const DefaultRoot = "/sys/fs/cgroup"

// Dir is the cgroup kitbashd owns under the root, and Leaf the child of one
// member's cgroup that holds their own processes.
const (
	Dir  = "kitbash"
	Leaf = "run"
)

// DirMode is the mode of every cgroup directory kitbashd creates.
const DirMode = 0o755

// Controllers are what kitbashd delegates, in the order they are written to
// cgroup.subtree_control. They are the three the manifest's limits map onto:
// memory for limits.memory, cpu for limits.cpu and pids for the bound kitbashd
// puts on every Process.
var Controllers = []string{"memory", "cpu", "pids"}

// CPUPeriod is the accounting window cpu.max is written against, in
// microseconds. It is the kernel's own default; the quota is what a limit
// changes.
const CPUPeriod = 100000

// The two names this package turns into paths. Both are checked again here
// because they become directories under the cgroup filesystem.
var (
	member    = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	processID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// The failures a caller distinguishes.
var (
	// ErrName reports a name that is not a member name or a Process id.
	ErrName = errors.New("cgroups: not a name this package makes a cgroup for")
	// ErrPID reports a process id that is not one, which is nothing this
	// package writes into a cgroup.
	ErrPID = errors.New("cgroups: not a process id")
	// ErrUnsupported reports a machine with no cgroup filesystem to place
	// anything in, which is anything that is not Linux.
	ErrUnsupported = errors.New("cgroups: placing a Process needs Linux")
)

func errName(name string) error { return fmt.Errorf("%w: %q", ErrName, name) }

func errPID(pid int) error { return fmt.Errorf("%w: %d", ErrPID, pid) }

// Limits is the ceiling one Process runs under, already in the spelling the
// cgroup files take. An empty field writes nothing, which leaves the kernel's
// own default of max.
type Limits struct {
	// Memory is memory.max, in bytes.
	Memory string
	// CPU is cpu.max, a quota and a period in microseconds.
	CPU string
	// Pids is pids.max.
	Pids int
}

// Empty reports whether this ceiling limits nothing.
func (l Limits) Empty() bool { return l.Memory == "" && l.CPU == "" && l.Pids <= 0 }

// Cgroups is the cgroup tree kitbashd places Processes in. Every method is
// safe to call again: the tree is a desired state, not a transaction.
//
// Every method reports what went wrong rather than turning itself off. A host
// that cannot delegate is a decision for the caller, which runs the Process
// unplaced and says so on that start, not once for the life of the daemon.
type Cgroups interface {
	// EnsureRoot creates the kitbash cgroup and delegates the controllers to
	// it. The daemon calls it at start, and every other method calls it.
	EnsureRoot(ctx context.Context) error
	// EnsureMember creates one member's cgroup and their leaf, and answers the
	// leaf: where their own processes go, which is their MCP sessions and the
	// podman children that start and stop their containers.
	EnsureMember(ctx context.Context, name string, uid, gid int) (leaf string, err error)
	// EnsureProcess creates the ceiling of one Process, with its limits
	// written by root, and answers the member's leaf, because that is where
	// the podman child that starts the container belongs: the child is not the
	// workload and must not spend the workload's memory.
	EnsureProcess(ctx context.Context, name, id string, uid, gid int, limits Limits) (leaf string, err error)
	// JoinSession puts one of the member's own processes, an MCP session, in
	// their leaf and answers where it went. Only root can: the session starts
	// in sshd's cgroup, and the only ancestor that has in common with anything
	// of kitbash's is the root cgroup.
	JoinSession(ctx context.Context, name string, uid, gid, pid int) (cgroup string, err error)
	// RemoveProcess removes the ceiling of one Process, which the daemon does
	// once its container is gone.
	RemoveProcess(ctx context.Context, name, id string) error
	// RemoveMember removes a member's cgroup and everything left in it, which
	// the daemon does when the account goes.
	RemoveMember(ctx context.Context, name string) error
}

// Parent is the --cgroup-parent one Process's container is created under: the
// Process's own cgroup, below the ceiling and above the container. The path is
// relative to the mount point, which is what podman means by an absolute
// cgroup path.
func Parent(name, id string) string { return "/" + Dir + "/" + name + "/" + id }

// MemberDir is where one member's cgroup lives under a mount point.
func MemberDir(root, name string) string { return filepath.Join(root, Dir, name) }

// LeafDir is where a member's own processes are placed: their sessions and the
// podman children of their Processes. It is beside the ceilings rather than
// under one, because a session outlives every Process it starts and a podman
// child is not the workload it starts.
func LeafDir(root, name string) string { return filepath.Join(MemberDir(root, name), Leaf) }

// ProcessDir is where the ceiling of one Process lives.
func ProcessDir(root, name, id string) string { return filepath.Join(MemberDir(root, name), id) }

// New returns the Cgroups of this host, rooted at the cgroup mount point.
// An empty root means DefaultRoot.
func New(root string) Cgroups {
	if root == "" {
		root = DefaultRoot
	}
	return newHost(root)
}

// MemoryMax converts one limits.memory value into the bytes memory.max takes.
// It reads both spellings the value passes through: the manifest's Ki, Mi and
// Gi and the runtime's k, m and g. Anything else is not a size, and the second
// return says so rather than writing a number nobody meant.
func MemoryMax(memory string) (string, bool) {
	memory = strings.TrimSpace(memory)
	if memory == "" {
		return "", false
	}
	multiplier := int64(1)
	digits := memory
	for suffix, scale := range map[string]int64{
		"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30,
	} {
		if strings.HasSuffix(memory, suffix) {
			multiplier = scale
			digits = strings.TrimSuffix(memory, suffix)
		}
	}
	if multiplier == 1 {
		for suffix, scale := range map[string]int64{
			"k": 1 << 10, "m": 1 << 20, "g": 1 << 30,
		} {
			if strings.HasSuffix(memory, suffix) {
				multiplier = scale
				digits = strings.TrimSuffix(memory, suffix)
			}
		}
	}
	value, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || value <= 0 {
		return "", false
	}
	return strconv.FormatInt(value*multiplier, 10), true
}

// CPUMax converts one limits.cpu value, a number of cores, into the quota and
// period cpu.max takes.
func CPUMax(cpus string) (string, bool) {
	cpus = strings.TrimSpace(cpus)
	if cpus == "" {
		return "", false
	}
	cores, err := strconv.ParseFloat(cpus, 64)
	if err != nil || cores <= 0 {
		return "", false
	}
	quota := int64(cores * CPUPeriod)
	if quota <= 0 {
		return "", false
	}
	return strconv.FormatInt(quota, 10) + " " + strconv.Itoa(CPUPeriod), true
}

// Fake is an in memory Cgroups. It records what was ensured, joined and
// removed and answers the paths a real host would, so a test asserts on the
// cgroup a child would have been placed in without a cgroup filesystem
// anywhere near it.
type Fake struct {
	mu sync.Mutex

	// Base stands in for the mount point. Empty means /fake/cgroup.
	Base string
	// Off makes this a host that cannot delegate: every call fails the way a
	// cgroup filesystem that refuses would, which is what the caller runs a
	// Process unplaced for.
	Off bool
	// RootErr, MemberErr, ProcessErr, SessionErr and RemoveErr fail on demand.
	RootErr    error
	MemberErr  error
	ProcessErr error
	SessionErr error
	RemoveErr  error

	// Roots is how many times the root was ensured, Members, Processes and
	// Joined every call in order, and Removed every removal.
	Roots     int
	Members   []MemberCall
	Processes []ProcessCall
	Joined    []SessionCall
	Removed   []ProcessCall
}

// MemberCall is one recorded EnsureMember or RemoveMember.
type MemberCall struct {
	Name string
	UID  int
	GID  int
	Leaf string
}

// ProcessCall is one recorded EnsureProcess or RemoveProcess, with the limits
// that were written into the Process's ceiling and the leaf its podman child
// was told to start in.
type ProcessCall struct {
	Name   string
	ID     string
	UID    int
	GID    int
	Limits Limits
	Leaf   string
}

// SessionCall is one recorded JoinSession: whose session it is, which process
// was placed and where it went.
type SessionCall struct {
	Name   string
	UID    int
	GID    int
	PID    int
	Cgroup string
}

// NewFake returns a fake that places what it is asked to.
func NewFake() *Fake { return &Fake{} }

// ErrFakeOff is what every call of a fake with Off set answers.
var ErrFakeOff = errors.New("cgroups: this host does not delegate cgroups")

// EnsureRoot records the call.
func (f *Fake) EnsureRoot(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.refuse(f.RootErr); err != nil {
		return err
	}
	f.Roots++
	return nil
}

// EnsureMember records the call and answers the leaf it would have created.
func (f *Fake) EnsureMember(_ context.Context, name string, uid, gid int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.refuse(f.MemberErr); err != nil {
		return "", err
	}
	if !member.MatchString(name) {
		return "", errName(name)
	}
	leaf := LeafDir(f.base(), name)
	f.Members = append(f.Members, MemberCall{Name: name, UID: uid, GID: gid, Leaf: leaf})
	return leaf, nil
}

// EnsureProcess records the call and answers the member's leaf.
func (f *Fake) EnsureProcess(_ context.Context, name, id string, uid, gid int, limits Limits) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.refuse(f.ProcessErr); err != nil {
		return "", err
	}
	if !member.MatchString(name) {
		return "", errName(name)
	}
	if !processID.MatchString(id) {
		return "", errName(id)
	}
	leaf := LeafDir(f.base(), name)
	f.Processes = append(f.Processes, ProcessCall{
		Name: name, ID: id, UID: uid, GID: gid, Limits: limits, Leaf: leaf,
	})
	return leaf, nil
}

// JoinSession records the call and answers the leaf it would have written the
// pid into.
func (f *Fake) JoinSession(_ context.Context, name string, uid, gid, pid int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.refuse(f.SessionErr); err != nil {
		return "", err
	}
	if !member.MatchString(name) {
		return "", errName(name)
	}
	if pid <= 0 {
		return "", errPID(pid)
	}
	leaf := LeafDir(f.base(), name)
	f.Joined = append(f.Joined, SessionCall{Name: name, UID: uid, GID: gid, PID: pid, Cgroup: leaf})
	return leaf, nil
}

// RemoveProcess records the removal of one Process's ceiling.
func (f *Fake) RemoveProcess(_ context.Context, name, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RemoveErr != nil {
		return f.RemoveErr
	}
	f.Removed = append(f.Removed, ProcessCall{Name: name, ID: id})
	return nil
}

// RemoveMember records the removal of a member's whole cgroup.
func (f *Fake) RemoveMember(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RemoveErr != nil {
		return f.RemoveErr
	}
	f.Removed = append(f.Removed, ProcessCall{Name: name})
	return nil
}

// Calls answers the recorded member calls, newest last.
func (f *Fake) Calls() []MemberCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]MemberCall(nil), f.Members...)
}

// Placed answers the recorded Process calls, newest last.
func (f *Fake) Placed() []ProcessCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ProcessCall(nil), f.Processes...)
}

// Sessions answers the recorded joins, newest last.
func (f *Fake) Sessions() []SessionCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SessionCall(nil), f.Joined...)
}

// Removals answers the recorded removals, newest last.
func (f *Fake) Removals() []ProcessCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ProcessCall(nil), f.Removed...)
}

// refuse is the failure a fake with Off set answers, or the one a test staged.
// The caller holds the lock.
func (f *Fake) refuse(staged error) error {
	if f.Off {
		return ErrFakeOff
	}
	return staged
}

func (f *Fake) base() string {
	if f.Base == "" {
		return "/fake/cgroup"
	}
	return f.Base
}
