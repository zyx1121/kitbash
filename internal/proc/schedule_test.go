package proc_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/proc"
	"github.com/zyx1121/kitbash/internal/telemetry/teltest"
)

// jobManifest is a unit that is a job: expose none, a cron expression, and the
// environment its runs are given.
const jobManifest = `name: weather
description: Fetch the forecast and post it to the group every morning.
deploy:
  units:
    - type: container
      build: .
      expose: none
      schedule: "0 8 * * *"
      env: { CITY: taipei }
      limits: { cpu: "1", memory: "512Mi" }
`

// TestRunningAScheduledUnitRegistersTheJobAndStartsNothing is the session half
// of the built in scheduler: proc_run registers the job with kitbashd, which
// is what holds the schedule, and starts no container at all.
func TestRunningAScheduledUnitRegistersTheJobAndStartsNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "weather", jobManifest)
	digest := f.build(folder, "weather")

	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if process.State != proc.StateScheduled {
		t.Errorf("state = %q, want %q", process.State, proc.StateScheduled)
	}
	if process.Digest != digest {
		t.Errorf("digest = %q, want the newest build %q", process.Digest, digest)
	}
	if process.Schedule != "0 8 * * *" {
		t.Errorf("schedule = %q, want the expression the unit declared", process.Schedule)
	}
	if process.NextRun == "" {
		t.Error("the job carries no nextRun, which is the daemon's answer to when it runs")
	}
	// Nothing was started and nothing was made: the container of a job is
	// made by the daemon at its first tick.
	containers, err := f.runner.Containers(ctx, nil, true)
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	if len(containers) != 0 {
		t.Errorf("proc_run made %d containers, want none", len(containers))
	}
	if got := len(f.daemon.Starts()); got != 0 {
		t.Errorf("proc_run asked kitbashd to start %d containers, want none", got)
	}
	// And the registration carries the job, which is everything a tick needs.
	reg, held := f.daemon.Registration(process.ID)
	if !held {
		t.Fatalf("the job was not registered")
	}
	if reg.Schedule == nil || reg.Schedule.Cron != "0 8 * * *" {
		t.Fatalf("the registration carries schedule %+v, want the unit's expression", reg.Schedule)
	}
	if reg.Schedule.Env["CITY"] != "taipei" {
		t.Errorf("the registration carries env %+v, want the unit's own", reg.Schedule.Env)
	}
	if reg.Schedule.Memory != "512Mi" || reg.Schedule.CPU != "1" {
		t.Errorf("the registration carries the ceiling %s/%s, want 512Mi/1", reg.Schedule.Memory, reg.Schedule.CPU)
	}
	if reg.Container != "kitbash-weather-weather" || reg.Digest != digest {
		t.Errorf("the registration names %s at %s, want the job's container and digest", reg.Container, reg.Digest)
	}
}

// Running a job again with the same digest is the same job: one registration,
// the same id, and still nothing started. It is the convergence every Process
// has, see PLAN.md section 2.6.
func TestRunningAJobAgainWithTheSameDigestChangesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "weather", jobManifest)
	f.build(folder, "weather")

	first, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("first Run: %s", prob.Detail)
	}
	second, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("second Run: %s", prob.Detail)
	}

	if second.ID != first.ID {
		t.Errorf("the second run answered %s, want the same job %s", second.ID, first.ID)
	}
	if got := len(f.daemon.Registrations()); got != 1 {
		t.Errorf("the registry holds %d jobs, want 1", got)
	}
	if got := len(f.daemon.Starts()); got != 0 {
		t.Errorf("running a job again started %d containers, want none", got)
	}
}

// A new digest replaces the job: the old registration goes, so it is not run
// twice, and the new one runs the new image.
func TestANewDigestReplacesTheJob(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "weather", jobManifest)
	f.build(folder, "weather")

	first, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("first Run: %s", prob.Detail)
	}
	newer := f.build(folder, "weather")
	second, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("second Run: %s", prob.Detail)
	}

	if second.ID == first.ID {
		t.Errorf("the new digest kept the job id %s, want a new one", second.ID)
	}
	if second.Digest != newer {
		t.Errorf("digest = %q, want the newer build %q", second.Digest, newer)
	}
	if _, held := f.daemon.Registration(first.ID); held {
		t.Errorf("the job it replaced is still registered, so it would run twice")
	}
	if got := len(f.daemon.Registrations()); got != 1 {
		t.Errorf("the registry holds %d jobs, want 1", got)
	}
}

// proc_stop of a job unregisters the schedule, which is the thing that would
// run it again, and removes the container its last run left behind.
func TestStoppingAJobUnregistersItAndRemovesTheLastRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "weather", jobManifest)
	f.build(folder, "weather")
	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	// The container a tick left behind, which is what proc_logs has been
	// reading since that run ended.
	f.runner.AddContainer(podman.Container{
		Name:     "kitbash-weather-weather",
		State:    podman.StateExited,
		ExitCode: 0,
		Labels: map[string]string{
			podman.LabelID:      process.ID,
			podman.LabelUser:    "tester",
			podman.LabelPackage: folder,
			podman.LabelName:    "weather",
		},
	})

	out, prob := f.processes.Stop(ctx, process.ID)
	if prob != nil {
		t.Fatalf("Stop: %s", prob.Detail)
	}
	if out.State != proc.StateStopped {
		t.Errorf("state = %q, want %q", out.State, proc.StateStopped)
	}
	if _, held := f.daemon.Registration(process.ID); held {
		t.Errorf("the job is still registered, so it would run again")
	}
	containers, err := f.runner.Containers(ctx, nil, true)
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	if len(containers) != 0 {
		t.Errorf("stopping the job left %d containers, want none", len(containers))
	}
}

// proc_list answers a job as scheduled with its next tick, before it has ever
// run, and the runtime has nothing to say about it.
func TestListingAJobBeforeItHasRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "weather", jobManifest)
	f.build(folder, "weather")
	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}

	list, prob := f.processes.List(ctx)
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	if len(list.Processes) != 1 {
		t.Fatalf("the listing holds %d Processes, want the one job", len(list.Processes))
	}
	listed := list.Processes[0]
	if listed.ID != process.ID || listed.State != proc.StateScheduled {
		t.Errorf("the listing holds %+v, want the job as scheduled", listed)
	}
	if listed.NextRun == "" {
		t.Error("the listed job carries no nextRun")
	}
	// And the one line answer carries the tick, which is what a line about a
	// job has to say.
	if line := list.Lines().Processes[0]; line.NextRun == "" || line.State != proc.StateScheduled {
		t.Errorf("the line is %+v, want a scheduled Process with its next tick", line)
	}
}

// Once a job has run, the container of that run is in the runtime and the
// listing still reads as the job: scheduled, with the run it finished.
func TestListingAJobAfterARun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "weather", jobManifest)
	f.build(folder, "weather")
	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	f.runner.AddContainer(podman.Container{
		Name:     "kitbash-weather-weather",
		State:    podman.StateExited,
		ExitCode: 0,
		Labels: map[string]string{
			podman.LabelID:      process.ID,
			podman.LabelUser:    "tester",
			podman.LabelPackage: folder,
			podman.LabelName:    "weather",
		},
	})
	reg, _ := f.daemon.Registration(process.ID)
	reg.LastRun = &teltest.LastRun{StartedAt: "2026-09-19T08:00:00Z", ExitCode: 0, DurationMs: 1200}
	f.daemon.AddProcess(reg)

	list, prob := f.processes.List(ctx)
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	if len(list.Processes) != 1 {
		t.Fatalf("the listing holds %d Processes, want one", len(list.Processes))
	}
	listed := list.Processes[0]
	if listed.State != proc.StateScheduled {
		t.Errorf("state = %q, want %q: between its runs a job is waiting, not stopped",
			listed.State, proc.StateScheduled)
	}
	if listed.LastRun == nil || listed.LastRun.DurationMs != 1200 {
		t.Errorf("lastRun = %+v, want the run the daemon reported", listed.LastRun)
	}
}

// proc_logs of a job that has not run says so, rather than answering that no
// Process of the caller has this id: the logs of a job are the last run's.
func TestLogsOfAJobThatHasNotRunSayItHasNotRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "weather", jobManifest)
	f.build(folder, "weather")
	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}

	_, prob = f.processes.Logs(ctx, process.ID, 0)
	if prob == nil {
		t.Fatal("the logs of a job that has not run were answered")
	}
	if prob.Status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", prob.Status, http.StatusNotFound)
	}
	if !strings.Contains(prob.Detail, "has not run yet") {
		t.Errorf("detail = %q, want it to say the job has not run yet", prob.Detail)
	}
}
