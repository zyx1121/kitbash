package proc

import (
	"context"
	"fmt"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// A unit with expose: none may declare a schedule, and then proc_run registers
// a job rather than starting a Process: kitbashd holds the cron expression and
// starts the container at each tick, as the owner, through the same four steps
// any start takes, see PLAN.md section 2.3.
//
// What this file does is therefore what proc.go does minus the start: the same
// image resolution, the same convergence by Package and name, the same
// registration. The container is made by the daemon at the first tick and not
// here.

// runJob registers one scheduled unit and starts nothing. It answers the job
// as the surface publishes it: state scheduled, and the tick kitbashd says it
// will run at.
//
// Convergence is the same rule as any Process: the same Package and name with
// the same digest is the same job and this changes nothing, and a new digest
// replaces it, which takes the container of its last run with it.
func (s *Service) runJob(ctx context.Context, m *manifest.Manifest, folder string, unit manifest.Unit,
	image *podman.Image, name, container string) (*Process, *problem.Problem) {
	if prob := checkOptions(folder, podman.RunOptions{
		Memory:  podman.MemoryLimit(unit.Limits.Memory),
		CPUs:    unit.Limits.CPU,
		Restart: podman.RestartNo,
	}); prob != nil {
		return nil, prob
	}
	// The mounts are checked here as the member, the same as for a Process:
	// the answer that decides is kitbashd's, and this one names the folder in
	// the session that named it, see Service.checkMounts.
	if prob := s.checkMounts(folder, unit.Mounts); prob != nil {
		return nil, prob
	}

	held, prob := s.byName(ctx, container)
	if prob != nil {
		return nil, prob
	}
	if held != nil {
		if owner := held.Labels[podman.LabelPackage]; owner != folder {
			return nil, problem.ConflictFix(folder, fmt.Sprintf(
				"a Process named %s already exists for %s", name, owner),
				"Pass a different name to proc_run, or stop that Process first.")
		}
	}
	known := s.registered(ctx)
	existing, isJob := s.heldJob(known, folder, name)
	if existing != nil && isJob && existing.Digest == image.ID {
		// Already converged: this job is registered for this digest and
		// kitbashd is holding its schedule. Repeating the call changes
		// nothing, which is the invariant in PLAN.md section 2.6.
		return s.jobProcess(*existing, unit), nil
	}

	var replaced *Process
	if held != nil {
		// Whatever holds this name goes: the container of the last run of a
		// job being replaced, or the Process this Package used to be before
		// its unit declared a schedule. Either way the name is this job's and
		// the next tick makes the container again.
		previous := s.describe(*held, unit)
		replaced = &previous
		if prob := s.remove(ctx, previous.ID, container); prob != nil {
			return nil, prob
		}
		s.unregister(ctx, previous.ID)
	}
	if existing != nil && (held == nil || existing.ID != held.Labels[podman.LabelID]) {
		// The registration of the job this one replaces, when the container it
		// named is already gone. A job that is registered twice would be run
		// twice, so the old one goes before the new one is written.
		s.unregister(ctx, existing.ID)
	}

	id := uuid.V7()
	if prob := s.register(ctx, telemetry.Registration{
		ID:        id,
		Package:   folder,
		Name:      name,
		Container: container,
		Digest:    image.ID,
		Expose:    unit.Expose,
		Permits:   m.Permits(),
		Mounts:    unit.Mounts,
		Secrets:   unit.Secrets,
		// The schedule carries what a start request would have carried: a tick
		// has no session behind it, so the unit's own environment and its
		// ceiling travel with the registration, see PLAN.md section 2.3.
		Schedule: &telemetry.Schedule{
			Cron:   unit.Schedule,
			Env:    ownEnv(unit.Env),
			Memory: unit.Limits.Memory,
			CPU:    unit.Limits.CPU,
		},
	}); prob != nil {
		return nil, prob
	}
	process := Process{
		ID:       id,
		Name:     name,
		Package:  folder,
		Digest:   image.ID,
		State:    StateScheduled,
		Expose:   unit.Expose,
		Schedule: unit.Schedule,
		Replaced: replaced,
	}
	// The tick is kitbashd's answer, not this session's: the expression is the
	// member's and the clock is the host's, so what the member is told is what
	// the daemon is holding.
	if entry, ok := s.registered(ctx)[id]; ok {
		process.NextRun = entry.NextRun
		process.Mounts = entry.Mounts
	}
	return &process, nil
}

// heldJob is the caller's registration of one Package and name, and whether it
// is a job. A Package converges by path and name, and a job has no container
// of its own between runs, so the registry is what says it exists.
func (s *Service) heldJob(known map[string]telemetry.Registered, folder, name string) (*telemetry.Registered, bool) {
	for id := range known {
		entry := known[id]
		if entry.Package != folder || entry.Name != name || !s.ownedByCaller(entry) {
			continue
		}
		return &entry, scheduledEntry(entry)
	}
	return nil, false
}

// scheduledEntry reports whether one registration is a job. A daemon of an
// earlier release answers no schedule at all, and a schedule with no
// expression in it is a Process rather than a job.
func scheduledEntry(entry telemetry.Registered) bool {
	return entry.Schedule != nil && entry.Schedule.Cron != ""
}

// jobProcess is one registered job as the surface publishes it. The state is
// scheduled whenever there is no container running: between its runs a job is
// not stopped, it is waiting, and a member reading proc_list wants the next
// tick rather than the exit of the last one.
func (s *Service) jobProcess(entry telemetry.Registered, unit manifest.Unit) *Process {
	process := Process{
		ID:       entry.ID,
		Name:     entry.Name,
		Package:  entry.Package,
		Digest:   entry.Digest,
		State:    StateScheduled,
		Expose:   entry.Expose,
		Mounts:   entry.Mounts,
		Problem:  entry.Problem,
		Fix:      entry.ProblemFix,
		NextRun:  entry.NextRun,
		LastRun:  lastRunOf(entry),
		Schedule: scheduleOf(entry),
	}
	if unit.Scheduled() {
		process.Schedule = unit.Schedule
	}
	if process.Expose == "" {
		process.Expose = manifest.ExposeNone
	}
	return &process
}

// scheduleOf is the expression one registration declares, or nothing.
func scheduleOf(entry telemetry.Registered) string {
	if !scheduledEntry(entry) {
		return ""
	}
	return entry.Schedule.Cron
}

// lastRunOf is the run one job finished most recently, as the surface
// publishes it. A job that has not run under this daemon has none, which is
// what a host that has just come back answers.
func lastRunOf(entry telemetry.Registered) *LastRun {
	if entry.LastRun == nil {
		return nil
	}
	return &LastRun{
		StartedAt:  entry.LastRun.StartedAt,
		ExitCode:   entry.LastRun.ExitCode,
		DurationMs: entry.LastRun.DurationMs,
	}
}

// scheduledProcesses are the caller's jobs that have no container here, which
// is every job between its registration and its first run. The registry is the
// only thing that knows they exist, the same as for a Process a run kit owns.
func (s *Service) scheduledProcesses(known map[string]telemetry.Registered, listed []Process) []Process {
	here := map[string]bool{}
	for _, p := range listed {
		here[p.ID] = true
	}
	var out []Process
	for id := range known {
		entry := known[id]
		if !scheduledEntry(entry) || here[entry.ID] || !s.ownedByCaller(entry) {
			continue
		}
		out = append(out, *s.jobProcess(entry, manifest.Unit{}))
	}
	return out
}

// asJob reports the Process a job's container belongs to as the job it is: a
// container that is not running means the job is waiting for its next tick,
// and one that is running is this tick's run. Either way the next tick and the
// last run are what the member reads.
func asJob(process Process, entry telemetry.Registered) Process {
	if !scheduledEntry(entry) {
		return process
	}
	if process.State != StateRunning {
		process.State = StateScheduled
	}
	process.Schedule = entry.Schedule.Cron
	process.NextRun = entry.NextRun
	process.LastRun = lastRunOf(entry)
	// A job is not a Process that failed to come back: it is started at its
	// ticks, so the reason the last boot could not start something is not
	// about it.
	process.Problem = ""
	process.Fix = ""
	return process
}

// scheduledJob is the caller's registration of one job by id, for proc_stop
// and proc_logs: neither can read a job off the runtime before its first run.
func (s *Service) scheduledJob(ctx context.Context, id string) (telemetry.Registered, bool) {
	if s.registry == nil {
		return telemetry.Registered{}, false
	}
	entry, held := s.registered(ctx)[id]
	if !held || !scheduledEntry(entry) || !s.ownedByCaller(entry) {
		return telemetry.Registered{}, false
	}
	return entry, true
}

// stopJob answers proc_stop for a job: the schedule is unregistered, so no
// tick runs it again, and the container of its last run is removed with it.
// The job stays a Package that can be run again, the same as any Process.
func (s *Service) stopJob(ctx context.Context, id string, entry telemetry.Registered) (*StopResult, *problem.Problem) {
	process := Process{
		ID:      id,
		Name:    entry.Name,
		Package: entry.Package,
		Digest:  entry.Digest,
		State:   StateStopped,
		Expose:  entry.Expose,
	}
	container, prob := s.containerOf(ctx, id)
	if prob != nil {
		return nil, prob
	}
	if container != nil {
		process.Container = container.Name
		if prob := s.remove(ctx, id, container.Name); prob != nil {
			return nil, prob
		}
	}
	s.unregister(ctx, id)
	return &StopResult{ID: id, State: StateStopped, Process: &process}, nil
}

// containerOf is byID without the refusal: a job that has not run yet has no
// container, and that is an answer rather than a problem.
func (s *Service) containerOf(ctx context.Context, id string) (*podman.Container, *problem.Problem) {
	containers, prob := s.containers(ctx, podman.Filter{
		podman.LabelUser: s.files.User(),
		podman.LabelID:   id,
	}, true)
	if prob != nil {
		return nil, prob
	}
	if len(containers) == 0 {
		return nil, nil
	}
	return &containers[0], nil
}
