package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The scheduler half of the job, PLAN.md 5.3 and section 2.3: an admin runs a
// Package whose unit declares a five field cron expression, proc_run registers
// the job and starts nothing, kitbashd starts the container at the next tick as
// the owner, and the run is one kitbash.schedule record a tel_query answers.
//
// The job is registered early in the story and read at the end. A minute has to
// pass for the tick, and the steps between the two cost more than that, so the
// wait below is a budget rather than a delay: on a host that is doing its work
// the record is already there when this looks.

// tickerName is the Package the admin writes, whose unit is a job.
const tickerName = "ticker"

// tickBudget is how long the last step waits for the first run of a job that
// runs every minute. A tick is at most a minute away and the run is a container
// that prints one line, so anything past this is the scheduler not working.
const tickBudget = 90 * time.Second

// registerTheJob is the first half: the Package is written, built and run, and
// what proc_run answers is a job that is waiting rather than a Process that is
// up. Nothing is started here, which is the thing to prove.
func registerTheJob(t *testing.T, s *state) {
	writeFixture(t, s.admin, tickerName, s.tickerPath)

	var built struct {
		Digest string `json:"digest"`
	}
	res := s.admin.callWithin(buildTimeout, "pkg_build", map[string]any{"path": s.tickerPath})
	res.mustSucceed(t, "pkg_build")
	if err := json.Unmarshal(res.Structured, &built); err != nil {
		t.Fatalf("decoding pkg_build: %v", err)
	}

	var out struct {
		ID       string `json:"id"`
		State    string `json:"state"`
		Expose   string `json:"expose"`
		Digest   string `json:"digest"`
		Schedule string `json:"schedule"`
		NextRun  string `json:"nextRun"`
	}
	s.admin.ok("proc_run", map[string]any{"package": s.tickerPath}, &out)
	if out.State != "scheduled" {
		t.Fatalf("proc_run answered %+v, want a scheduled Process", out)
	}
	if out.Expose != "none" || out.Schedule != "* * * * *" {
		t.Fatalf("proc_run answered expose %q and schedule %q, want none and every minute", out.Expose, out.Schedule)
	}
	if out.Digest != built.Digest {
		t.Fatalf("proc_run registered %s, want the digest pkg_build answered, %s", out.Digest, built.Digest)
	}
	next, err := time.Parse(time.RFC3339, out.NextRun)
	if err != nil {
		t.Fatalf("proc_run answered nextRun %q, want an RFC 3339 time: %v", out.NextRun, err)
	}
	if next.Before(time.Now().UTC().Add(-time.Minute)) {
		t.Fatalf("the next tick is %s, which is in the past", out.NextRun)
	}
	s.tickerID = out.ID
	t.Logf("registered the job %s, first tick %s", out.ID, out.NextRun)
}

// theJobRan is the second half: kitbashd started the container by itself, the
// run wrote one metric record carrying the four attributes and what became of
// the tick, and proc_logs answers with what that run printed.
func theJobRan(t *testing.T, s *state) {
	record, waited := waitForTheTick(t, s)
	t.Logf("the first tick was recorded after %s: %+v", waited.Round(time.Second), record.Other)
	if record.Attributes.User != adminName() || record.Attributes.Process != s.tickerID {
		t.Fatalf("the record is attributed to %+v, want the admin and the job", record.Attributes)
	}
	if record.Attributes.Package != s.tickerPath {
		t.Fatalf("the record carries package %q, want %s", record.Attributes.Package, s.tickerPath)
	}
	if result, _ := record.Other["kitbash.schedule.result"].(string); result != "ran" {
		t.Fatalf("the record says the tick %v, want it to have run", record.Other["kitbash.schedule.result"])
	}
	if code, ok := record.Other["kitbash.schedule.exit_code"].(float64); !ok || code != 0 {
		t.Fatalf("the record says the run exited %v, want 0", record.Other["kitbash.schedule.exit_code"])
	}
	if record.Value != 1 {
		t.Fatalf("the record's value is %v, want 1 for a tick that ran", record.Value)
	}

	// The listing reads as a job between its runs, and the run it finished is
	// on it.
	var listed struct {
		Processes []struct {
			ID      string `json:"id"`
			State   string `json:"state"`
			NextRun string `json:"nextRun"`
			LastRun *struct {
				ExitCode   int   `json:"exitCode"`
				DurationMs int64 `json:"durationMs"`
			} `json:"lastRun"`
		} `json:"processes"`
	}
	s.admin.ok("proc_list", map[string]any{"package": s.tickerPath}, &listed)
	var held bool
	for _, p := range listed.Processes {
		if p.ID != s.tickerID {
			continue
		}
		held = true
		if p.State != "scheduled" && p.State != "running" {
			t.Fatalf("proc_list reports the job as %s, want scheduled or running", p.State)
		}
		if p.NextRun == "" {
			t.Fatalf("proc_list reports the job without a next tick")
		}
		if p.LastRun == nil {
			t.Fatalf("proc_list reports no last run after a tick that ran")
		}
	}
	if !held {
		t.Fatalf("proc_list does not hold the job %s: %+v", s.tickerID, listed.Processes)
	}

	// And the logs of a job are the last run's, which is the line that run
	// printed before it exited.
	var logs struct {
		Lines []string `json:"lines"`
	}
	s.admin.ok("proc_logs", map[string]any{"id": s.tickerID}, &logs)
	if len(logs.Lines) == 0 {
		t.Fatalf("proc_logs answered nothing for a job that has run")
	}
	if !containsLine(logs.Lines, "ticked at") {
		t.Fatalf("proc_logs answered %v, want the line the run printed", logs.Lines)
	}
}

// stopTheJob is the end of it: proc_stop unregisters the schedule, so no tick
// runs it again, and the Process leaves the listing.
func stopTheJob(t *testing.T, s *state) {
	var out struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	s.admin.ok("proc_stop", map[string]any{"id": s.tickerID}, &out)
	if out.State != "stopped" {
		t.Fatalf("proc_stop answered %+v, want the job stopped", out)
	}
	var listed struct {
		Processes []struct {
			ID string `json:"id"`
		} `json:"processes"`
	}
	s.admin.ok("proc_list", map[string]any{"package": s.tickerPath}, &listed)
	for _, p := range listed.Processes {
		if p.ID == s.tickerID {
			t.Fatalf("the job is still listed after proc_stop, so a tick would run it again")
		}
	}
}

// metricRecord is one metric as tel_query answers it.
type metricRecord struct {
	Name       string  `json:"name"`
	Value      float64 `json:"value"`
	Attributes struct {
		User    string `json:"user"`
		Package string `json:"package"`
		Process string `json:"process"`
		Path    string `json:"path"`
	} `json:"attributes"`
	Other map[string]any `json:"other"`
}

// waitForTheTick reads Telemetry until the job's first run is recorded, and
// answers that record and how long it waited.
func waitForTheTick(t *testing.T, s *state) (metricRecord, time.Duration) {
	t.Helper()
	started := time.Now()
	for {
		var answer struct {
			Records []metricRecord `json:"records"`
		}
		s.admin.ok("tel_query", map[string]any{
			"signal":  "metrics",
			"user":    adminName(),
			"process": s.tickerID,
		}, &answer)
		for _, record := range answer.Records {
			if record.Name == "kitbash.schedule" {
				return record, time.Since(started)
			}
		}
		if time.Since(started) > tickBudget {
			t.Fatalf("waited %s for a kitbash.schedule record of the job %s", tickBudget, s.tickerID)
		}
		time.Sleep(2 * time.Second)
	}
}

// containsLine reports whether any line carries the text.
func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}
