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
	"strings"
	"sync"
	"syscall"
)

// logger writes where the operator reads, the same shape internal/daemon uses.
var logger = log.New(os.Stderr, "kitbashd: ", log.LstdFlags)

// cgroup2Magic identifies the unified hierarchy, from the kernel's
// magic.h. A version 1 mount, or no cgroup filesystem at all, is a host where
// limits are recorded and not enforced.
const cgroup2Magic = 0x63677270

// The two files of the cgroup interface this package writes.
const (
	subtreeFile = "cgroup.subtree_control"
	procsFile   = "cgroup.procs"
)

// host is the real Cgroups. It turns itself off the first time the cgroup
// filesystem refuses it, and says so once: a host without delegation is a host
// where limits are a record, which is worth one line and not one per Process.
type host struct {
	root string

	mu  sync.Mutex
	off bool
}

func newHost(root string) Cgroups { return &host{root: root} }

// EnsureRoot creates /sys/fs/cgroup/kitbash and delegates the controllers to
// it. It never fails the caller: a cgroup filesystem that will not take this
// tree is a host that enforces no limits, not a daemon that cannot start.
func (h *host) EnsureRoot(context.Context) error {
	if !h.Enabled() {
		return nil
	}
	if !isCgroup2(h.root) {
		h.disable(fmt.Sprintf("%s is not a cgroup v2 mount", h.root))
		return nil
	}
	// The controllers have to be in the parent's subtree_control before a
	// child can enable them. A root that already delegates them is left
	// alone, and one that refuses is not fatal: what matters is whether the
	// kitbash cgroup ends up with them, which is checked next.
	if err := delegate(h.root); err != nil {
		logger.Printf("cgroups: %s does not delegate %s: %v",
			h.root, strings.Join(Controllers, ", "), err)
	}
	dir := filepath.Join(h.root, Dir)
	if err := mkdir(dir); err != nil {
		h.disable(fmt.Sprintf("%s could not be created: %v", dir, err))
		return nil
	}
	if err := delegate(dir); err != nil {
		h.disable(fmt.Sprintf("%s does not delegate %s: %v", dir, strings.Join(Controllers, ", "), err))
		return nil
	}
	return nil
}

// EnsureMember creates one member's subtree and the leaf their podman children
// are started in, and hands the subtree to them. The delegation is written
// while the subtree holds no processes, which is the only time the kernel
// takes it.
func (h *host) EnsureMember(ctx context.Context, name string, uid, gid int) (string, error) {
	if !member.MatchString(name) {
		return "", errName(name)
	}
	if uid <= 0 || gid <= 0 {
		return "", fmt.Errorf("cgroups: %s has no uid and gid to own a cgroup", name)
	}
	if err := h.EnsureRoot(ctx); err != nil {
		return "", err
	}
	if !h.Enabled() {
		return "", nil
	}
	dir := MemberDir(h.root, name)
	if err := mkdir(dir); err != nil {
		h.disable(fmt.Sprintf("%s could not be created: %v", dir, err))
		return "", nil
	}
	if err := delegate(dir); err != nil {
		h.disable(fmt.Sprintf("%s does not delegate %s: %v", dir, strings.Join(Controllers, ", "), err))
		return "", nil
	}
	leaf := LeafDir(h.root, name)
	if err := mkdir(leaf); err != nil {
		h.disable(fmt.Sprintf("%s could not be created: %v", leaf, err))
		return "", nil
	}
	// The member owns their whole subtree, which is what lets their rootless
	// podman create the container's own cgroup under it. kitbashd keeps
	// /sys/fs/cgroup/kitbash itself: a member may fill their subtree and no
	// more.
	if err := chownTree(dir, uid, gid); err != nil {
		h.disable(fmt.Sprintf("%s could not be given to %s: %v", dir, name, err))
		return "", nil
	}
	return leaf, nil
}

// Enabled reports whether this host still places Processes.
func (h *host) Enabled() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.off
}

// disable turns placement off and says why, once. Every later call is a no op,
// so a host without delegation costs one log line and not one per Process.
func (h *host) disable(reason string) {
	h.mu.Lock()
	already := h.off
	h.off = true
	h.mu.Unlock()
	if already {
		return
	}
	logger.Printf("cgroups: %s; the limits of every Process are recorded and not enforced", reason)
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

// mkdir creates one cgroup directory, which the kernel fills with the
// interface files. A directory that is already there is the desired state.
func mkdir(dir string) error {
	if err := os.Mkdir(dir, DirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// chownTree gives one member their subtree: the directories and the interface
// files in them, which is what the kernel's delegation convention asks for.
// Only this member's own cgroups are walked, so the tree is two levels deep.
func chownTree(dir string, uid, gid int) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := os.Chown(path, uid, gid); err != nil {
			// A file the kernel does not let anybody own is not a reason to
			// leave the subtree half given away, but cgroup.procs and
			// cgroup.subtree_control are the two that must succeed.
			if d.IsDir() || d.Name() == procsFile || d.Name() == subtreeFile {
				return err
			}
		}
		return nil
	})
}
