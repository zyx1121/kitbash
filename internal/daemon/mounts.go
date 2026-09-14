package daemon

import (
	"context"

	"github.com/zyx1121/kitbash/internal/mounts"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// The mounts of a Process are resolved here, as root, and nowhere else that
// counts. kitbash-mcp runs the same check as the member first so the problem
// arrives in the session that caused it, but that process is the member's and
// what it says about a path is a claim: the daemon is the one that decides,
// see mounts in spec/kitbashd-api.yaml.
//
// The check runs twice, at registration and at every start, because the answer
// can change in between: a folder can be removed, replaced by a symlink or
// lose its manifest while the Process is registered and not running. A restore
// takes the same path as a start, so a mount that has stopped being legal
// fails the restore and proc_list reports that Process failed with the reason.

// mountChecker is the checker for one member on this host: their own home, the
// shared root and the uid every source under the home has to be owned by.
func (s *Server) mountChecker(m sysusers.Member) *mounts.Checker {
	home := m.Home
	if home == "" {
		home = mounts.HomesRoot + "/" + m.Name
	}
	return &mounts.Checker{
		Owner: m.Name,
		Home:  home,
		Org:   s.orgRoot,
		UID:   m.UID,
		Homes: mounts.HomesRoot,
	}
}

// resolveMounts checks what a registration declared and answers what it
// resolves to, which is what goes into the store. A member this host does not
// know is refused: every rule is about a member that exists, and a Process runs
// as one.
func (s *Server) resolveMounts(ctx context.Context, instance, owner string, declared []mounts.Declared) ([]mounts.Resolved, *problem.Problem) {
	if len(declared) == 0 {
		return nil, nil
	}
	m, found, err := s.users.Lookup(ctx, owner)
	if err != nil {
		return nil, problem.Internal(instance, err.Error(), "")
	}
	if !found {
		return nil, problem.NotPermitted(instance,
			owner+" is not a member of this host, so kitbashd cannot say which folders they own",
			"Ask an administrator to create a member for this account.")
	}
	return s.mountChecker(m).Resolve(instance, declared)
}

// revalidateMounts resolves what a registration already holds, before the
// container is created. The stored source is already the resolved path, so
// this is the same question asked again rather than a second interpretation of
// the manifest: what it catches is a folder that has changed since.
func (s *Server) revalidateMounts(instance string, p store.Process, m sysusers.Member) ([]mounts.Resolved, *problem.Problem) {
	if len(p.Mounts) == 0 {
		return nil, nil
	}
	return s.mountChecker(m).Resolve(instance, mounts.Redeclare(p.Mounts))
}

// mountProblem is what restore records about a Process whose mount is no
// longer legal, in the two sentences proc_list carries. The detail is the
// member's own problem rather than the daemon's words for it: they wrote the
// manifest and they can move the folder back.
func mountProblem(prob *problem.Problem) restoreProblem {
	fix := prob.Fix
	if fix == "" {
		fix = "Fix the folder deploy.units[0].mounts names, then run the Package again."
	}
	return restoreProblem{
		Detail: "this Process declares a mount that is no longer legal, so kitbashd did not start it: " + prob.Detail,
		Fix:    fix,
	}
}
