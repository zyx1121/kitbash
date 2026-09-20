package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// Removing a member is a job kitbashd runs for itself, see issue #152.
//
// It used to be the handler's own work, and a member holding seventeen
// Processes outran the client's deadline: the cancellation landed in the
// middle of userdel, the account was gone and the archive step answered 500,
// and what was left was half a member nobody could finish removing. A deadline
// belongs to a request and this is not the length of a request.
//
// So the handler refuses what it has always refused, marks the member removing
// and answers 202. What follows runs here, on the daemon's own time: every
// step is idempotent, a step that fails is logged and the next one runs, and
// the mark is a row in the store, so a daemon that stopped halfway through
// starts the job again at its next start. An admin reads where it got to
// through users_list.

// The steps of one removal, in the order they run. The name of the step that
// failed is what users_list carries, so it is the word an admin reads to know
// what is left on this host by hand.
const (
	StepProcesses     = "processes"
	StepRegistrations = "registrations"
	StepApprovals     = "approvals"
	StepBuilds        = "builds"
	StepSecrets       = "secrets"
	StepAccount       = "account"
	StepCgroups       = "cgroups"
)

// RemovalTimeout bounds one removal job. It is minutes and not seconds because
// it is every container of one member stopped and removed, and it is bounded
// at all because a job that hangs on a runtime that will not answer must still
// reach a final state an admin can read.
const RemovalTimeout = 30 * time.Minute

// RemovalConcurrency is how many of one member's containers are stopped and
// removed at once. A rootless podman serialises its own store per user, so
// this is not a number of parallel runtimes; it is what keeps twenty stops
// from being twenty times ten seconds when a container ignores SIGTERM.
const RemovalConcurrency = 8

// markRemoving records that this member is being removed, and reports whether
// this call is the one that marked them. It is the in memory half of the row
// the store holds: every path that would give a member new work asks this one,
// which is a map lookup, rather than the database.
func (s *Server) markRemoving(name string) bool {
	s.removingMu.Lock()
	defer s.removingMu.Unlock()
	if _, held := s.removing[name]; held {
		return false
	}
	s.removing[name] = struct{}{}
	return true
}

// doneRemoving forgets the mark, which the job does whatever it ended as: a
// member whose removal failed is not one this daemon keeps refusing work for
// forever, because the account may still be there and the state is readable.
func (s *Server) doneRemoving(name string) {
	s.removingMu.Lock()
	delete(s.removing, name)
	s.removingMu.Unlock()
}

// isRemoving reports whether a member is being removed right now, which is
// what stops new work from being taken for them: a Process registered or
// started while their containers are being torn down is a container the job
// has already walked past.
func (s *Server) isRemoving(name string) bool {
	s.removingMu.Lock()
	defer s.removingMu.Unlock()
	_, held := s.removing[name]
	return held
}

// notRemoving refuses a member new work while their removal job is running.
// A container started while the job is stopping the containers it read is a
// container the job has already walked past, and a registration written after
// it deleted them is a Process with a live token owned by an account that is
// about to be gone.
func (s *Server) notRemoving(r *http.Request, name, doing string) *problem.Problem {
	if !s.isRemoving(name) {
		return nil
	}
	return problem.ConflictFix(r.URL.Path,
		fmt.Sprintf("%s is being removed from this host, so may not %s", name, doing),
		"Call users_list to see how far the removal got.")
}

// startRemoval marks the member and runs the job. The context it runs under is
// the daemon's and never the request's: the answer is 202 and the caller is
// free to hang up, which is the whole point of the job.
func (s *Server) startRemoval(name, by string) {
	if !s.markRemoving(name) {
		// A job for this member is already running, and two of them stopping
		// the same containers is not a faster removal.
		return
	}
	go s.removeMember(name, by)
}

// removeMember is the job. Every step is idempotent and logged, a step that
// fails is logged and the job goes on, and the state it ends in is removed or
// failed with the first step that failed.
func (s *Server) removeMember(name, by string) {
	defer s.doneRemoving(name)
	ctx, cancel := context.WithTimeout(context.Background(), RemovalTimeout)
	defer cancel()

	state, step, archived := store.Removed, "", ""
	failed := func(at string, err error) {
		logger.Printf("users: removing %s: the %s step failed: %v", name, at, err)
		if state == store.Removed {
			state, step = store.Failed, at
		}
	}

	// The containers first, as their owner, because the account they run
	// under has to still be there. They are stopped and removed at once, a
	// few at a time, each one under the action lock of its own Process for
	// the reason proc_stop holds one: a tick that read this registration must
	// not be starting the container this is removing, see schedule.go.
	list, err := s.store.Processes(ctx, name)
	if err != nil {
		failed(StepProcesses, err)
	}
	if len(list) > 0 {
		s.removeContainers(ctx, name, list, failed)
	}

	// Their registrations, which is what revokes every Telemetry token they
	// hold: a token outliving the member it names is a producer nobody owns.
	// The names, the jobs, the probes and the fan out go with them, because
	// each one is a way this daemon would go on speaking for an account it no
	// longer has, see users.go.
	for _, p := range list {
		s.untrackProcess(p.ID)
	}
	ids, err := s.store.DeleteProcessesByOwner(ctx, name)
	if err != nil {
		failed(StepRegistrations, err)
	}
	for _, id := range ids {
		s.untrackProcess(id)
	}

	if _, err := s.store.DeleteApprovals(ctx, name); err != nil {
		failed(StepApprovals, err)
	}
	// Their build records go with their image store: a row naming an account
	// that is gone would send the next fetch to a member this host no longer
	// has, see builds.go.
	if _, err := s.store.DeleteBuildsByBuilder(ctx, name); err != nil {
		failed(StepBuilds, err)
	}
	// The values they set with secrets_set are the member's own and nobody
	// inherits them, and a directory left behind would hand them to the next
	// account created with that name, see PLAN.md section 2.3.
	if err := s.secrets.RemoveMember(name); err != nil {
		failed(StepSecrets, err)
	}

	// The home and the account, which is the step the whole job was made for:
	// it is minutes of work on a large home and it is what a client deadline
	// used to cut in half. An account this host no longer has is this step
	// done rather than this step failed, which is what makes the job safe to
	// run again after a restart.
	moved, err := s.users.Remove(ctx, name)
	switch {
	case err == nil:
		archived = moved
	case errors.Is(err, sysusers.ErrNotFound):
		logger.Printf("users: removing %s: the account was already gone", name)
	default:
		failed(StepAccount, err)
	}

	// Their containers are gone, so the cgroups those containers ran in go
	// too. One that is still busy is left for the next boot rather than
	// holding up an account that is already deleted.
	if err := s.cgroups.RemoveMember(ctx, name); err != nil {
		failed(StepCgroups, err)
	}

	if err := s.store.FinishRemoval(ctx, name, state, step, archived, s.now()); err != nil {
		logger.Printf("users: recording the removal of %s: %v", name, err)
	}
	if state == store.Removed {
		logger.Printf("%s removed the member %s, home archived at %s", by, name, archived)
		return
	}
	logger.Printf("%s removed the member %s, and the %s step did not finish; users_list carries it",
		by, name, step)
}

// removeContainers stops and removes every container one member holds, a few
// at a time. A container the runtime does not have is not a failure: the step
// describes the state it wants, which is what makes the job safe to run again.
func (s *Server) removeContainers(ctx context.Context, name string, list []store.Process,
	failed func(string, error)) {
	m, found, err := s.users.Lookup(ctx, name)
	if err != nil {
		failed(StepProcesses, err)
		return
	}
	if !found {
		// The account is already gone, so there is nobody to run podman as.
		// Whatever containers are left are the next boot's to find.
		logger.Printf("users: removing %s: the account is already gone, leaving the containers", name)
		return
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	tokens := make(chan struct{}, RemovalConcurrency)
	for _, p := range list {
		if p.Runner != "" || p.Container == "" {
			// A Process a run kit owns has no container on this host, and a
			// registration written before container names did not name one.
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			tokens <- struct{}{}
			defer func() { <-tokens }()

			release := s.actions.lock(p.ID)
			defer release()
			if err := s.runner.Stop(ctx, m, p.Container, StopTimeout); err != nil &&
				!errors.Is(err, sysusers.ErrNoContainer) {
				mu.Lock()
				failed(StepProcesses, err)
				mu.Unlock()
			}
			if err := s.runner.RemoveContainer(ctx, m, p.Container, true); err != nil &&
				!errors.Is(err, sysusers.ErrNoContainer) {
				mu.Lock()
				failed(StepProcesses, err)
				mu.Unlock()
			}
			if err := s.cgroups.RemoveProcess(ctx, name, p.ID); err != nil {
				logger.Printf("cgroups: the cgroup of %s could not be removed: %v", p.ID, err)
			}
		}()
	}
	wg.Wait()
}

// untrackProcess takes one Process off everything this daemon holds it in.
// A name left on the routing table would answer for a member this host no
// longer has, and it would answer by forwarding to a host port their container
// has freed, which is a port the next member's container may be given. A tick
// of a job they registered would start a container as nobody.
func (s *Server) untrackProcess(id string) {
	s.fanout.untrack(id)
	s.probes.untrack(id)
	s.proxy.untrack(id)
	s.jobs.untrack(id)
	s.endMCPSessions(id)
}

// resumeRemovals starts the job again for every member this host was removing
// when it stopped. It runs at the top of restore, before anything is started
// again, so a member whose containers are being torn down does not have them
// brought back by the same boot, see Restore.
func (s *Server) resumeRemovals(ctx context.Context) []string {
	list, err := s.store.RemovalsInState(ctx, store.Removing)
	if err != nil {
		logger.Printf("users: could not read the removals this host was in the middle of: %v", err)
		return nil
	}
	names := make([]string, 0, len(list))
	for _, r := range list {
		names = append(names, r.Name)
		logger.Printf("users: %s was being removed when this daemon stopped, taking it up again", r.Name)
		s.startRemoval(r.Name, "kitbashd")
	}
	sort.Strings(names)
	return names
}
