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
		log.New(os.Stderr, "kitbashd: ", log.LstdFlags).Printf(
			"cgroups: %s does not delegate %s: %v", h.root, strings.Join(Controllers, ", "), err)
	}
	dir := filepath.Join(h.root, Dir)
	if err := mkdir(dir); err != nil {
		return err
	}
	return delegate(dir)
}

// EnsureMember creates one member's cgroup. It belongs to root in full: it
// holds the cgroup of each of their Processes, and a member who could write
// here could make a Process cgroup of their own with no ceiling in it.
func (h *host) EnsureMember(ctx context.Context, name string, uid, gid int) error {
	if !member.MatchString(name) {
		return errName(name)
	}
	if uid <= 0 || gid <= 0 {
		return fmt.Errorf("cgroups: %s has no uid and gid to run Processes as", name)
	}
	if err := h.EnsureRoot(ctx); err != nil {
		return err
	}
	dir := MemberDir(h.root, name)
	if err := mkdir(dir); err != nil {
		return err
	}
	return delegate(dir)
}

// EnsureProcess creates the cgroup of one Process, writes its ceiling as root
// and hands the member the little they need to start a container under it: the
// directory, so their rootless podman can create the container's own cgroup,
// and the three delegation files, so it can move the container into it. The
// limit files are not among them.
func (h *host) EnsureProcess(ctx context.Context, name, id string, uid, gid int, limits Limits) (string, error) {
	if !processID.MatchString(id) {
		return "", errName(id)
	}
	if err := h.EnsureMember(ctx, name, uid, gid); err != nil {
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
	leaf := filepath.Join(dir, Leaf)
	if err := mkdir(leaf); err != nil {
		return "", err
	}
	if err := handOver(dir, uid, gid); err != nil {
		return "", err
	}
	if err := handOver(leaf, uid, gid); err != nil {
		return "", err
	}
	return leaf, nil
}

// RemoveProcess removes the cgroup of one Process. A cgroup that still holds
// something is busy, which is a container that has not gone yet; the caller
// logs it and the next removal or the next boot clears it.
func (h *host) RemoveProcess(_ context.Context, name, id string) error {
	if !member.MatchString(name) || !processID.MatchString(id) {
		return errName(name + "/" + id)
	}
	return removeTree(ProcessDir(h.root, name, id))
}

// RemoveMember removes a member's cgroup and every Process cgroup left in it,
// which is what the account going away leaves behind.
func (h *host) RemoveMember(_ context.Context, name string) error {
	if !member.MatchString(name) {
		return errName(name)
	}
	return removeTree(MemberDir(h.root, name))
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
	for _, file := range delegated {
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
