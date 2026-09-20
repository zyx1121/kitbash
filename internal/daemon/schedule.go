package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
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
	AttrScheduleError    = "kitbash.schedule.error"
)

// What one tick came to. The first three are the ordinary answers: the
// container ran to its exit, the tick was skipped, or the run could not be
// started at all. The last two are what a host says when it stopped being able
// to speak for the run: this daemon was shutting down while the container was
// running, and the runtime would not say what the container exited with.
// "Did it run this morning" is one tel_query for these words.
const (
	ScheduleRan           = "ran"
	ScheduleSkipped       = "skipped"
	ScheduleFailedToStart = "failed_to_start"
	ScheduleInterrupted   = "interrupted"
	ScheduleLost          = "lost"
)

// scheduleGone is not one of them. It is what a run answers when the
// registration was unregistered while the tick was reaching for it: there is
// no Process for a record to be about, so nothing is written, see runJob.
const scheduleGone = "gone"

// The classes of a wait that failed, recorded as kitbash.schedule.error beside
// the lost result. They are the runtime's failures and not the container's, so
// they are a class rather than an exit status.
const (
	lostNoContainer = "no-container"
	lostTimeout     = "timeout"
	lostRuntime     = "runtime"
)

// Why a tick was skipped. A job whose previous run is still going is not run
// twice at once, and a host already running as many scheduled containers as it
// will is busy. Neither is a failure of the Package, which is why the two are
// not one word.
const (
	SkipRunning = "running"
	SkipBusy    = "busy"
	// SkipRemoving is a tick of a member kitbashd is removing. The job has
	// already stopped and removed that container, or is about to, so a tick
	// that started it again would be a container the removal walked past and
	// a container running as an account that is going, see removal.go.
	SkipRemoving = "removing"
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

// MaxRunsPerMember is how many scheduled runs of one member go at once. It is
// the member's share of MaxScheduledRuns: without it a member with eight jobs
// that each take an hour holds the whole host, and every other member's tick
// is skipped as busy for reasons that have nothing to do with them.
const MaxRunsPerMember = 3

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

// noTickWait is how long a job whose expression names no date is left before
// the daemon looks at it again. It is a day because the thing it says is a log
// line, and a log line a day is a host an operator can read.
const noTickWait = 24 * time.Hour

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

// outcome is what one tick came to: the result, the status the entrypoint
// exited with where there was one, and the one word that says why on a tick
// that was skipped or a run this host lost track of.
type outcome struct {
	result string
	code   int
	reason string
	class  string
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
	maxRuns  int
	maxOwner int

	mu      sync.Mutex
	jobs    map[string]*job
	running int
	// perOwner is how many runs each member has going, which is what bounds
	// one member's share of the host, see MaxRunsPerMember.
	perOwner map[string]int

	// changed wakes the loop when the set of jobs changed, so a job
	// registered now is due at its own next tick rather than at the end of
	// the current wait. It is buffered and never blocks.
	changed chan struct{}
}

func newScheduler(maxRuns, perOwner int) *scheduler {
	if maxRuns <= 0 {
		maxRuns = MaxScheduledRuns
	}
	if perOwner <= 0 {
		perOwner = MaxRunsPerMember
	}
	return &scheduler{
		maxRuns:  maxRuns,
		maxOwner: perOwner,
		jobs:     map[string]*job{},
		perOwner: map[string]int{},
		changed:  make(chan struct{}, 1),
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
// The jobs are walked in a fixed order, by the tick they were due at and then
// by id, rather than in the order a map hands them over. When more are due
// than the host will run, which job loses has to be the same answer every
// time: otherwise the member whose tick is skipped is whoever the runtime
// hashed last.
//
// Every due job's next tick is computed from now whether or not this one runs,
// so a skipped tick costs one tick and not the schedule. A clock that moved
// backwards is no reason to run a tick that was already taken: the next tick
// is only ever moved forward.
func (sc *scheduler) due(now time.Time) ([]job, []skipped, time.Duration) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	var run []job
	var missed []skipped
	wait := ScheduleIdleWait
	for _, target := range sc.ordered() {
		if target.next.After(now) {
			if left := target.next.Sub(now); left < wait {
				wait = left
			}
			continue
		}
		sc.reschedule(target, now)
		if left := target.next.Sub(now); left > 0 && left < wait {
			wait = left
		}
		switch {
		case target.running:
			missed = append(missed, skipped{job: *target, reason: SkipRunning, at: now})
		case sc.running >= sc.maxRuns, sc.perOwner[target.owner] >= sc.maxOwner:
			missed = append(missed, skipped{job: *target, reason: SkipBusy, at: now})
		default:
			target.running = true
			sc.running++
			sc.perOwner[target.owner]++
			run = append(run, *target)
		}
	}
	return run, missed, wait
}

// ordered is the jobs in the order ticks are taken: the one due longest first,
// and the lower id first where two are due at the same moment. It is the whole
// of the fairness rule between two jobs of one member, and what makes the loser
// of a busy host the same job on every run.
func (sc *scheduler) ordered() []*job {
	out := make([]*job, 0, len(sc.jobs))
	for _, target := range sc.jobs {
		out = append(out, target)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].next.Equal(out[j].next) {
			return out[i].next.Before(out[j].next)
		}
		return out[i].id < out[j].id
	})
	return out
}

// reschedule moves one job to its next tick after now. A job whose expression
// names no date the calendar has, which is what an hour that no longer comes
// round looks like once a month field is read, is put a day out rather than
// left due: a job that is due for ever is a job every pass of the loop tries
// to run, and this way the daemon says so once a day instead.
func (sc *scheduler) reschedule(target *job, now time.Time) {
	next, ok := target.cron.Next(now)
	if !ok {
		logger.Printf("schedule: %q of the Process %s names no date this calendar has, so it is not due again today",
			target.cron.Expr, target.id)
		target.next = now.Add(noTickWait)
		return
	}
	target.next = next
}

// finished records one run and reports whether it counts. It answers false for
// a job that was unregistered while its container was running, which is a job
// kitbashd no longer holds: the run belongs to nothing, so nothing is recorded
// about it. Either way the job stops being in flight and the host has one
// fewer run going.
func (sc *scheduler) finished(id, owner string, run lastRun) bool {
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
	if sc.perOwner[owner] > 0 {
		sc.perOwner[owner]--
		if sc.perOwner[owner] == 0 {
			delete(sc.perOwner, owner)
		}
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
		// A member whose removal this boot is taking up again holds no jobs
		// as far as the ticker is concerned: their registrations are the
		// removal's to delete, and a tick before it gets there would start a
		// container it has already stopped, see removal.go.
		if !scheduled(p) || s.isRemoving(p.Owner) {
			continue
		}
		s.jobs.track(p, now)
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
	came := s.runJob(ctx, target)
	run := lastRun{startedAt: started, exitCode: came.code, duration: s.now().Sub(started)}
	tracked := s.jobs.finished(target.id, target.owner, run)
	if !tracked {
		// The job was unregistered while this run was going, which is a job
		// nobody holds any more. What a record would say is that a Process
		// that is not registered ran, so it is dropped, the same way a health
		// reading of an unregistered Process is.
		return
	}
	if came.result == scheduleGone {
		// The registration was gone before anything was started. There is no
		// Process for a record to be about and no container was made.
		return
	}
	s.recordRun(ctx, target, came, started, run.duration)
}

// runJob is one run: the container is made again from the registration, put
// through the same four steps a start takes, and waited on until its
// entrypoint exits.
//
// Everything up to the start happens under this Process's action lock, and the
// registration is read inside it. A tick reads a row and then writes it back
// when it mints the token of the run, so a proc_stop that landed in between
// would be undone by the tick that raced it: under the lock the unregister is
// either wholly before this read, which answers no row and runs nothing, or
// wholly after the start, which is a run of a Process that was registered when
// it began. The lock is released before the wait: a job that runs for an hour
// must not be a Process its owner cannot stop for an hour.
//
// The container of the previous run is removed here and not when that run
// ended, because proc_logs reads the last run: the logs of a job live in the
// container until the next tick replaces it, see PLAN.md section 2.3.
func (s *Server) runJob(ctx context.Context, target job) outcome {
	// A member being removed starts nothing. The removal stops and removes
	// their containers and then unregisters the jobs, and a tick that landed
	// between the two would hand the runtime a container the job has already
	// walked past, see removal.go.
	if s.isRemoving(target.owner) {
		logger.Printf("schedule: %s is being removed from this host, so the tick of %s started nothing",
			target.owner, target.id)
		return outcome{result: ScheduleSkipped, reason: SkipRemoving}
	}
	unlock := s.actions.lock(target.id)
	p, found, err := s.store.Process(ctx, target.id)
	if err != nil || !found {
		unlock()
		if err != nil {
			logger.Printf("schedule: reading the Process %s of %s: %v", target.id, target.owner, err)
			return outcome{result: ScheduleFailedToStart}
		}
		logger.Printf("schedule: the Process %s of %s was unregistered before its tick, so nothing ran",
			target.id, target.owner)
		return outcome{result: scheduleGone}
	}
	m, found, err := s.users.Lookup(ctx, p.Owner)
	if err != nil || !found {
		unlock()
		logger.Printf("schedule: %s owns the job %s and is not a member of this host: %v", p.Owner, p.ID, err)
		return outcome{result: ScheduleFailedToStart}
	}
	hash, prob, gone := s.startRun(ctx, p, m)
	unlock()
	switch {
	case gone:
		logger.Printf("schedule: the Process %s of %s was unregistered while its tick was starting it",
			p.ID, p.Owner)
		return outcome{result: scheduleGone}
	case prob != nil:
		logger.Printf("schedule: starting %s of %s: %s", p.Container, p.Owner, prob.Detail)
		return outcome{result: ScheduleFailedToStart}
	}
	logger.Printf("schedule: %s started %s for %s", p.ID, p.Container, p.Owner)
	came := s.waitForRun(ctx, p, m)
	// The container is kept for proc_logs and its token is not: a container
	// that has exited is not a producer, see PLAN.md section 2.4.
	s.revokeRunToken(ctx, p, hash)
	return came
}

// revokeRunToken ends the credential one run was given. The hash is named, so
// a Process registered again while this run was going keeps the token that
// registration minted.
func (s *Server) revokeRunToken(ctx context.Context, p store.Process, hash string) {
	if hash == "" {
		return
	}
	revoke, cancel := context.WithTimeout(context.WithoutCancel(ctx), ScheduleWriteTimeout)
	defer cancel()
	if _, err := s.store.RevokeProcessToken(revoke, p.ID, hash); err != nil {
		logger.Printf("schedule: revoking the token of the run of %s: %v", p.ID, err)
	}
}

// startRun makes the container of one run and starts it. It is the start of
// run.go with the request taken out: the environment, the ceiling, the secrets
// and the mounts come from the registration, because a tick has no session
// behind it, and everything after that is the same four steps, see
// createVerifiedContainer.
func (s *Server) startRun(ctx context.Context, p store.Process, m sysusers.Member) (string, *problem.Problem, bool) {
	// Whatever the previous tick left is removed first: the container name is
	// this job's, and a finished container of it is the logs proc_logs has
	// been reading since that run ended.
	if err := s.runner.RemoveContainer(ctx, m, p.Container, true); err != nil &&
		!errors.Is(err, sysusers.ErrNoContainer) {
		logger.Printf("schedule: removing the container of the previous run of %s: %v", p.ID, err)
	}
	opts, prob := scheduleOptions("", p)
	if prob != nil {
		return "", prob, false
	}
	// The mounts and the secrets are resolved again, exactly as a start
	// resolves them: what was true at registration is not what is true now,
	// and a run given a credential its owner has removed is worse than one
	// that did not start, see mounts.go and secrets.go.
	mounted, prob := s.revalidateMounts("", p, m)
	if prob != nil {
		return "", prob, false
	}
	opts.Mounts = mounts.Podman(mounted)
	held, prob := s.resolveSecrets("", p)
	if prob != nil {
		return "", prob, false
	}
	leaf := s.processCgroup(ctx, m, p, limitsOf(opts.Memory, opts.CPUs, opts.PidsLimit))
	if leaf != "" {
		opts.CgroupParent = cgroups.Parent(m.Name, p.ID)
	}
	p.Limits = store.Limits{Memory: p.Schedule.Memory, CPU: p.Schedule.CPU, Pids: opts.PidsLimit, Ceiling: leaf != ""}
	// Every run gets a token of its own, the same as every start: the store
	// keeps the hash alone, and the container of the run before this one is
	// gone. The write is an update and never an insert, so a registration that
	// was deleted while this tick was reaching for it stays deleted and this
	// run is the one that is abandoned, see store.UpdateProcessRun.
	token, hash, err := store.NewToken()
	if err != nil {
		return "", problem.Internal("", err.Error(), ""), false
	}
	held_, err := s.store.UpdateProcessRun(ctx, p.ID, p.Limits, hash)
	if err != nil {
		return "", problem.Internal("", err.Error(), ""), false
	}
	if !held_ {
		return "", nil, true
	}
	envFile, err := s.writeEnvFile(p, m, p.Schedule.Env, token, held)
	if err != nil {
		return hash, problem.Internal("", err.Error(), ""), false
	}
	defer func() {
		if err := os.Remove(envFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Printf("schedule: removing the environment file of %s: %v", p.ID, err)
		}
	}()
	opts.EnvFile = envFile
	if err := s.createVerifiedContainer(ctx, m, p, opts, leaf, mounted); err != nil {
		return hash, problem.Internal("",
			fmt.Sprintf("the container runtime could not run %s: %v", p.Container, err), ""), false
	}
	return hash, nil, false
}

// waitForRun waits for the entrypoint of one run to exit and answers the
// status it exited with and what became of the run.
//
// There are four ends to a wait and they are four different things to record.
// The entrypoint exited, which is the ordinary one. The run outlived
// MaxRunTime, so this host stopped it: the schedule is what starts this
// container again, and a run that outlives its ceiling would skip every tick
// after it, so it is recorded as a run with the status a killed container
// carries. The daemon is stopping, which says nothing about the container: it
// is still running and the next daemon reads it as the container of the last
// run. And the runtime would not answer at all, which is a run this host has
// lost track of rather than one that failed to start.
func (s *Server) waitForRun(ctx context.Context, p store.Process, m sysusers.Member) outcome {
	wait, cancel := context.WithTimeout(ctx, s.maxRunTime())
	defer cancel()
	code, err := s.runner.WaitContainer(wait, m, p.Container)
	switch {
	case err == nil:
		return outcome{result: ScheduleRan, code: code}
	case ctx.Err() != nil:
		logger.Printf("schedule: %s of %s was still running when this daemon stopped", p.Container, p.Owner)
		return outcome{result: ScheduleInterrupted}
	case errors.Is(wait.Err(), context.DeadlineExceeded):
		logger.Printf("schedule: %s of %s ran longer than %s, stopping it", p.Container, p.Owner, s.maxRunTime())
		if err := s.runner.Stop(context.WithoutCancel(ctx), m, p.Container, StopTimeout); err != nil &&
			!errors.Is(err, sysusers.ErrNoContainer) {
			logger.Printf("schedule: stopping %s of %s: %v", p.Container, p.Owner, err)
		}
		return outcome{result: ScheduleRan, code: StoppedExitCode}
	default:
		logger.Printf("schedule: waiting for %s of %s: %v", p.Container, p.Owner, err)
		return outcome{result: ScheduleLost, class: lostClass(err)}
	}
}

// lostClass says why a wait failed, for the record a lost run writes. It is
// the class and never the runtime's own words: those carry host paths and
// container ids, which are the operator's and not a member's.
func lostClass(err error) string {
	switch {
	case errors.Is(err, sysusers.ErrNoContainer):
		return lostNoContainer
	case errors.Is(err, sysusers.ErrTimeout):
		return lostTimeout
	default:
		return lostRuntime
	}
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
	s.recordRun(ctx, target.job, outcome{result: ScheduleSkipped, reason: target.reason}, target.at, 0)
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
func (s *Server) recordRun(ctx context.Context, target job, came outcome, at time.Time, took time.Duration) {
	value := float64(notRanValue)
	if came.result == ScheduleRan {
		value = ranValue
	}
	other := map[string]any{
		AttrScheduleResult:   came.result,
		AttrScheduleDuration: took.Milliseconds(),
	}
	// The exit status is on the records of a run that reached one. A skipped
	// tick never started a container, and an interrupted or lost run has no
	// status to report: a zero there would read as a run that succeeded.
	if came.result == ScheduleRan {
		other[AttrScheduleExitCode] = came.code
	}
	if came.reason != "" {
		other[AttrScheduleReason] = came.reason
	}
	if came.class != "" {
		other[AttrScheduleError] = came.class
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
