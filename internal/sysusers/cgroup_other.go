//go:build !linux

package sysusers

import (
	"fmt"
	"syscall"
)

// place refuses on a machine that is not Linux: there is no cgroup to put a
// child in. internal/cgroups answers no leaf there, so the caller never asks,
// and a caller that does is told rather than silently running unplaced.
func place(_ *syscall.SysProcAttr, dir string) (closer func(), err error) {
	if dir == "" {
		return func() {}, nil
	}
	return func() {}, fmt.Errorf("sysusers: placing a child in the cgroup %s needs Linux", dir)
}
