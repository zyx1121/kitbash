//go:build !linux

package cgroups

// newHost is the Cgroups of a machine that is not Linux: there is no cgroup
// filesystem to place anything in, so limits are recorded and not enforced.
// kitbashd runs on Linux; this exists so the daemon builds and its tests run
// anywhere.
func newHost(string) Cgroups { return disabled{} }
