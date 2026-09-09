//go:build linux

package sysusers

import (
	"fmt"
	"os"
	"syscall"
)

// place starts one child inside a cgroup: the kernel takes a file descriptor
// on the cgroup directory and puts the new process there before it execs, so
// the child is never in the daemon's own cgroup, not even for an instant.
//
// A member cannot do this for themselves. Moving a process into a delegated
// cgroup needs write access to the common ancestor, which is root's, so
// kitbashd is the one that places the podman child and the container inherits
// the placement through --cgroup-parent, see internal/cgroups.
//
// The returned close releases the descriptor. The caller holds it until the
// child has started, which for an exec.Cmd that is run to completion is until
// it has finished.
func place(attr *syscall.SysProcAttr, dir string) (closer func(), err error) {
	if dir == "" {
		return func() {}, nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return func() {}, fmt.Errorf("sysusers: open the cgroup %s: %w", dir, err)
	}
	attr.UseCgroupFD = true
	attr.CgroupFD = int(f.Fd())
	return func() { f.Close() }, nil
}
