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
	"github.com/zyx1121/kitbash/internal/mounts"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
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
	// Kit is how many a run kit owns, which restore leaves alone: the Process
	// runs wherever the kit put it, and kitbashd supervises none of it, see
	// PLAN.md section 3. The registration stays, because it is what keeps the
	// Process on its owner's proc_list and its token alive.
	Kit int
	// Scheduled is how many are jobs, which restore registers with the ticker
	// and starts none of: a job runs at its ticks and nowhere else, and the
	// ticks missed while this host was down are not made up, see PLAN.md
	// section 2.3.
	Scheduled int
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
	// A member this host was removing when it stopped is taken up again
	// first, before anything of theirs is started: the job is what deletes
	// their registrations, and a boot that started their containers again
	// while it ran would hand the job containers it has already walked past,
	// see removal.go.
	removing := map[string]bool{}
	for _, name := range s.resumeRemovals(ctx) {
		removing[name] = true
	}
	if s.noRestore {
		logger.Printf("restore: skipped, the daemon was started with restore off")
		// The table is still built. It says what is reachable, which on a host
		// whose Processes were not started is every registered name answering
		// 503, and the container of one that was running already keeps being
		// forwarded to, see proxy.go.
		s.LoadRoutes(ctx)
		return RestoreCounts{}
	}
	list, err := s.store.Processes(ctx, "")
	if err != nil {
		logger.Printf("restore: could not read the registered Processes: %v", err)
		return RestoreCounts{}
	}

	var counts RestoreCounts
	now := s.now()
	byOwner := map[string][]store.Process{}
	for _, p := range list {
		// A member whose removal is being taken up again is left to the job:
		// their registrations are its to delete and their containers its to
		// stop.
		if removing[p.Owner] {
			continue
		}
		// A Process a run kit owns is not this daemon's to start. It carries
		// no container name either, so it is answered before the check below
		// that would unregister it as a registration naming nothing.
		if p.Runner != "" {
			counts.Kit++
			continue
		}
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
			s.probes.untrack(p.ID)
			s.proxy.untrack(p.ID)
			s.jobs.untrack(p.ID)
			s.endMCPSessions(p.ID)
			continue
		}
		// A job is registered again and started by nothing: its container is
		// made at the next tick, from this registration, and the tick it
		// would have had while the host was down is not made up, see
		// schedule.go.
		if scheduled(p) {
			counts.Scheduled++
			s.jobs.track(p, now)
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

	// The routing table is built from the registrations that are left, once,
	// after every container this boot brings back is up: the names travel with
	// the registration, so what a Process was reachable at before a reboot is
	// what it is reachable at after one, see PLAN.md section 2.3.
	s.LoadRoutes(ctx)

	logger.Printf("restore: started %d (%d were already running), missing %d, failed %d, legacy %d, healed %d, owned by a run kit %d, scheduled %d",
		counts.Started, counts.Running, counts.Missing, counts.Failed, counts.Legacy, counts.Healed, counts.Kit,
		counts.Scheduled)
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
		// A Process whose mount is no longer legal is not started. Its
		// container was created with that folder bound into it, so starting it
		// again would mount it again: the check has to happen before podman is
		// asked, and its answer is what proc_list reports about this Process,
		// see mounts.go.
		mounted, prob := s.revalidateMounts("", p, m)
		if prob != nil {
			counts.Failed++
			logger.Printf("restore: not starting %s of %s: %s", p.Container, owner, prob.Detail)
			failed := mountProblem(prob)
			s.processFailed(p.ID, failed.Detail, failed.Fix)
			continue
		}
		// The secrets are resolved again too, for the same reason: a name the
		// owner has removed since the Process was registered is one kitbashd
		// cannot give the container, and a Process started without a
		// credential it declared is worse than one that did not start. The
		// owner reads it through proc_list, the same shape as a mount that
		// stopped being legal, see secrets.go.
		//
		// A container that is started by name keeps the environment it was
		// created with, so what a rotation changes reaches it at the next
		// proc_run and not here; the resolution is what refuses to bring back
		// a Process whose credential is gone. The two paths that make the
		// container again, remakeMounted and healCeiling, write these values
		// into the environment file they create it with.
		held, prob := s.resolveSecrets("", p)
		if prob != nil {
			counts.Failed++
			logger.Printf("restore: not starting %s of %s: %s", p.Container, owner, prob.Detail)
			failed := unresolvedSecret(prob)
			s.processFailed(p.ID, failed.Detail, failed.Fix)
			continue
		}
		// The name the unit declared is proved again too, and for the same
		// reason: a record the member moved while this host was down is a name
		// kitbashd would serve for whoever holds it now. The owner reads it
		// through proc_list in the shape a missing secret has, see
		// hostnames.go.
		if prob := s.proveHostname(ctx, "", p); prob != nil {
			counts.Failed++
			logger.Printf("restore: not starting %s of %s: %s", p.Container, owner, prob.Detail)
			failed := hostnameProblem(prob)
			s.processFailed(p.ID, failed.Detail, failed.Fix)
			continue
		}
		// The Process's cgroup is created again, with its ceiling, before the
		// container starts: the cgroup filesystem does not survive a reboot,
		// and a container whose cgroup parent is gone does not start at all.
		// The limits come from the registration, which is the only thing that
		// remembers them once the filesystem is empty. A registration that
		// carries none still gets the cgroup, unlimited, which is what a
		// container that has to be created again is created under.
		limits := limitsOf(p.Limits.Memory, p.Limits.CPU, p.Limits.Pids)
		leaf := s.processCgroup(ctx, m, p, limits)
		// A Process with mounts that is still running is verified through the
		// process it already has: its namespace is the only place that says
		// what it is holding, and this daemon did not make that container.
		// One that is not running is not started again by name, because a
		// start makes the bind mounts anew and the entrypoint would run before
		// anything could be read: it is made again through create, prepare,
		// verify, start, the same four steps a start takes, see mounts.go.
		if len(mounted) > 0 {
			done, counted := s.restoreMounted(ctx, m, p, leaf, mounted, held)
			if done {
				counts.add(counted)
				if counted == restoredPlaced && !p.Limits.Written() {
					unplaced++
				}
				continue
			}
		}
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
			s.probes.untrack(p.ID)
			s.proxy.untrack(p.ID)
			s.jobs.untrack(p.ID)
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
			healed, healErr := s.healCeiling(ctx, m, p, leaf, held)
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
func (s *Server) healCeiling(ctx context.Context, m sysusers.Member, p store.Process, leaf string,
	held map[string]string) (bool, error) {
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
	// This creates the container again, so the mounts go on the new command
	// line and are resolved once more first: the registration is what says
	// which folders this Process sees, and the container's own configuration
	// is only read for what the registration does not carry. A mount that no
	// longer checks out fails the heal rather than recreating a container
	// bound to a folder the member may not see.
	mounted, prob := s.revalidateMounts("", p, m)
	if prob != nil {
		return false, fmt.Errorf("daemon: the mounts of %s: %s", p.ID, prob.Detail)
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
		Mounts:       mounts.Podman(mounted),
		CgroupParent: parent,
	}
	// No limits are put on the command line and none are written into the
	// ceiling: this Process was registered without any, so what it gains here
	// is a cgroup of its own and not a bound it never had. The ceiling is
	// where a limit would go once the Process is run again.
	labels, labelProb := labelsOf("", p, healedLabels(config.Labels))
	if labelProb != nil {
		return false, fmt.Errorf("daemon: the labels of %s: %s", p.Container, labelProb.Detail)
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
	envFile, err := s.writeEnvFile(p, m, healedEnv(config.Env), token, held)
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
	// The container this heal makes goes through the same four steps a start
	// does: create, prepare, verify, start. Nothing in it runs before its
	// mounts have been read in its own namespace.
	runErr := s.createVerifiedContainer(run, m, p, opts, leaf, mounted)
	if runErr != nil {
		// The name goes back to the container that holds this Process, so the
		// registration still names something the next boot can find and this
		// Process is reported failed rather than unregistered.
		if back := s.runner.RenameContainer(ctx, m, aside, p.Container); back != nil {
			logger.Printf("restore: %s of %s is left under %s: %v", p.Container, m.Name, aside, back)
		}
		return false, runErr
	}
	// The registration catches up only now: the ceiling is recorded, so the
	// next boot takes the ordinary path, and the token is the one the new
	// container is holding.
	if err := s.store.RegisterProcess(ctx, ceilingWritten(p), hash, store.Quota{}); err != nil {
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

// createVerifiedContainer is the four steps every container of a Process with
// mounts is made by: the runtime makes it, the runtime prepares it, which is
// where the bind mounts appear, kitbashd reads them in the container's own
// mount namespace, and only then is it started. A container that does not check
// out is removed here, having executed nothing, see mounts.go.
func (s *Server) createVerifiedContainer(ctx context.Context, m sysusers.Member, p store.Process,
	opts podman.RunOptions, leaf string, mounted []mounts.Resolved) error {
	if _, err := s.runner.CreateContainer(ctx, m, opts, leaf); err != nil {
		return err
	}
	if prob := s.prepareAndVerify(ctx, "", p, m, leaf, mounted); prob != nil {
		return errors.New(prob.Detail)
	}
	if err := s.runner.Start(ctx, m, p.Container, leaf); err != nil {
		s.tearDownAfterSwap(ctx, p, m)
		return err
	}
	return nil
}

// restoredPlaced and restoredFailed are what one Process of the verified
// restore path came to, which the caller adds to its counts.
const (
	restoredPlaced = iota
	restoredRunning
	restoredFailed
)

// add records one Process of the verified restore path in the counts.
func (c *RestoreCounts) add(outcome int) {
	switch outcome {
	case restoredPlaced:
		c.Started++
	case restoredRunning:
		c.Started++
		c.Running++
	case restoredFailed:
		c.Failed++
	}
}

// restoreMounted brings back one Process that declares mounts. It answers
// whether it handled the Process and, if it did, what it came to.
//
// Three things a container can be, and one answer each.
//
// Running: it is read where it stands. Its own namespace is the only thing that
// says what it is holding, and this daemon did not make that container.
//
// Initialized: the runtime has built its rootfs and its bind mounts and its
// init process exists, and the image's entrypoint has executed nothing. That is
// where a start pauses to read the mounts, so a container found in it is one a
// daemon was killed in the middle of, or one whose start failed. It is read the
// same way and then started, which runs the entrypoint in the namespace that
// was just read: verified on podman 5.7.0, where start after init keeps the
// same pid and the same namespace and resolves no path again.
//
// Anything else: it is not started by name, because a start of a container that
// has no namespace yet makes the bind mounts again and the entrypoint would be
// running before anything could read them. It is made again, from the
// registration and from its own configuration, through the same four steps a
// start takes.
//
// It hands the Process back unhandled only when the runtime has no container of
// it at all, which is the case restore answers by unregistering it.
func (s *Server) restoreMounted(ctx context.Context, m sysusers.Member, p store.Process,
	leaf string, mounted []mounts.Resolved, held map[string]string) (handled bool, outcome int) {
	failed := func(prob *problem.Problem) (bool, int) {
		logger.Printf("restore: not bringing %s of %s back: %s", p.Container, p.Owner, prob.Detail)
		report := mountProblem(prob)
		s.processFailed(p.ID, report.Detail, report.Fix)
		return true, restoredFailed
	}
	config, err := s.runner.ContainerConfig(ctx, m, p.Container)
	if err != nil {
		if errors.Is(err, sysusers.ErrNoContainer) {
			return false, 0
		}
		logger.Printf("restore: reading %s of %s: %v", p.Container, p.Owner, err)
		return failed(problem.Internal("", err.Error(), ""))
	}
	// A pid is not enough on its own: a container podman init prepared has one
	// and has run nothing. What it is doing is the state, and the pid is what
	// makes that state readable.
	live := config.PID > 0
	switch {
	case live && (config.State == podman.StateRunning || config.State == podman.StatePaused):
		if prob := s.verifyConfig("", p, config, mounted); prob != nil {
			s.tearDownAfterSwap(ctx, p, m)
			return failed(prob)
		}
		s.clearProcessProblem(p.ID)
		return true, restoredRunning
	case live && config.State == podman.StateInitialized:
		// A daemon that was killed between the check and the start leaves one
		// of these, and so does a start the runtime refused. The namespace is
		// there and nothing has run in it, which is exactly where a start
		// pauses, so it is read and then started rather than made again.
		if prob := s.verifyConfig("", p, config, mounted); prob != nil {
			s.tearDownAfterSwap(ctx, p, m)
			return failed(prob)
		}
		start, cancel := context.WithTimeout(ctx, RestoreTimeout)
		err := s.runner.Start(start, m, p.Container, leaf)
		cancel()
		if err != nil {
			logger.Printf("restore: starting the prepared %s of %s: %v", p.Container, p.Owner, err)
			s.tearDownAfterSwap(ctx, p, m)
			return failed(problem.Internal("", err.Error(), ""))
		}
		s.clearProcessProblem(p.ID)
		return true, restoredPlaced
	}
	if err := s.remakeMounted(ctx, m, p, config, leaf, mounted, held); err != nil {
		logger.Printf("restore: making %s of %s again: %v", p.Container, p.Owner, err)
		return failed(problem.NotPermitted("", err.Error(), ""))
	}
	s.clearProcessProblem(p.ID)
	return true, restoredPlaced
}

// remakeMounted makes the container of one Process again from the registration
// and from what the old one was made with, and starts it only once its mounts
// have been read in its own namespace. The old container is removed first: it
// is not running, its name is what the registration holds, and a start of it
// would put the bind mounts back without anything reading them.
func (s *Server) remakeMounted(ctx context.Context, m sysusers.Member, p store.Process,
	config sysusers.ContainerConfig, leaf string, mounted []mounts.Resolved, held map[string]string) error {
	image := p.Digest
	if image == "" {
		image = config.Image
	}
	if image == "" {
		return fmt.Errorf("daemon: %s names no image to make %s from", p.ID, p.Container)
	}
	opts := podman.RunOptions{
		Name:         p.Container,
		Image:        image,
		Detach:       true,
		Interactive:  true,
		Restart:      config.Restart,
		Publish:      config.Publish,
		Mounts:       mounts.Podman(mounted),
		CgroupParent: config.CgroupParent,
		Memory:       podman.MemoryLimit(p.Limits.Memory),
		CPUs:         p.Limits.CPU,
		PidsLimit:    p.Limits.Pids,
	}
	if leaf != "" {
		opts.CgroupParent = cgroups.Parent(m.Name, p.ID)
	}
	labels, prob := labelsOf("", p, healedLabels(config.Labels))
	if prob != nil {
		return fmt.Errorf("daemon: the labels of %s: %s", p.Container, prob.Detail)
	}
	opts.Labels = labels
	// The token is minted again, because the container that held the previous
	// one is about to be removed and the store keeps only its hash.
	token, hash, err := store.NewToken()
	if err != nil {
		return err
	}
	envFile, err := s.writeEnvFile(p, m, healedEnv(config.Env), token, held)
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(envFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Printf("restore: removing the environment file of %s: %v", p.ID, err)
		}
	}()
	opts.EnvFile = envFile

	if err := s.runner.RemoveContainer(ctx, m, p.Container, true); err != nil &&
		!errors.Is(err, sysusers.ErrNoContainer) {
		return err
	}
	make, cancel := context.WithTimeout(ctx, RestoreTimeout)
	defer cancel()
	if err := s.createVerifiedContainer(make, m, p, opts, leaf, mounted); err != nil {
		return err
	}
	if err := s.store.RegisterProcess(ctx, p, hash, store.Quota{}); err != nil {
		logger.Printf("restore: recording the token of %s: %v", p.ID, err)
	}
	return nil
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
