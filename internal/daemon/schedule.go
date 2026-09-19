package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/zyx1121/kitbash/internal/cgroups"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/mounts"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// The built in scheduler. A unit with expose: none may declare a five field
// cron expression, read in UTC; proc_run registers the job and starts nothing,
// and kitbashd starts the container at each tick as its owner, through the
// same four steps every start takes, see PLAN.md section 2.3.
//
// It is not a third built in. The rootless podman runner is what runs the
// container; this decides when, from what the manifest declared, the same way
// the reverse proxy decides what a Process is reachable at.

// ScheduleMetric is the name of the metric one tick writes, and the four
// attributes beside the usual ones say what became of it: the status the
// entrypoint exited with, whether the tick ran, was skipped or could not be
// started, how long the run took, and why a skipped tick was skipped.
const (
	ScheduleMetric       = "kitbash.schedule"
	AttrScheduleExitCode = "kitbash.schedule.exit_code"
	AttrScheduleResult   = "kitbash.schedule.result"
	AttrScheduleDuration = "kitbash.schedule.duration_ms"
	AttrScheduleReason   = "kitbash.schedule.reason"
)

// What one tick came to. A tick either ran the container to its exit, was
// skipped, or could not be started at all; there is no fourth answer, and
// "did it run this morning" is one tel_query for these words.
const (
	ScheduleRan           = "ran"
	ScheduleSkipped       = "skipped"
	ScheduleFailedToStart = "failed_to_start"
)

// Why a tick was skipped. A job whose previous run is still going is not run
// twice at once, and a host already running as many scheduled containers as it
// will is busy. Neither is a failure of the Package, which is why the two are
// not one word.
const (
	SkipRunning = "running"
	SkipBusy    = "busy"
)

// The value of the metric: a tick that ran the container, and one that did
// not. It is the same shape kitbash.health uses, so a query for "did it run"
// is a sum and not a string comparison.
const (
	ranValue    = 1
	notRanValue = 0
)

// MaxScheduledRuns is how many scheduled containers this host runs at once,
// across every member. A tick that arrives beyond it is skipped and recorded
// as busy rather than queued: a queue would run a job at a time nobody asked
// for, and the next tick of that job is the answer.
const MaxScheduledRuns = 8

// MaxScheduledPerMember is how many jobs one member may hold registered. Every
// one of them is a container this host starts on its own, so the count is what
// bounds what one member costs the ticker. It sits under
// MaxProcessesPerMember, which bounds their registrations in all.
const MaxScheduledPerMember = 32

// MaxRunTime is how long one run may take. A job that is still going after it
// is stopped, because a daily job that runs for a day is a job that is never
// not running, and the next tick would be skipped for ever.
const MaxRunTime = 6 * time.Hour

// StoppedExitCode is what a run stopped at MaxRunTime is recorded with, which
// is the status a shell reports for a process killed by SIGKILL. The result is
// still ran: the container ran, and it was this host that ended it.
const StoppedExitCode = 137

// ScheduleIdleWait is how long the loop waits when no tick is nearer than
// that, which is what a host with no jobs spends its time on. A registration
// wakes the loop, so this is a ceiling and not a delay.
const ScheduleIdleWait = time.Minute

// ScheduleWriteTimeout bounds the write of one run's record.
const ScheduleWriteTimeout = 5 * time.Second

// job is one registered schedule: what to run, when it is next due, and what
// the run before this one came to.
type job struct {
	id        string
	owner     string
	pkg       string
	container string
	cron      manifest.Cron

	// next is when this job is due and running whether a container of it is
	// up. A tick that arrives while the previous run is going is skipped and
	// recorded, which is the whole of the concurrency rule for one job, see
	// PLAN.md section 2.3.
	next    time.Time
	running bool

	// last is the run this job finished most recently, which proc_list
	// carries. It lives here and nowhere else: the store keeps the records a
	// run wrote, not the latest one of them, and a daemon that has just
	// started has no run of its own to report.
	last    lastRun
	hasLast bool
}

// lastRun is one finished run: when the container was started, what its
// entrypoint exited with, and how long it took.
type lastRun struct {
	startedAt time.Time
	exitCode  int
	duration  time.Duration
}

// skipped is one tick that was not run, and why.
type skipped struct {
	job    job
	reason string
	at     time.Time
}

// scheduler holds the registered jobs and how many runs are going. The jobs it
// holds are the registrations that declare a schedule, tracked as they arrive
// and dropped as they go, the same way the prober holds what it probes.
type scheduler struct {
	maxRuns int

	mu      sync.Mutex
	jobs    map[string]*job
	running int

	// changed wakes the loop when the set of jobs changed, so a job
	// registered now is due at its own next tick rather than at the end of
	// the current wait. It is buffered and never blocks.
	changed chan struct{}
}

func newScheduler(maxRuns int) *scheduler {
	if maxRuns <= 0 {
		maxRuns = MaxScheduledRuns
	}
	return &scheduler{
		maxRuns: maxRuns,
		jobs:    map[string]*job{},
		changed: make(chan struct{}, 1),
	}
}

// scheduled reports whether one registration is a job at all: it declares a
// cron expression, kitbashd runs it here, and it names a container to run.
//
// A Process a run kit owns is never scheduled. There is no container of it on
// this host and kitbashd supervises none of it, the same reason it probes
// none, see PLAN.md section 3.
func scheduled(p store.Process) bool {
	return p.Schedule.Declared() && p.Runner == "" && p.Container != ""
}

// track registers one job, or drops it when the registration no longer
// declares one. It is what a registration calls: one row changed, so one entry
// changes.
//
// A job this scheduler already holds keeps its next tick and its in flight
// marker when the expression has not changed, so registering the same job
// again does not move it. A new expression is read at once and the next tick
// is computed from now, which is also what a new registration gets: ticks are
// not made up, so there is nothing to carry over.
func (sc *scheduler) track(p store.Process, now time.Time) {
	if !scheduled(p) {
		sc.untrack(p.ID)
		return
	}
	cron, err := manifest.ParseCron(p.Schedule.Cron)
	if err != nil {
		// The expression was refused at registration, so this is a row from a
		// release that read it differently. Saying so once is the whole
		// answer: the job is not tracked and the owner reads proc_list.
		logger.Printf("schedule: the Process %s of %s carries a schedule this daemon cannot read: %v",
			p.ID, p.Owner, err)
		sc.untrack(p.ID)
		return
	}
	next, ok := cron.Next(now)
	if !ok {
		logger.Printf("schedule: the Process %s of %s declares %q, which names no date this calendar has",
			p.ID, p.Owner, p.Schedule.Cron)
		sc.untrack(p.ID)
		return
	}
	sc.mu.Lock()
	current, tracked := sc.jobs[p.ID]
	if tracked {
		current.owner = p.Owner
		current.pkg = p.Package
		current.container = p.Container
		if current.cron.Expr != cron.Expr {
			current.cron = cron
			current.next = next
		}
		sc.mu.Unlock()
		return
	}
	sc.jobs[p.ID] = &job{
		id:        p.ID,
		owner:     p.Owner,
		pkg:       p.Package,
		container: p.Container,
		cron:      cron,
		next:      next,
	}
	sc.mu.Unlock()
	sc.wake()
}

// untrack drops one job, which is what unregistering it does. A run already
// going for it is recorded to nobody, see finished.
func (sc *scheduler) untrack(id string) {
	sc.mu.Lock()
	_, tracked := sc.jobs[id]
	delete(sc.jobs, id)
	sc.mu.Unlock()
	if tracked {
		sc.wake()
	}
}

// due takes the jobs that are due now and answers three things: the ones to
// run, the ticks that were skipped with the reason for each, and how long the
// loop may wait before it looks again.
//
// Every due job's next tick is computed from now whether or not this one runs,
// so a skipped tick costs one tick and not the schedule.
func (sc *scheduler) due(now time.Time) ([]job, []skipped, time.Duration) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	var run []job
	var missed []skipped
	wait := ScheduleIdleWait
	for _, target := range sc.jobs {
		if target.next.After(now) {
			if left := target.next.Sub(now); left < wait {
				wait = left
			}
			continue
		}
		if next, ok := target.cron.Next(now); ok {
			target.next = next
			if left := next.Sub(now); left < wait {
				wait = left
			}
		}
		switch {
		case target.running:
			missed = append(missed, skipped{job: *target, reason: SkipRunning, at: now})
		case sc.running >= sc.maxRuns:
			missed = append(missed, skipped{job: *target, reason: SkipBusy, at: now})
		default:
			target.running = true
			sc.running++
			run = append(run, *target)
		}
	}
	return run, missed, wait
}

// finished records one run and reports whether it counts. It answers false for
// a job that was unregistered while its container was running, which is a job
// kitbashd no longer holds: the run belongs to nothing, so nothing is recorded
// about it. Either way the job stops being in flight and the host has one
// fewer run going.
func (sc *scheduler) finished(id string, run lastRun) bool {
	sc.mu.Lock()
	current, tracked := sc.jobs[id]
	if tracked {
		current.running = false
		current.last = run
		current.hasLast = true
	}
	if sc.running > 0 {
		sc.running--
	}
	sc.mu.Unlock()
	if tracked {
		// The next tick of this job may be nearer than whatever the loop is
		// waiting for, and a run that ended frees a slot for another job.
		sc.wake()
	}
	return tracked
}

// reading is one job's next tick and its most recent run, for processes_list.
func (sc *scheduler) reading(id string) (next time.Time, last lastRun, hasLast bool, tracked bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	target, held := sc.jobs[id]
	if !held {
		return time.Time{}, lastRun{}, false, false
	}
	return target.next, target.last, target.hasLast, true
}

// wake tells the loop to look again. A wake up that is already pending is
// enough, so this never blocks and never grows a queue.
func (sc *scheduler) wake() {
	select {
	case sc.changed <- struct{}{}:
	default:
	}
}

// count is how many jobs are registered, which health reports.
func (sc *scheduler) count() int {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return len(sc.jobs)
}

// ScheduleLoop starts every registered job at its ticks until ctx is done.
// kitbashd starts it after restore, beside the health probes: a job whose
// container is still coming back would otherwise be started while the runtime
// is busy with the boot.
//
// Ticks missed while the daemon was down are not made up. The next tick of
// every job is computed from now, here and at every registration, so a host
// that was off over a nightly run runs that job the next night and not at the
// moment it came back, see PLAN.md section 2.3.
func (s *Server) ScheduleLoop(ctx context.Context) {
	s.loadJobs(ctx)
	for {
		run, missed, wait := s.jobs.due(s.now())
		for _, target := range missed {
			s.recordSkip(ctx, target)
		}
		for _, target := range run {
			go s.runScheduled(ctx, target)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.stopped:
			timer.Stop()
			return
		case <-s.jobs.changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// loadJobs registers every job the store holds, without starting any of them.
// It runs after restore, which has already tracked what it walked; a job whose
// owner was not restorable is tracked here too, because a registration is what
// makes a job and restore starts no scheduled container.
//
// A daemon started with restore off loads none: an operator who asked for a
// host without its Processes did not ask for its jobs either.
func (s *Server) loadJobs(ctx context.Context) {
	if s.noRestore {
		logger.Printf("schedule: skipped, the daemon was started with restore off")
		return
	}
	list, err := s.store.Processes(ctx, "")
	if err != nil {
		logger.Printf("schedule: could not read the registered Processes: %v", err)
		return
	}
	now := s.now()
	for _, p := range list {
		if scheduled(p) {
			s.jobs.track(p, now)
		}
	}
	if held := s.jobs.count(); held > 0 {
		logger.Printf("schedule: %d registered job(s)", held)
	}
}

// runScheduled runs one tick to its end and records it. It is one goroutine
// per run: a job that takes an hour holds up no other job, and the loop is
// free the moment it hands the run over.
func (s *Server) runScheduled(ctx context.Context, target job) {
	started := s.now()
	code, result := s.runJob(ctx, target)
	run := lastRun{startedAt: started, exitCode: code, duration: s.now().Sub(started)}
	if !s.jobs.finished(target.id, run) {
		// The job was unregistered while this run was going, which is a job
		// nobody holds any more. What a record would say is that a Process
		// that is not registered ran, so it is dropped, the same way a health
		// reading of an unregistered Process is.
		return
	}
	s.recordRun(ctx, target, result, "", code, started, run.duration)
}

// runJob is one run: the container is made again from the registration, put
// through the same four steps a start takes, and waited on until its
// entrypoint exits.
//
// The container of the previous run is removed here and not when that run
// ended, because proc_logs reads the last run: the logs of a job live in the
// container until the next tick replaces it, see PLAN.md section 2.3.
func (s *Server) runJob(ctx context.Context, target job) (int, string) {
	p, found, err := s.store.Process(ctx, target.id)
	if err != nil || !found {
		logger.Printf("schedule: the Process %s of %s is no longer registered, so its tick ran nothing",
			target.id, target.owner)
		return 0, ScheduleFailedToStart
	}
	m, found, err := s.users.Lookup(ctx, p.Owner)
	if err != nil || !found {
		logger.Printf("schedule: %s owns the job %s and is not a member of this host: %v", p.Owner, p.ID, err)
		return 0, ScheduleFailedToStart
	}
	// The run holds this Process's action lock while the container is made and
	// started, so a proc_stop and a tick cannot take the runtime apart from
	// two sides. It is released before the wait: a job that runs for an hour
	// would otherwise be a Process its owner cannot stop for an hour.
	unlock := s.actions.lock(p.ID)
	if prob := s.startRun(ctx, p, m); prob != nil {
		unlock()
		logger.Printf("schedule: starting %s of %s: %s", p.Container, p.Owner, prob.Detail)
		return 0, ScheduleFailedToStart
	}
	unlock()
	logger.Printf("schedule: %s started %s for %s", p.ID, p.Container, p.Owner)
	return s.waitForRun(ctx, p, m), ScheduleRan
}

// startRun makes the container of one run and starts it. It is the start of
// run.go with the request taken out: the environment, the ceiling, the secrets
// and the mounts come from the registration, because a tick has no session
// behind it, and everything after that is the same four steps, see
// createVerifiedContainer.
func (s *Server) startRun(ctx context.Context, p store.Process, m sysusers.Member) *problem.Problem {
	// Whatever the previous tick left is removed first: the container name is
	// this job's, and a finished container of it is the logs proc_logs has
	// been reading since that run ended.
	if err := s.runner.RemoveContainer(ctx, m, p.Container, true); err != nil &&
		!errors.Is(err, sysusers.ErrNoContainer) {
		logger.Printf("schedule: removing the container of the previous run of %s: %v", p.ID, err)
	}
	opts, prob := scheduleOptions("", p)
	if prob != nil {
		return prob
	}
	// The mounts and the secrets are resolved again, exactly as a start
	// resolves them: what was true at registration is not what is true now,
	// and a run given a credential its owner has removed is worse than one
	// that did not start, see mounts.go and secrets.go.
	mounted, prob := s.revalidateMounts("", p, m)
	if prob != nil {
		return prob
	}
	opts.Mounts = mounts.Podman(mounted)
	held, prob := s.resolveSecrets("", p)
	if prob != nil {
		return prob
	}
	leaf := s.processCgroup(ctx, m, p, limitsOf(opts.Memory, opts.CPUs, opts.PidsLimit))
	if leaf != "" {
		opts.CgroupParent = cgroups.Parent(m.Name, p.ID)
	}
	p.Limits = store.Limits{Memory: p.Schedule.Memory, CPU: p.Schedule.CPU, Pids: opts.PidsLimit, Ceiling: leaf != ""}
	// Every run gets a token of its own, the same as every start: the store
	// keeps the hash alone, and the container of the run before this one is
	// gone.
	token, err := s.mintToken(ctx, p)
	if err != nil {
		return problem.Internal("", err.Error(), "")
	}
	envFile, err := s.writeEnvFile(p, m, p.Schedule.Env, token, held)
	if err != nil {
		return problem.Internal("", err.Error(), "")
	}
	defer func() {
		if err := os.Remove(envFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Printf("schedule: removing the environment file of %s: %v", p.ID, err)
		}
	}()
	opts.EnvFile = envFile
	if err := s.createVerifiedContainer(ctx, m, p, opts, leaf, mounted); err != nil {
		return problem.Internal("", fmt.Sprintf("the container runtime could not run %s: %v", p.Container, err), "")
	}
	return nil
}

// waitForRun waits for the entrypoint of one run to exit and answers the
// status it exited with. A run still going after MaxRunTime is stopped and
// recorded with StoppedExitCode: the schedule is what starts this container
// again, so a run that outlives its ceiling would skip every tick after it.
func (s *Server) waitForRun(ctx context.Context, p store.Process, m sysusers.Member) int {
	wait, cancel := context.WithTimeout(ctx, s.maxRunTime())
	defer cancel()
	code, err := s.runner.WaitContainer(wait, m, p.Container)
	if err == nil {
		return code
	}
	if ctx.Err() != nil {
		// The daemon is stopping, not the run. The container keeps going and
		// the next daemon reads it as the container of the last run, which is
		// what proc_logs answers with.
		logger.Printf("schedule: %s of %s was still running when this daemon stopped", p.Container, p.Owner)
		return 0
	}
	logger.Printf("schedule: %s of %s ran longer than %s, stopping it", p.Container, p.Owner, s.maxRunTime())
	if err := s.runner.Stop(context.WithoutCancel(ctx), m, p.Container, StopTimeout); err != nil &&
		!errors.Is(err, sysusers.ErrNoContainer) {
		logger.Printf("schedule: stopping %s of %s: %v", p.Container, p.Owner, err)
	}
	return StoppedExitCode
}

// maxRunTime is how long one run may take on this host. It is configurable for
// the tests alone, the same way the health interval floor is.
func (s *Server) maxRunTime() time.Duration {
	if s.scheduleMaxRun > 0 {
		return s.scheduleMaxRun
	}
	return MaxRunTime
}

// scheduleOptions is the command line of one run, from the registration
// alone. The container name, the image and the identity labels are the
// registration's, like every start; the environment and the ceiling are the
// schedule's, because that is where a unit's own declarations were put when
// the job was registered.
//
// Nothing is published and no restart policy is written: a job is expose: none
// and the schedule is what starts it again, so a policy here would restart a
// container that has done what it was started to do.
func scheduleOptions(instance string, p store.Process) (podman.RunOptions, *problem.Problem) {
	if p.Digest == "" {
		return podman.RunOptions{}, problem.BadRequest(instance,
			fmt.Sprintf("the job %s is registered without an image digest", p.ID),
			"Run the Package again, which registers the job with the digest it was built to.")
	}
	opts := podman.RunOptions{
		Name:        p.Container,
		Image:       p.Digest,
		Detach:      true,
		Interactive: true,
		Restart:     podman.RestartNo,
		PidsLimit:   DefaultPidsLimit,
	}
	if p.Schedule.Memory != "" {
		opts.Memory = podman.MemoryLimit(p.Schedule.Memory)
	}
	opts.CPUs = p.Schedule.CPU
	labels, prob := labelsOf(instance, p, nil)
	if prob != nil {
		return podman.RunOptions{}, prob
	}
	opts.Labels = labels
	return opts, nil
}

// recordSkip stores the one record a skipped tick writes. It carries the same
// attributes a run does, with the exit code of a run that never happened left
// out: what a reader asks of these records is whether the job ran, and a skip
// answers that with a reason.
func (s *Server) recordSkip(ctx context.Context, target skipped) {
	s.recordRun(ctx, target.job, ScheduleSkipped, target.reason, 0, target.at, 0)
}

// recordRun stores one tick as a metric and hands it to the fan out, which is
// the path every stored record takes: tel_query answers it to the job's owner
// and a subscriber sees it the way it sees anything else.
//
// The record carries the four attributes PLAN.md section 2.4 requires. The
// user is the member who owns the job, because the run is theirs; the producer
// is kitbashd, because a tick is not something the container said about
// itself; and the path is the Package folder, which is the file this run is
// about.
func (s *Server) recordRun(ctx context.Context, target job, result, reason string, code int,
	at time.Time, took time.Duration) {
	value := float64(notRanValue)
	if result == ScheduleRan {
		value = ranValue
	}
	other := map[string]any{
		AttrScheduleResult:   result,
		AttrScheduleDuration: took.Milliseconds(),
	}
	if result != ScheduleSkipped {
		other[AttrScheduleExitCode] = code
	}
	if reason != "" {
		other[AttrScheduleReason] = reason
	}
	export := store.Export{Metrics: []store.Metric{{
		TimeNS: at.UnixNano(),
		Name:   ScheduleMetric,
		Value:  value,
		Attributes: store.Attributes{
			User:     target.owner,
			Package:  target.pkg,
			Process:  target.id,
			Path:     target.pkg,
			Producer: InternalProducer,
			Other:    other,
		},
	}}}
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), ScheduleWriteTimeout)
	defer cancel()
	if err := s.store.Insert(write, export); err != nil {
		if ctx.Err() == nil {
			logger.Printf("schedule: could not record the tick of %s: %v", target.id, err)
		}
		return
	}
	s.fanout.dispatch(export, InternalProducer)
}
