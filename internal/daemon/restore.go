package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zyx1121/kitbash/internal/cgroups"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// RestoreTimeout is how long one container may take to come back, which is
// what spec/kitbashd-api.yaml gives it.
const RestoreTimeout = 60 * time.Second

// RestoreCounts is what one restore did, and what its log line reports.
type RestoreCounts struct {
	// Started is how many containers came back, the ones that were up
	// already included: what restore promises is that every registered
	// Process is running afterwards, not that it started each one.
	Started int
	// Running is how many of those were up already, which is what a daemon
	// that restarted without the host finds.
	Running int
	// Missing is how many the runtime no longer has, which are unregistered.
	Missing int
	// Failed is how many the runtime refused to start, which stay registered
	// so the next session or the next boot can try again.
	Failed int
	// Legacy is how many carried no container name, which are unregistered:
	// a registration written before M5 names nothing restore could start, and
	// no later boot will make it restorable.
	Legacy int
	// Healed is how many were registered before limits were recorded and were
	// given a ceiling of their own by this restore, which is a container
	// created again under it. They are counted with the started as well: a
	// heal that worked is a Process that came back.
	Healed int
}

// Restore starts every registered Process again as its owner, which is what
// makes "all Processes come back after a reboot" true, see PLAN.md section 2.3.
// It runs once per daemon start.
//
// The daemon does not wait for it before it serves: a host with fifty
// containers would keep every member's session waiting on podman. Restore runs
// beside the listeners and logs one line when it is done.
//
// Owners run in parallel and the containers of one owner run one at a time: a
// rootless podman serialises its own store per user anyway, and starting one
// member's containers must not be held up by another member's.
func (s *Server) Restore(ctx context.Context) RestoreCounts {
	if s.noRestore {
		logger.Printf("restore: skipped, the daemon was started with restore off")
		return RestoreCounts{}
	}
	list, err := s.store.Processes(ctx, "")
	if err != nil {
		logger.Printf("restore: could not read the registered Processes: %v", err)
		return RestoreCounts{}
	}

	var counts RestoreCounts
	byOwner := map[string][]store.Process{}
	for _, p := range list {
		// A registration without a container name was written before M5. It
		// names nothing the runtime could start and no later boot will change
		// that, so it goes: the row is deleted, which revokes its token and
		// drops its fan out subscription.
		if p.Container == "" {
			counts.Legacy++
			logger.Printf("restore: the Process %s of %s carries no container name, unregistering it",
				p.ID, p.Owner)
			if _, err := s.store.DeleteProcess(ctx, p.ID); err != nil {
				logger.Printf("restore: unregistering %s: %v", p.ID, err)
			}
			s.fanout.untrack(p.ID)
			s.endMCPSessions(p.ID)
			continue
		}
		byOwner[p.Owner] = append(byOwner[p.Owner], p)
	}
	owners := make([]string, 0, len(byOwner))
	for owner := range byOwner {
		owners = append(owners, owner)
	}
	sort.Strings(owners)

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, owner := range owners {
		processes := byOwner[owner]
		wg.Add(1)
		go func() {
			defer wg.Done()
			owned := s.restoreOwner(ctx, owner, processes)
			mu.Lock()
			counts.Started += owned.Started
			counts.Running += owned.Running
			counts.Missing += owned.Missing
			counts.Failed += owned.Failed
			counts.Healed += owned.Healed
			mu.Unlock()
		}()
	}
	wg.Wait()

	logger.Printf("restore: started %d (%d were already running), missing %d, failed %d, legacy %d, healed %d",
		counts.Started, counts.Running, counts.Missing, counts.Failed, counts.Legacy, counts.Healed)
	return counts
}

// restoreOwner starts one member's containers, one after the other.
func (s *Server) restoreOwner(ctx context.Context, owner string, processes []store.Process) RestoreCounts {
	var counts RestoreCounts
	// unplaced counts the Processes that come back without a ceiling, which is
	// said once for the owner rather than once per Process: a host with a
	// dozen of them would otherwise print a dozen lines at every boot.
	unplaced := 0
	m, found, err := s.users.Lookup(ctx, owner)
	if err != nil {
		logger.Printf("restore: looking up %s: %v", owner, err)
		counts.Failed += len(processes)
		return counts
	}
	if !found {
		// A registration whose owner is gone is a Process nobody can start.
		// Removing the member is what deletes those registrations; one that
		// survived that is left alone rather than run as somebody else.
		logger.Printf("restore: %s owns %d registered Processes and is no longer a member",
			owner, len(processes))
		counts.Failed += len(processes)
		return counts
	}

	s.memberCgroup(ctx, m)
	for _, p := range processes {
		// The Process's cgroup is created again, with its ceiling, before the
		// container starts: the cgroup filesystem does not survive a reboot,
		// and a container whose cgroup parent is gone does not start at all.
		// The limits come from the registration, which is the only thing that
		// remembers them once the filesystem is empty. A registration that
		// carries none still gets the cgroup, unlimited, which is what a
		// container that has to be created again is created under.
		limits := limitsOf(p.Limits.Memory, p.Limits.CPU, p.Limits.Pids)
		leaf := s.processCgroup(ctx, m, p, limits)
		start, cancel := context.WithTimeout(ctx, RestoreTimeout)
		err := s.runner.Start(start, m, p.Container, leaf)
		cancel()
		switch {
		case err == nil:
			counts.Started++
			s.clearProcessProblem(p.ID)
			if !p.Limits.Written() {
				unplaced++
			}
		case errors.Is(err, sysusers.ErrAlreadyRunning):
			// The daemon restarted and the host did not: the Process never
			// stopped. It is running, which is what restore is for, so it is
			// counted with the rest and named only in the one line at the end.
			counts.Started++
			counts.Running++
			s.clearProcessProblem(p.ID)
			if !p.Limits.Written() {
				unplaced++
			}
		case errors.Is(err, sysusers.ErrNoContainer):
			// The container is gone, so the registration names nothing and
			// its token belongs to no Process. Unregistering revokes it.
			counts.Missing++
			logger.Printf("restore: %s of %s no longer exists, unregistering the Process %s",
				p.Container, owner, p.ID)
			if _, err := s.store.DeleteProcess(ctx, p.ID); err != nil {
				logger.Printf("restore: unregistering %s: %v", p.ID, err)
			}
			s.fanout.untrack(p.ID)
			// There is no Process left to report a problem about.
			s.clearProcessProblem(p.ID)
			s.endMCPSessions(p.ID)
		default:
			logger.Printf("restore: starting %s of %s: %v", p.Container, owner, err)
			// A Process registered before kitbashd wrote a ceiling for each of
			// them may have a container whose cgroup parent is its member's
			// cgroup, which belongs to root: the member's runtime cannot
			// create the container's own cgroup under it and this start fails
			// at every boot, for ever. The ceiling above exists now, so the
			// container is created again under it rather than left to fail
			// again at the next boot.
			//
			// Only this one failure is healed. A start that ran out of its
			// budget, or one the daemon cancelled, says nothing about the
			// cgroup parent and everything about the runtime being busy: the
			// container may yet be coming up, and taking it apart because a
			// call was slow would break a Process that was about to run.
			if p.Limits.Written() || timedOut(err) {
				counts.Failed++
				s.startFailed(p)
				continue
			}
			healed, healErr := s.healCeiling(ctx, m, p, leaf)
			switch {
			case healed:
				counts.Started++
				counts.Healed++
				s.clearProcessProblem(p.ID)
				logger.Printf("restore: the Process %s of %s was registered without limits; it now runs under an unlimited ceiling, run it again to write limits",
					p.ID, owner)
			case healErr != nil:
				counts.Failed++
				logger.Printf("restore: giving %s of %s a ceiling of its own: %v", p.Container, owner, healErr)
				s.processFailed(p.ID, fmt.Sprintf(
					"this Process was registered before kitbashd gave each Process a cgroup of its own, and kitbashd could not create %s again under one: %v",
					p.Container, healErr),
					"Run the Process again, which creates its container under a ceiling of its own.")
			default:
				// The start failed for a reason that is not the cgroup parent,
				// so there is nothing to heal and the ordinary report stands.
				counts.Failed++
				s.startFailed(p)
			}
		}
	}
	if unplaced > 0 {
		logger.Printf("restore: %d Process(es) of %s came back without a ceiling, which is how they were registered; run each of them again to write one",
			unplaced, owner)
	}
	return counts
}

// timedOut reports a start that did not finish rather than one that failed: the
// runtime's own budget ran out, or the context this restore runs under was
// cancelled, which is a daemon that is stopping. Neither says anything about
// the container, so neither is a reason to take one apart.
func timedOut(err error) bool {
	return errors.Is(err, sysusers.ErrTimeout) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled)
}

// startFailed is what the owner reads through proc_list about a Process the
// runtime would not start. The runtime's own words stay in the daemon log,
// which is the operator's: they carry host paths and container ids.
func (s *Server) startFailed(p store.Process) {
	s.processFailed(p.ID,
		fmt.Sprintf("the container runtime did not start %s at the last boot of this host", p.Container),
		"Read proc_logs for this Process and run it again.")
}

// AsideSuffix is what the container being replaced is renamed to while its
// replacement is created. A heal that does not finish leaves it under this
// name, which is a container an operator can read and remove; the next heal
// refuses rather than removing it, because that container may be the only copy
// of the Process left.
const AsideSuffix = "-preceiling"

// healCeiling gives one legacy Process a ceiling of its own and answers whether
// its container was created again, which is a Process that is running once this
// returns.
//
// The cgroup parent of a container is written into it when it is created and
// podman start cannot change it, so a container created under its member's
// cgroup cannot be moved under the ceiling: it is created again from the
// registration and from its own configuration, keeping its id, its name and
// everything the unit declared.
//
// Nothing is removed before the replacement is up. The old container is
// renamed aside, the new one takes its name, and the old one is removed only
// once the new one has been created: a heal that fails puts the name back, so
// there is no moment in which the registration names a container that does not
// exist, and no boot in which the Process is unregistered because of it.
//
// Only the one container this can heal is touched: the cgroup parent has to be
// the member's own cgroup, which is the directory a rootless runtime is
// refused by. A container that names no parent, or one that is already under
// its ceiling, or one that names anything else, is left where it is.
//
// A host that could not make the ceiling heals nothing either: leaf is empty
// there, so there would be nothing to create the container under.
func (s *Server) healCeiling(ctx context.Context, m sysusers.Member, p store.Process, leaf string) (bool, error) {
	if leaf == "" {
		return false, nil
	}
	config, err := s.runner.ContainerConfig(ctx, m, p.Container)
	if err != nil {
		if errors.Is(err, sysusers.ErrNoContainer) {
			return false, nil
		}
		return false, err
	}
	if config.CgroupParent != cgroups.MemberParent(m.Name) {
		// Nothing about the cgroup parent stopped this start.
		return false, nil
	}
	image := p.Digest
	if image == "" {
		image = config.Image
	}
	if image == "" {
		return false, fmt.Errorf("daemon: %s names no image to create %s from", p.ID, p.Container)
	}
	parent := cgroups.Parent(m.Name, p.ID)
	opts := podman.RunOptions{
		Name:  p.Container,
		Image: image,
		// The same two flags every start uses: a Process is PID 1 of its
		// container with stdin held open.
		Detach:       true,
		Interactive:  true,
		Restart:      config.Restart,
		Publish:      config.Publish,
		CgroupParent: parent,
	}
	// No limits are put on the command line and none are written into the
	// ceiling: this Process was registered without any, so what it gains here
	// is a cgroup of its own and not a bound it never had. The ceiling is
	// where a limit would go once the Process is run again.
	labels, prob := labelsOf("", p, healedLabels(config.Labels))
	if prob != nil {
		return false, fmt.Errorf("daemon: the labels of %s: %s", p.Container, prob.Detail)
	}
	opts.Labels = labels

	// The image is checked before anything is moved. Creating the container
	// again needs it, and a member whose store no longer holds it is one this
	// heal can do nothing for.
	if _, err := s.runner.ImageInfo(ctx, m, image); err != nil {
		if errors.Is(err, sysusers.ErrNoImage) {
			return false, fmt.Errorf("%s is no longer in %s's image store: %w", image, m.Name, err)
		}
		return false, err
	}

	// The token is minted here and recorded only once the container is up: the
	// store keeps the hash alone, so the one the old container holds cannot be
	// read back and put in the new one, and a heal that fails must leave the
	// old container with the token it is holding. The fan out secret is not
	// minted again: it is kept in the clear, so the new container is given the
	// one the registration already carries.
	token, hash, err := store.NewToken()
	if err != nil {
		return false, err
	}
	envFile, err := s.writeEnvFile(p, m, healedEnv(config.Env), token)
	if err != nil {
		return false, err
	}
	defer func() {
		if err := os.Remove(envFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Printf("restore: removing the environment file of %s: %v", p.ID, err)
		}
	}()
	opts.EnvFile = envFile

	aside := p.Container + AsideSuffix
	if err := s.runner.RenameContainer(ctx, m, p.Container, aside); err != nil {
		return false, err
	}
	run, cancel := context.WithTimeout(ctx, RestoreTimeout)
	defer cancel()
	if _, err := s.runner.Run(run, m, opts, leaf); err != nil {
		// The name goes back to the container that holds this Process, so the
		// registration still names something the next boot can find and this
		// Process is reported failed rather than unregistered.
		if back := s.runner.RenameContainer(ctx, m, aside, p.Container); back != nil {
			logger.Printf("restore: %s of %s is left under %s: %v", p.Container, m.Name, aside, back)
		}
		return false, err
	}
	// The registration catches up only now: the ceiling is recorded, so the
	// next boot takes the ordinary path, and the token is the one the new
	// container is holding.
	if err := s.store.RegisterProcess(ctx, ceilingWritten(p), hash, 0); err != nil {
		// The container is up and the store missed the write. Saying so is the
		// whole answer: the Process runs, its exports are refused until it is
		// run again, and the next boot heals nothing because the container is
		// under its ceiling already.
		logger.Printf("restore: recording the ceiling and the token of %s: %v", p.ID, err)
	}
	// The container that was replaced goes last, once its name is free and the
	// new one exists.
	if err := s.runner.RemoveContainer(ctx, m, aside, true); err != nil &&
		!errors.Is(err, sysusers.ErrNoContainer) {
		logger.Printf("restore: removing %s, which %s replaced: %v", aside, p.Container, err)
	}
	return true, nil
}

// ceilingWritten is the registration as it stands once the Process has a
// ceiling of its own, which is what the next boot reads to take the ordinary
// path. The ceiling limits nothing: what is recorded is that it exists.
func ceilingWritten(p store.Process) store.Process {
	p.Limits.Ceiling = true
	return p
}

// healedLabels are the container's own labels without the ones kitbashd
// writes from the registration, which labelsOf puts back. Keeping them here
// would let a container's copy of a label outrank the registry's.
func healedLabels(labels map[string]string) map[string]string {
	kept := make(map[string]string, len(labels))
	for k, v := range labels {
		kept[k] = v
	}
	for _, owned := range []string{
		podman.LabelID, podman.LabelUser, podman.LabelPackage,
		podman.LabelName, podman.LabelExpose, podman.LabelDigest, podman.LabelEndpoint,
	} {
		delete(kept, owned)
	}
	return kept
}

// healedEnv is the environment the container is created again with: what it
// was created with the first time, minus the variables the runtime writes
// itself and minus anything that is not a name an environment file can carry.
// The variables kitbashd speaks for are dropped when the file is written.
func healedEnv(env map[string]string) map[string]string {
	kept := make(map[string]string, len(env))
	for k, v := range env {
		if k == "HOSTNAME" || k == "container" {
			continue
		}
		if !envKey.MatchString(k) || strings.ContainsAny(v, "\n\r") {
			continue
		}
		kept[k] = v
	}
	return kept
}

// restoreProblem is why one Process is not running, in the two sentences the
// surface carries: what happened and what its owner can do about it.
type restoreProblem struct {
	Detail string
	Fix    string
}

// processFailed records why one Process did not come back, which is what
// proc_list reports instead of leaving a container that never started looking
// like one that is on its way up.
func (s *Server) processFailed(id, detail, fix string) {
	s.restoreMu.Lock()
	defer s.restoreMu.Unlock()
	if s.restoreProblems == nil {
		s.restoreProblems = map[string]restoreProblem{}
	}
	s.restoreProblems[id] = restoreProblem{Detail: detail, Fix: fix}
}

// clearProcessProblem forgets it, which every start that worked does and so
// does unregistering: the Process is running, or it is gone, and either way
// whatever the last boot could not do for it is over.
func (s *Server) clearProcessProblem(id string) {
	s.restoreMu.Lock()
	defer s.restoreMu.Unlock()
	delete(s.restoreProblems, id)
}

// processProblem answers what was recorded for one Process, empty when nothing
// was.
func (s *Server) processProblem(id string) restoreProblem {
	s.restoreMu.Lock()
	defer s.restoreMu.Unlock()
	return s.restoreProblems[id]
}
