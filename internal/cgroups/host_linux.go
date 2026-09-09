//go:build linux

package cgroups

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// logger writes where the operator reads, the same shape internal/daemon uses.
var logger = log.New(os.Stderr, "kitbashd: ", log.LstdFlags)

// cgroup2Magic identifies the unified hierarchy, from the kernel's magic.h. A
// version 1 mount, or no cgroup filesystem at all, is a host where limits are
// recorded and not enforced.
const cgroup2Magic = 0x63677270

// The interface files this package writes. The first three are what the
// kernel's delegation convention says may be handed to a member; the last
// three are the ceiling, and they stay root's.
const (
	procsFile   = "cgroup.procs"
	threadsFile = "cgroup.threads"
	subtreeFile = "cgroup.subtree_control"

	memoryMax = "memory.max"
	cpuMax    = "cpu.max"
	pidsMax   = "pids.max"
)

// delegated are the three files one member is given on a cgroup of theirs,
// beside the directory itself. Nothing else is chowned, which is what keeps
// the ceiling out of their reach.
var delegated = []string{procsFile, threadsFile, subtreeFile}

// host is the real Cgroups. It reports what it could not do and turns nothing
// off: whether a Process runs unplaced is the caller's decision to make, on
// that start, and a host that refuses one member may serve the next.
type host struct{ root string }

func newHost(root string) Cgroups { return &host{root: root} }

// EnsureRoot creates /sys/fs/cgroup/kitbash and delegates the controllers to
// it, so a member's cgroup below it can carry the limits of a Process.
func (h *host) EnsureRoot(context.Context) error {
	if !isCgroup2(h.root) {
		return fmt.Errorf("cgroups: %s is not a cgroup v2 mount", h.root)
	}
	// The controllers have to be in the parent's subtree_control before a
	// child can enable them. A parent that already delegates them is left
	// alone, and one that refuses is not fatal on its own: what matters is
	// whether the kitbash cgroup ends up with them, which is checked next.
	if err := delegate(h.root); err != nil {
		logger.Printf("cgroups: %s does not delegate %s: %v",
			h.root, strings.Join(Controllers, ", "), err)
	}
	dir := filepath.Join(h.root, Dir)
	if err := mkdir(dir); err != nil {
		return err
	}
	return delegate(dir)
}

// EnsureMember creates one member's cgroup and the leaf their own processes
// run in, and answers the leaf. The cgroup's directory stays root's: it holds
// the ceiling of each of their Processes, and a member who could write here
// could make a cgroup of their own with no ceiling in it. Its cgroup.procs and
// cgroup.threads are handed over, because moving a process between two cgroups
// under here needs write access to the common ancestor's, which this is:
// without it a member's session cannot exec into their own container.
//
// Handing over that one file lets a member move a process between the cgroups
// under here and nowhere else, and it does not let them park one outside a
// ceiling. Writing a pid into this cgroup itself is refused by the kernel: it
// has controllers in its cgroup.subtree_control, which only root may change,
// and a cgroup that delegates controllers may hold no processes of its own.
// Every other destination is either root's, which they cannot write, or under
// a ceiling, which is where the process already was.
func (h *host) EnsureMember(ctx context.Context, name string, uid, gid int) (string, error) {
	if !member.MatchString(name) {
		return "", errName(name)
	}
	if uid <= 0 || gid <= 0 {
		return "", fmt.Errorf("cgroups: %s has no uid and gid to run Processes as", name)
	}
	if err := h.EnsureRoot(ctx); err != nil {
		return "", err
	}
	dir := MemberDir(h.root, name)
	if err := mkdir(dir); err != nil {
		return "", err
	}
	if err := delegate(dir); err != nil {
		return "", err
	}
	// The ownership is set, not assumed. A host upgraded from a release that
	// handed a member their whole subtree still has this directory as theirs,
	// and a member who can write it can make a cgroup beside the ceilings with
	// no limits in it. Setting it every time is what makes the fix arrive with
	// the daemon rather than with a reinstall.
	if err := takeBack(dir); err != nil {
		return "", err
	}
	if err := handOverFiles(dir, uid, gid, procsFile, threadsFile); err != nil {
		return "", err
	}
	// The leaf holds the member's own processes: their MCP sessions and the
	// podman children that start and stop their containers. It stays root's in
	// full, because kitbashd is what puts anything in it.
	leaf := LeafDir(h.root, name)
	if err := mkdir(leaf); err != nil {
		return "", err
	}
	if err := takeBack(leaf); err != nil {
		return "", err
	}
	if err := takeBackFiles(leaf, delegated...); err != nil {
		return "", err
	}
	// And the ceilings of the Processes already in here: the limit files are
	// root's, whoever a previous release gave them to.
	return leaf, takeBackCeilings(dir)
}

// EnsureProcess creates the ceiling of one Process, writes its limits as root
// and hands the member the little they need to start a container under it: the
// directory, so their rootless podman can create the container's own cgroup,
// and the three delegation files, so it can move the container into it. The
// limit files are not among them.
//
// What it answers is the member's leaf, not this cgroup: the podman child that
// creates the container is not the workload, and under the ceiling it would
// spend the memory the manifest meant for the container.
func (h *host) EnsureProcess(ctx context.Context, name, id string, uid, gid int, limits Limits) (string, error) {
	if !processID.MatchString(id) {
		return "", errName(id)
	}
	leaf, err := h.EnsureMember(ctx, name, uid, gid)
	if err != nil {
		return "", err
	}
	dir := ProcessDir(h.root, name, id)
	if err := mkdir(dir); err != nil {
		return "", err
	}
	// The ceiling is written before anything can run under it, and by root.
	if err := writeLimits(dir, limits); err != nil {
		return "", err
	}
	// The controllers for the container's own cgroup, written while this one
	// holds no processes, which is the only time the kernel takes it.
	if err := delegate(dir); err != nil {
		return "", err
	}
	if err := handOver(dir, uid, gid); err != nil {
		return "", err
	}
	return leaf, nil
}

// JoinSession puts one of the member's own processes in their leaf. A session
// cannot do this for itself: it starts in sshd's cgroup, and the only ancestor
// that has in common with anything of kitbash's is the root cgroup, which is
// root's.
func (h *host) JoinSession(ctx context.Context, name string, uid, gid, pid int) (string, error) {
	if pid <= 0 {
		return "", errPID(pid)
	}
	// The pid is what SO_PEERCRED said when the socket was opened, and the
	// kernel reuses pids: by now it may name a process of somebody else's,
	// which this would move into a member's cgroup. What the process is now
	// decides, and a process that has already gone is not there to move.
	if err := ownedBy(pid, uid); err != nil {
		return "", err
	}
	leaf, err := h.EnsureMember(ctx, name, uid, gid)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(leaf, procsFile), []byte(strconv.Itoa(pid)), 0); err != nil {
		return "", fmt.Errorf("cgroups: place %d in %s: %w", pid, leaf, err)
	}
	return leaf, nil
}

// RemoveProcess removes the ceiling of one Process. A cgroup that still holds
// something is busy, which is a container that has not gone yet; the caller
// logs it and the next removal or the next boot clears it.
func (h *host) RemoveProcess(_ context.Context, name, id string) error {
	if !member.MatchString(name) || !processID.MatchString(id) {
		return errName(name + "/" + id)
	}
	return removeTree(ProcessDir(h.root, name, id))
}

// RemoveMember removes a member's cgroup and everything left in it, which is
// what the account going away leaves behind.
func (h *host) RemoveMember(_ context.Context, name string) error {
	if !member.MatchString(name) {
		return errName(name)
	}
	return removeTree(MemberDir(h.root, name))
}

// ownedBy reports whether one process belongs to a uid. The kernel says so
// through /proc/<pid>, which it owns by the process's effective user: a
// process that has exited has no directory there, and one that was replaced by
// a recycled pid has somebody else's.
func ownedBy(pid, uid int) error {
	info, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid)))
	if err != nil {
		return fmt.Errorf("%w: %d: %v", ErrNotOwned, pid, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: %d has no owner", ErrNotOwned, pid)
	}
	if int(stat.Uid) != uid {
		return fmt.Errorf("%w: %d belongs to uid %d, not %d", ErrNotOwned, pid, stat.Uid, uid)
	}
	return nil
}

// isCgroup2 reports whether a path is on the unified hierarchy.
func isCgroup2(path string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false
	}
	return int64(st.Type) == cgroup2Magic
}

// delegate enables the controllers on one cgroup, writing only the ones it
// does not have yet. A cgroup.subtree_control write is refused once the cgroup
// holds processes, so the smallest write is the one most likely to be taken,
// and a controller that is already delegated needs no write at all.
func delegate(dir string) error {
	enabled, err := os.ReadFile(filepath.Join(dir, subtreeFile))
	if err != nil {
		return fmt.Errorf("cgroups: read %s: %w", filepath.Join(dir, subtreeFile), err)
	}
	have := map[string]bool{}
	for _, name := range strings.Fields(string(enabled)) {
		have[name] = true
	}
	var missing []string
	for _, name := range Controllers {
		if !have[name] {
			missing = append(missing, "+"+name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	// One write, because the kernel takes the whole line or none of it.
	if err := os.WriteFile(filepath.Join(dir, subtreeFile), []byte(strings.Join(missing, " ")), 0); err != nil {
		return fmt.Errorf("cgroups: write %s: %w", filepath.Join(dir, subtreeFile), err)
	}
	return nil
}

// writeLimits writes the ceiling of one Process. The files stay root's,
// whatever is handed over afterwards.
func writeLimits(dir string, limits Limits) error {
	values := map[string]string{}
	if limits.Memory != "" {
		values[memoryMax] = limits.Memory
	}
	if limits.CPU != "" {
		values[cpuMax] = limits.CPU
	}
	if limits.Pids > 0 {
		values[pidsMax] = strconv.Itoa(limits.Pids)
	}
	for _, file := range []string{memoryMax, cpuMax, pidsMax} {
		value, ok := values[file]
		if !ok {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, file), []byte(value), 0); err != nil {
			return fmt.Errorf("cgroups: write %s: %w", filepath.Join(dir, file), err)
		}
	}
	return nil
}

// handOver gives one member the directory of a cgroup and the three files the
// kernel's delegation convention names, and nothing else. memory.max, cpu.max
// and pids.max are deliberately not in that list: they are the ceiling, and a
// member who could write them could raise their own limit.
func handOver(dir string, uid, gid int) error {
	if err := os.Chown(dir, uid, gid); err != nil {
		return fmt.Errorf("cgroups: give %s to %d: %w", dir, uid, err)
	}
	return handOverFiles(dir, uid, gid, delegated...)
}

// handOverFiles gives one member named files of a cgroup without the directory
// they are in, which is how a member may move a process between two cgroups
// they cannot create anything in.
func handOverFiles(dir string, uid, gid int, files ...string) error {
	for _, file := range files {
		path := filepath.Join(dir, file)
		if err := os.Chown(path, uid, gid); err != nil {
			// cgroup.threads is absent on a kernel built without thread
			// granularity, which is not a delegation this package needs.
			if file == threadsFile && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("cgroups: give %s to %d: %w", path, uid, err)
		}
	}
	return nil
}

// takeBack makes one cgroup directory root's again. It is what an upgrade
// needs: a directory an earlier release handed to a member is a hole in the
// ceiling, and nothing else here would close it.
func takeBack(dir string) error {
	if err := os.Chown(dir, 0, 0); err != nil {
		return fmt.Errorf("cgroups: take %s back: %w", dir, err)
	}
	return nil
}

// takeBackFiles makes named files of a cgroup root's again.
func takeBackFiles(dir string, files ...string) error {
	for _, file := range files {
		path := filepath.Join(dir, file)
		if err := os.Chown(path, 0, 0); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("cgroups: take %s back: %w", path, err)
		}
	}
	return nil
}

// takeBackCeilings makes the limit files of every Process cgroup under one
// member root's again. The directory of a Process cgroup is the member's, by
// design: that is how their podman creates the container's cgroup. The three
// files that say what it may spend are not, and a release that gave them away
// is corrected here rather than left until the Process is run again.
func takeBackCeilings(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("cgroups: read %s: %w", dir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !processID.MatchString(entry.Name()) {
			continue
		}
		if err := takeBackFiles(filepath.Join(dir, entry.Name()), memoryMax, cpuMax, pidsMax); err != nil {
			return err
		}
	}
	return nil
}

// mkdir creates one cgroup directory, which the kernel fills with the
// interface files. A directory that is already there is the desired state.
func mkdir(dir string) error {
	if err := os.Mkdir(dir, DirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("cgroups: create %s: %w", dir, err)
	}
	return nil
}

// removeTree removes one cgroup and the cgroups under it, deepest first. A
// cgroup directory is removed with rmdir and never with anything recursive:
// the kernel's own files are not deletable, and only an empty cgroup goes.
func removeTree(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cgroups: read %s: %w", dir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := removeTree(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cgroups: remove %s: %w", dir, err)
	}
	return nil
}
