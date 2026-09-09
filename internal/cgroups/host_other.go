//go:build !linux

package cgroups

import "context"

// unsupported is the Cgroups of a machine that is not Linux: there is no
// cgroup filesystem to place anything in. Every call says so, and the caller
// runs the Process with its limits recorded rather than enforced. kitbashd
// runs on Linux; this exists so the daemon builds and its tests run anywhere.
type unsupported struct{}

func newHost(string) Cgroups { return unsupported{} }

func (unsupported) EnsureRoot(context.Context) error { return ErrUnsupported }

func (unsupported) EnsureMember(context.Context, string, int, int) error { return ErrUnsupported }

func (unsupported) EnsureProcess(context.Context, string, string, int, int, Limits) (string, error) {
	return "", ErrUnsupported
}

func (unsupported) RemoveProcess(context.Context, string, string) error { return ErrUnsupported }

func (unsupported) RemoveMember(context.Context, string) error { return ErrUnsupported }
