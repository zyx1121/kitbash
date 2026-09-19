package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"strings"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/cgroups"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// everyMinute is the expression a test uses when it wants a job that is always
// about to be due, and eightDaily one that fires once a day.
const (
	everyMinute = "* * * * *"
	eightDaily  = "0 8 * * *"
)

// scheduleRegistration is one valid registration of a job: expose none, no
// probe, no subscription, and the cron the caller names.
func scheduleRegistration(cron string) processRequest {
	req := registration("")
	req.Package = "/home/tester/weather"
	req.Name = "weather"
	req.Container = "kitbash-weather-weather"
	req.Digest = testDigest
	req.Schedule = &scheduleRequest{Cron: cron, Env: map[string]string{"CITY": "taipei"}}
	return req
}

// scheduled writes one job straight to the store, the way a registration
// would, and answers its id. It is what a daemon that has just started finds.
func (h *harness) scheduledProcess(owner, container, cron string) string {
	h.t.Helper()
	_, hash, err := store.NewToken()
	if err != nil {
		h.t.Fatalf("NewToken: %v", err)
	}
	id := uuid.V7()
	if err := h.store.RegisterProcess(context.Background(), store.Process{
		ID:           id,
		Owner:        owner,
		Package:      "/home/" + owner + "/weather",
		Name:         "weather",
		Container:    container,
		Digest:       testDigest,
		Expose:       ExposeNone,
		Schedule:     store.Schedule{Cron: cron, Env: map[string]string{"CITY": "taipei"}},
		FanoutSecret: "the-secret",
		RegisteredAt: time.Now().UTC(),
	}, hash, store.Quota{}); err != nil {
		h.t.Fatalf("RegisterProcess: %v", err)
	}
	return id
}

// scheduleRecords is every kitbash.schedule record the store holds.
func (h *harness) scheduleRecords() []store.Metric {
	h.t.Helper()
	var out []store.Metric
	for _, m := range h.metrics() {
		if m.Name == ScheduleMetric {
			out = append(out, m)
		}
	}
	return out
}

// tick runs whatever is due now, to its end, without the loop: the loop is a
// timer around exactly these two calls, and a test that drove the timer would
// be testing time.
func (h *harness) tick(at time.Time) ([]job, []skipped) {
	h.t.Helper()
	run, missed, _ := h.server.jobs.due(at)
	for _, target := range missed {
		h.server.recordSkip(context.Background(), target)
	}
	for _, target := range run {
		h.server.runScheduled(context.Background(), target)
	}
	return run, missed
}

// TestRegisteringAJobStartsNothing is the sentence of the built in scheduler:
// proc_run of a scheduled unit registers the job, builds nothing, starts no
// container and answers the tick kitbashd will run it at.
func TestRegisteringAJobStartsNothing(t *testing.T) {
	h, fake := supervised(t)

	req := scheduleRegistration(eightDaily)
	_, res, body := h.register(req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	if got := len(fake.Runs()); got != 0 {
		t.Errorf("the registration made %d containers, want none: a job is started by its ticks", got)
	}
	if got := len(fake.Calls()); got != 0 {
		t.Errorf("the registration started %d containers, want none", got)
	}
	if got := h.server.jobs.count(); got != 1 {
		t.Errorf("the daemon holds %d jobs, want 1", got)
	}
	var answer processResponse
	decodeJSON(t, body, &answer)
	next, err := time.Parse(time.RFC3339, answer.NextRun)
	if err != nil {
		t.Fatalf("nextRun = %q, want an RFC 3339 time: %v", answer.NextRun, err)
	}
	if next.Location() != time.UTC {
		t.Errorf("nextRun = %q, want it in UTC", answer.NextRun)
	}
	if next.Hour() != 8 || next.Minute() != 0 {
		t.Errorf("nextRun = %q, want the next 08:00 of %q", answer.NextRun, eightDaily)
	}
	// And the row carries the job, so the daemon that comes after this one
	// knows it is one.
	if held := h.process(req.ID); held.Schedule.Cron != eightDaily {
		t.Errorf("the registration holds schedule %q, want %q", held.Schedule.Cron, eightDaily)
	}
}

// A registration that declares a schedule is held to the rules of a job: it is
// expose none, it declares no probe, no subscription and no runner, and its
// expression is one this daemon can read. Each of them is refused to the
// member's face rather than stored as a job nothing runs.
func TestARegistrationRefusesAScheduleThatIsNotAJob(t *testing.T) {
	cases := []struct {
		name   string
		change func(*processRequest)
		status int
	}{
		{"an exposed unit", func(req *processRequest) { req.Expose = ExposeHTTP }, http.StatusBadRequest},
		{"a health probe", func(req *processRequest) {
			req.Health = &healthRequest{HTTP: "/healthz"}
		}, http.StatusBadRequest},
		{"a subscription", func(req *processRequest) {
			req.Subscriptions = []string{store.SubscriptionTelemetry}
		}, http.StatusBadRequest},
		{"a runner", func(req *processRequest) { req.Runner = "/org/pve-runner" }, http.StatusForbidden},
		{"four fields", func(req *processRequest) { req.Schedule.Cron = "0 8 * *" }, http.StatusBadRequest},
		{"a name for a day", func(req *processRequest) { req.Schedule.Cron = "0 8 * * mon" }, http.StatusBadRequest},
		{"a minute of 60", func(req *processRequest) { req.Schedule.Cron = "60 8 * * *" }, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// One daemon per case: a refusal that is not one would otherwise
			// leave a job behind for the case after it.
			h, _ := supervised(t)
			req := scheduleRegistration(eightDaily)
			c.change(&req)
			_, res, body := h.register(req)
			if res.StatusCode != c.status {
				t.Fatalf("register with %s: status = %d, want %d; body %s", c.name, res.StatusCode, c.status, body)
			}
			if _, found, _ := h.store.Process(context.Background(), req.ID); found {
				t.Errorf("the refused registration was stored anyway")
			}
			if h.server.jobs.count() != 0 {
				t.Errorf("the refused registration was tracked as a job anyway")
			}
		})
	}
}

// A member holds at most MaxScheduledPerMember jobs, the same shape of answer
// the 64 Process limit gives: the next one is a conflict naming the limit.
func TestAMemberHoldsAtMostThirtyTwoJobs(t *testing.T) {
	h, _ := supervised(t)
	for i := range MaxScheduledPerMember {
		h.scheduledProcess(h.user, "kitbash-weather-"+strings.Repeat("a", i+1), eightDaily)
	}

	req := scheduleRegistration(eightDaily)
	_, res, body := h.register(req)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("the 33rd job: status = %d, want %d; body %s", res.StatusCode, http.StatusConflict, body)
	}
	if !strings.Contains(string(body), "scheduled") {
		t.Errorf("the refusal does not say it is about scheduled Processes: %s", body)
	}
	// A Process that is not a job is not refused by it: the limit is the
	// ticker's, and the registrations are bounded by their own limit.
	plain := registration("")
	if _, res, body := h.register(plain); res.StatusCode != http.StatusOK {
		t.Fatalf("registering a Process that is not a job: status = %d, body %s", res.StatusCode, body)
	}
}

// TestATickStartsTheContainerAsTheOwner is the other half of the sentence: at
// the tick kitbashd makes the container through the same four steps a start
// takes, waits for it to exit, and writes one record saying what happened.
func TestATickStartsTheContainerAndRecordsTheRun(t *testing.T) {
	h, fake := supervised(t)
	req := scheduleRegistration(everyMinute)
	fake.Exits = map[string]int{req.Container: 0}
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}

	run, _ := h.tick(time.Now().Add(time.Hour))
	if len(run) != 1 {
		t.Fatalf("%d jobs were due, want 1", len(run))
	}
	runs := fake.Runs()
	if len(runs) != 1 {
		t.Fatalf("the tick made %d containers, want 1", len(runs))
	}
	if runs[0].Options.Name != req.Container || runs[0].Options.Image != testDigest {
		t.Errorf("the tick ran %s from %s, want %s from %s",
			runs[0].Options.Name, runs[0].Options.Image, req.Container, testDigest)
	}
	// The same four steps as any start: create, init, verify, start.
	if len(fake.Inits()) != 1 || len(fake.Calls()) != 1 {
		t.Errorf("the tick prepared %d and started %d containers, want one of each",
			len(fake.Inits()), len(fake.Calls()))
	}
	// And the same environment file: the unit's own variables, the Process's
	// identity and its token, written for the member.
	for _, want := range []string{"CITY=taipei", EnvProcess + "=" + req.ID, EnvTelemetryToken + "="} {
		if !strings.Contains(runs[0].Env, want) {
			t.Errorf("the environment file of the run does not carry %q:\n%s", want, runs[0].Env)
		}
	}
	if len(fake.Waits()) != 1 {
		t.Errorf("the tick waited for %d containers, want 1: a run is over when the entrypoint exits",
			len(fake.Waits()))
	}

	records := h.scheduleRecords()
	if len(records) != 1 {
		t.Fatalf("the run wrote %d records, want 1", len(records))
	}
	record := records[0]
	if record.Value != ranValue {
		t.Errorf("value = %v, want %v for a tick that ran", record.Value, float64(ranValue))
	}
	if record.Attributes.User != h.user || record.Attributes.Process != req.ID ||
		record.Attributes.Package != req.Package || record.Attributes.Path != req.Package {
		t.Errorf("attributes = %+v, want the four of the job", record.Attributes)
	}
	if record.Attributes.Producer != InternalProducer {
		t.Errorf("producer = %q, want %q: a tick is not something the container said",
			record.Attributes.Producer, InternalProducer)
	}
	if got := record.Attributes.Other[AttrScheduleResult]; got != ScheduleRan {
		t.Errorf("%s = %v, want %s", AttrScheduleResult, got, ScheduleRan)
	}
	if got, ok := record.Attributes.Other[AttrScheduleExitCode]; !ok || !isZero(got) {
		t.Errorf("%s = %v, want 0", AttrScheduleExitCode, got)
	}
	if _, ok := record.Attributes.Other[AttrScheduleDuration]; !ok {
		t.Errorf("the record carries no %s", AttrScheduleDuration)
	}
	// The job is waiting again, with the run it just finished on it.
	next, last, hasLast, tracked := h.server.jobs.reading(req.ID)
	if !tracked || next.IsZero() || !hasLast || last.exitCode != 0 {
		t.Errorf("after the run the job reads next %v, last %+v (has %v, tracked %v)", next, last, hasLast, tracked)
	}
}

// A tick that arrives while the previous run of that job is still going is
// skipped and recorded, and nothing is started twice.
func TestATickDuringARunIsSkipped(t *testing.T) {
	h, fake := supervised(t)
	req := scheduleRegistration(everyMinute)
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	// The first tick takes the job, which is what a run that is still going
	// looks like to the second.
	at := time.Now().Add(time.Hour)
	run, _, _ := h.server.jobs.due(at)
	if len(run) != 1 {
		t.Fatalf("%d jobs were due, want 1", len(run))
	}

	_, missed := h.tick(at.Add(time.Minute))
	if len(missed) != 1 || missed[0].reason != SkipRunning {
		t.Fatalf("the second tick missed %+v, want one skipped as %s", missed, SkipRunning)
	}
	if got := len(fake.Runs()); got != 0 {
		t.Errorf("the skipped tick made %d containers, want none", got)
	}
	records := h.scheduleRecords()
	if len(records) != 1 {
		t.Fatalf("the skipped tick wrote %d records, want 1", len(records))
	}
	if got := records[0].Attributes.Other[AttrScheduleResult]; got != ScheduleSkipped {
		t.Errorf("%s = %v, want %s", AttrScheduleResult, got, ScheduleSkipped)
	}
	if got := records[0].Attributes.Other[AttrScheduleReason]; got != SkipRunning {
		t.Errorf("%s = %v, want %s", AttrScheduleReason, got, SkipRunning)
	}
	if records[0].Value != notRanValue {
		t.Errorf("value = %v, want %v for a tick that did not run", records[0].Value, float64(notRanValue))
	}
}

// Across jobs a host runs at most so many scheduled containers at once, and a
// tick beyond that is skipped as busy rather than queued.
func TestATickBeyondTheConcurrencyCapIsSkippedAsBusy(t *testing.T) {
	fake := sysusers.NewFake()
	h := serveWith(t, Options{
		Admin:           func(*user.User) (bool, error) { return false, nil },
		Users:           fake,
		Runner:          fake,
		ScheduleMaxRuns: 1,
	})
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid()})
	first := scheduleRegistration(everyMinute)
	second := scheduleRegistration(everyMinute)
	second.Name = "tides"
	second.Container = "kitbash-weather-tides"
	for _, req := range []processRequest{first, second} {
		if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
			t.Fatalf("register status = %d, body %s", res.StatusCode, body)
		}
	}

	at := time.Now().Add(time.Hour)
	run, missed, _ := h.server.jobs.due(at)
	if len(run) != 1 || len(missed) != 1 {
		t.Fatalf("%d jobs ran and %d were skipped, want one of each under a cap of 1", len(run), len(missed))
	}
	if missed[0].reason != SkipBusy {
		t.Errorf("the skipped tick reads %q, want %q", missed[0].reason, SkipBusy)
	}
	h.server.recordSkip(context.Background(), missed[0])
	records := h.scheduleRecords()
	if len(records) != 1 || records[0].Attributes.Other[AttrScheduleReason] != SkipBusy {
		t.Fatalf("the records are %+v, want one skipped as busy", records)
	}
}

// A run that outlives the ceiling is stopped and recorded with the status a
// killed container exits with. The result is still ran: it ran, and this host
// ended it.
func TestARunLongerThanTheCeilingIsStopped(t *testing.T) {
	fake := sysusers.NewFake()
	h := serveWith(t, Options{
		Admin:          func(*user.User) (bool, error) { return false, nil },
		Users:          fake,
		Runner:         fake,
		ScheduleMaxRun: 50 * time.Millisecond,
	})
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid()})
	req := scheduleRegistration(everyMinute)
	// A container whose wait never answers, which is a run that does not end.
	fake.Waiting = map[string]chan int{req.Container: make(chan int)}
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}

	h.tick(time.Now().Add(time.Hour))

	stops := fake.Stops()
	if len(stops) != 1 || stops[0].Container != req.Container {
		t.Fatalf("the host stopped %+v, want %s", stops, req.Container)
	}
	records := h.scheduleRecords()
	if len(records) != 1 {
		t.Fatalf("the stopped run wrote %d records, want 1", len(records))
	}
	if got := records[0].Attributes.Other[AttrScheduleResult]; got != ScheduleRan {
		t.Errorf("%s = %v, want %s: the container ran and this host ended it", AttrScheduleResult, got, ScheduleRan)
	}
	if got := records[0].Attributes.Other[AttrScheduleExitCode]; !isCode(got, StoppedExitCode) {
		t.Errorf("%s = %v, want %d", AttrScheduleExitCode, got, StoppedExitCode)
	}
}

// The container of a run is kept after it exits, which is what proc_logs
// reads, and removed at the next tick, which makes it again.
func TestTheContainerOfARunIsRemovedAtTheNextTick(t *testing.T) {
	h, fake := supervised(t)
	req := scheduleRegistration(everyMinute)
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}

	h.tick(time.Now().Add(time.Hour))
	if got := len(fake.Removals()); got != 1 {
		// The first tick removes whatever was there, which is nothing; the
		// fake records the call either way.
		t.Logf("the first tick removed %d containers", got)
	}
	h.tick(time.Now().Add(2 * time.Hour))

	if got := len(fake.Runs()); got != 2 {
		t.Fatalf("two ticks made %d containers, want 2", got)
	}
	removals := fake.Removals()
	if len(removals) < 1 || removals[len(removals)-1].Container != req.Container {
		t.Errorf("the second tick removed %+v, want the container of the first run", removals)
	}
}

// Ticks missed while the daemon was down are not made up: the next tick of
// every job is computed from now, so a host that was off over the run of a
// daily job runs it the next day and not at the moment it came back.
func TestMissedTicksAreNotMadeUp(t *testing.T) {
	h, fake := supervised(t)
	id := h.scheduledProcess(h.user, "kitbash-weather-weather", eightDaily)

	// A daemon that starts at nine, an hour after the tick it was down for.
	nine := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	h.server.now = func() time.Time { return nine }
	h.server.loadJobs(context.Background())

	next, _, hasLast, tracked := h.server.jobs.reading(id)
	if !tracked {
		t.Fatalf("the job was not registered again at start")
	}
	if hasLast {
		t.Errorf("a daemon that has just started reports a last run it did not see")
	}
	if want := nine.Add(23 * time.Hour); !next.Equal(want) {
		t.Errorf("next tick = %s, want %s: the tick at 08:00 is not made up", next, want)
	}
	run, missed := h.tick(nine)
	if len(run) != 0 || len(missed) != 0 {
		t.Errorf("the daemon ran %d and skipped %d ticks at start, want none of either", len(run), len(missed))
	}
	if got := len(fake.Runs()); got != 0 {
		t.Errorf("starting the ticker made %d containers, want none", got)
	}
}

// Restore registers every job again and starts none of them: a job has no
// container to bring back, and its next tick is the one that makes one.
func TestRestoreRegistersJobsAndStartsNothing(t *testing.T) {
	h, fake := supervised(t)
	id := h.scheduledProcess(h.user, "kitbash-weather-weather", eightDaily)

	counts := h.server.Restore(context.Background())

	if counts.Scheduled != 1 {
		t.Errorf("restore counted %d jobs, want 1", counts.Scheduled)
	}
	if counts.Started != 0 || counts.Missing != 0 || counts.Failed != 0 {
		t.Errorf("restore started %d, missed %d and failed %d, want none of any: a job is started by its ticks",
			counts.Started, counts.Missing, counts.Failed)
	}
	if got := len(fake.Calls()); got != 0 {
		t.Errorf("restore started %d containers, want none", got)
	}
	if _, _, _, tracked := h.server.jobs.reading(id); !tracked {
		t.Errorf("restore did not register the job with the ticker")
	}
	// And the registration is still there: a job with no container is not a
	// registration naming nothing.
	if _, found, _ := h.store.Process(context.Background(), id); !found {
		t.Errorf("restore unregistered the job")
	}
}

// A session cannot start a job: kitbashd starts one at its ticks and nowhere
// else, so the start of one is refused rather than run at a time nobody
// declared.
func TestStartingAJobFromASessionIsRefused(t *testing.T) {
	h, fake := supervised(t)
	id := h.scheduledProcess(h.user, "kitbash-weather-weather", eightDaily)

	res, body := h.start(id, startRequest{Image: testDigest})

	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("start of a job: status = %d, want %d; body %s", res.StatusCode, http.StatusForbidden, body)
	}
	if got := len(fake.Runs()); got != 0 {
		t.Errorf("the refused start made %d containers, want none", got)
	}
}

// Unregistering a job drops it from the ticker, which is what proc_stop does:
// there is no tick after the registration is gone.
func TestUnregisteringAJobDropsIt(t *testing.T) {
	h, _ := supervised(t)
	req := scheduleRegistration(everyMinute)
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}

	res, body := h.do(http.MethodDelete, processesPath+"/"+req.ID, "", nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("unregister status = %d, body %s", res.StatusCode, body)
	}
	if got := h.server.jobs.count(); got != 0 {
		t.Errorf("the daemon holds %d jobs after unregistering, want none", got)
	}
	run, _, _ := h.server.jobs.due(time.Now().Add(time.Hour))
	if len(run) != 0 {
		t.Errorf("%d ticks were due for an unregistered job", len(run))
	}
}

// A run whose job was unregistered while it was going is recorded about
// nobody, the same rule a health reading follows.
func TestARunOfAnUnregisteredJobIsNotRecorded(t *testing.T) {
	h, _ := supervised(t)
	target := job{id: uuid.V7(), owner: h.user, pkg: "/home/tester/weather", container: "kitbash-weather-weather"}

	h.server.runScheduled(context.Background(), target)

	if got := len(h.scheduleRecords()); got != 0 {
		t.Errorf("a run of a job nobody holds wrote %d records, want none", got)
	}
}

// A job whose registration the ticker is holding answers processes_list with
// the next tick and, once it has run, the run it finished.
func TestTheListingCarriesTheNextTickAndTheLastRun(t *testing.T) {
	h, fake := supervised(t)
	req := scheduleRegistration(everyMinute)
	fake.Exits = map[string]int{req.Container: 3}
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	h.tick(time.Now().Add(time.Hour))

	res, body := h.do(http.MethodGet, processesPath, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, body %s", res.StatusCode, body)
	}
	var list processList
	decodeJSON(t, body, &list)
	if len(list.Processes) != 1 {
		t.Fatalf("the listing holds %d Processes, want 1", len(list.Processes))
	}
	entry := list.Processes[0]
	if entry.NextRun == "" {
		t.Errorf("the listed job carries no nextRun")
	}
	if entry.LastRun == nil {
		t.Fatalf("the listed job carries no lastRun after a run")
	}
	if entry.LastRun.ExitCode != 3 {
		t.Errorf("lastRun.exitCode = %d, want 3", entry.LastRun.ExitCode)
	}
	if entry.Schedule.Cron != everyMinute {
		t.Errorf("the listed job carries schedule %q, want %q", entry.Schedule.Cron, everyMinute)
	}
}

// decodeJSON reads one answer of the API into the shape the daemon wrote it
// from, which is what a client of it does.
func decodeJSON(t *testing.T, body []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(body, into); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
}

// isZero reads a number the store answered, whatever shape JSON gave it.
func isZero(value any) bool { return isCode(value, 0) }

// isCode reports whether an attribute the store answered is one number.
func isCode(value any, want int) bool {
	switch n := value.(type) {
	case int:
		return n == want
	case int64:
		return int(n) == want
	case float64:
		return int(n) == want
	default:
		return false
	}
}

// envValue reads one KEY=value out of an environment file body.
func envValue(body, key string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		if name, value, found := strings.Cut(strings.TrimSpace(line), "="); found && name == key {
			return value, true
		}
	}
	return "", false
}

// The loop itself. Every other test here calls due and runScheduled by hand,
// so this is the one that proves ScheduleLoop starts a container on its own,
// at the tick and not before, and starts it once.
func TestTheLoopStartsTheContainerOnItsOwn(t *testing.T) {
	h, fake := supervised(t)
	// The daemon's clock sits just before a minute boundary, so the loop has a
	// tick to wait for that a test can afford to wait out.
	base := time.Now()
	boundary := base.Truncate(time.Minute).Add(time.Minute)
	offset := boundary.Add(-300 * time.Millisecond).Sub(base)
	h.server.now = func() time.Time { return time.Now().Add(offset) }

	req := scheduleRegistration(everyMinute)
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.server.ScheduleLoop(ctx)
	}()

	waitFor(t, "the loop to start the container of the first tick", func() bool {
		return len(fake.Runs()) > 0
	})
	if got := len(fake.Runs()); got != 1 {
		t.Fatalf("the loop started %d containers, want exactly 1", got)
	}
	// And not again for the same minute.
	time.Sleep(700 * time.Millisecond)
	if got := len(fake.Runs()); got != 1 {
		t.Errorf("the loop started %d containers, want 1: a tick is a minute", got)
	}
	waitFor(t, "the record of the run", func() bool { return len(h.scheduleRecords()) == 1 })
	cancel()
	if _, returned := recvWithin(done); !returned {
		t.Errorf("waited %s for ScheduleLoop to return after the context was cancelled", waitBudget)
	}
}

// A clock that moves backwards, which is what a correction does, must not take
// a tick that was already taken, and one that jumps forward over four ticks
// runs one.
func TestAClockThatMovesDoesNotRunATickTwice(t *testing.T) {
	h, fake := supervised(t)
	req := scheduleRegistration(everyMinute)
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Minute)
	if run, _ := h.tick(at); len(run) != 1 {
		t.Fatalf("%d jobs ran at the first tick, want 1", len(run))
	}

	run, missed := h.tick(at.Add(-5 * time.Minute))
	if len(run) != 0 || len(missed) != 0 {
		t.Errorf("after the clock went back the daemon ran %d and skipped %d ticks, want none of either",
			len(run), len(missed))
	}
	// And forward again over four minutes: one tick, not four. Ticks are not
	// made up, whichever way the clock moved.
	if run, _ = h.tick(at.Add(4 * time.Minute)); len(run) != 1 {
		t.Errorf("after the clock jumped forward the daemon ran %d ticks, want 1", len(run))
	}
	if got := len(fake.Runs()); got != 2 {
		t.Errorf("the runtime made %d containers, want 2", got)
	}
}

// The variables kitbashd speaks for are dropped at a tick, not only at a
// start. The registration is a member's claim and it travels through a column,
// so this is the one place a stored KITBASH_ name could reach podman.
func TestTheEnvironmentOfATickDropsWhatKitbashdSpeaksFor(t *testing.T) {
	h, fake := supervised(t)
	req := scheduleRegistration(everyMinute)
	req.Schedule.Env = map[string]string{
		"CITY":               "taipei",
		EnvTelemetryToken:    "stolen-token",
		EnvUser:              "root",
		EnvFanoutSecret:      "stolen-secret",
		EnvProcess:           "0199a000-0000-7000-8000-0000000000ff",
		EnvTelemetryEndpoint: "http://evil.invalid",
	}
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}

	h.tick(time.Now().Add(time.Hour))

	runs := fake.Runs()
	if len(runs) != 1 {
		t.Fatalf("the tick made %d containers, want 1", len(runs))
	}
	env := runs[0].Env
	for _, stolen := range []string{"stolen-token", "stolen-secret", "evil.invalid"} {
		if strings.Contains(env, stolen) {
			t.Errorf("the run carries %q out of the registration:\n%s", stolen, env)
		}
	}
	if got, _ := envValue(env, EnvUser); got != h.user {
		t.Errorf("%s = %q, want the owner %q", EnvUser, got, h.user)
	}
	if got, _ := envValue(env, EnvProcess); got != req.ID {
		t.Errorf("%s = %q, want the Process the tick is of", EnvProcess, got)
	}
	if got, _ := envValue(env, "CITY"); got != "taipei" {
		t.Errorf("CITY = %q, want the unit's own value", got)
	}
}

// A tick runs as the owner, in that Process's cgroup, with the values the
// owner holds now. A credential the owner took away is a run that does not
// start rather than one that runs without it, and neither tick leaves an
// environment file behind.
func TestATickRunsAsTheOwnerWithTheirCurrentSecrets(t *testing.T) {
	h, fake := supervised(t)
	req := scheduleRegistration(everyMinute)
	req.Secrets = []string{"ANTHROPIC_API_KEY"}
	h.seedSecret(h.user, "ANTHROPIC_API_KEY", secretValue)
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Minute)
	h.tick(at)

	runs := fake.Runs()
	if len(runs) != 1 {
		t.Fatalf("the tick made %d containers, want 1", len(runs))
	}
	if runs[0].Member != h.user {
		t.Errorf("the tick ran the container as %q, want the owner %q", runs[0].Member, h.user)
	}
	if want := cgroups.Parent(h.user, req.ID); runs[0].Options.CgroupParent != want {
		t.Errorf("cgroup parent = %q, want %q", runs[0].Options.CgroupParent, want)
	}
	if !strings.Contains(runs[0].Env, "ANTHROPIC_API_KEY="+secretValue) {
		t.Errorf("the run was not given the owner's current value of the secret it declared")
	}

	res, body := h.do(http.MethodDelete, secretsPath+"/ANTHROPIC_API_KEY", "", nil)
	if res.StatusCode != http.StatusNoContent && res.StatusCode != http.StatusOK {
		t.Fatalf("removing the secret: %d %s", res.StatusCode, body)
	}
	h.tick(at.Add(time.Minute))
	if got := len(fake.Runs()); got != 1 {
		t.Errorf("the tick after the secret was removed made %d containers, want no more", got)
	}
	records := h.scheduleRecords()
	if len(records) != 2 {
		t.Fatalf("the two ticks wrote %d records, want 2", len(records))
	}
	if got := records[0].Attributes.Other[AttrScheduleResult]; got != ScheduleFailedToStart {
		t.Errorf("the second tick is recorded %v, want %s", got, ScheduleFailedToStart)
	}
	// And no environment file of either tick is left on the host: each one
	// holds a live token.
	left, err := os.ReadDir(h.server.envDir)
	if err == nil && len(left) != 0 {
		names := make([]string, 0, len(left))
		for _, entry := range left {
			names = append(names, entry.Name())
		}
		t.Errorf("the ticks left %v behind, and an environment file holds a token", names)
	}
}

// The race a tick runs, made deterministic: the ticker holds the job it took
// from due, the member stops it, and the tick goes on with what it is holding.
// The registration must stay deleted, because the write that mints a run's
// token is an update and never an insert, and nothing may be started for a
// Process nobody holds.
func TestATickThatLostItsRegistrationResurrectsNothing(t *testing.T) {
	h, fake := supervised(t)
	req := scheduleRegistration(everyMinute)
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	ctx := context.Background()
	// What the ticker is holding when the tick is handed over.
	run, _, _ := h.server.jobs.due(time.Now().Add(time.Hour))
	if len(run) != 1 {
		t.Fatalf("%d jobs were due, want 1", len(run))
	}
	res, body := h.do(http.MethodDelete, processesPath+"/"+req.ID, "", nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("unregister status = %d, body %s", res.StatusCode, body)
	}

	h.server.runScheduled(ctx, run[0])

	if _, found, _ := h.store.Process(ctx, req.ID); found {
		t.Error("the tick put the registration proc_stop deleted back")
	}
	if got := len(fake.Runs()); got != 0 {
		t.Errorf("the tick made %d containers for a Process that was unregistered, want none", got)
	}
	// And it asked the runtime for nothing at all: the registration is read
	// under the lock, before the container of the previous run is removed, so
	// a tick that lost its Process does nothing rather than most of it.
	if got := len(fake.Removals()); got != 0 {
		t.Errorf("the tick made %d runtime calls for a Process nobody holds, want none", got)
	}
	if got := len(h.scheduleRecords()); got != 0 {
		t.Errorf("the tick wrote %d records about a Process nobody holds, want none", got)
	}
	// And the daemon that comes after this one holds no job.
	h.server.loadJobs(ctx)
	if got := h.server.jobs.count(); got != 0 {
		t.Errorf("the next start of the daemon holds %d jobs, want none", got)
	}
}

// And the two cannot interleave at all: unregistering takes the same action
// lock a tick holds while it reads the registration and starts the run, so a
// proc_stop is either wholly before a tick or wholly after it.
func TestUnregisteringWaitsForTheActionLock(t *testing.T) {
	h, _ := supervised(t)
	req := scheduleRegistration(everyMinute)
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}

	// A tick is reading this Process and about to mint the token of its run.
	unlock := h.server.actions.lock(req.ID)
	answered := make(chan int, 1)
	go func() {
		res, _ := h.do(http.MethodDelete, processesPath+"/"+req.ID, "", nil)
		answered <- res.StatusCode
	}()
	select {
	case status := <-answered:
		t.Fatalf("the unregister answered %d while the lock was held, so it can land inside a tick", status)
	case <-time.After(100 * time.Millisecond):
	}

	unlock()
	status, returned := recvWithin(answered)
	if !returned {
		t.Fatalf("waited %s for the unregister to finish once the lock was free", waitBudget)
	}
	if status != http.StatusNoContent {
		t.Fatalf("unregister status = %d", status)
	}
	if _, found, _ := h.store.Process(context.Background(), req.ID); found {
		t.Error("the registration is still there after the unregister")
	}
}

// Removing a member takes their jobs with them, and a tick that raced the
// removal does not put one back: a job of an account this host no longer has
// would start a container as nobody.
func TestRemovingAMemberTakesTheirJobs(t *testing.T) {
	fake := sysusers.NewFake()
	h := serveWith(t, Options{
		Admin:  func(*user.User) (bool, error) { return true, nil },
		Users:  fake,
		Runner: fake,
	})
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid()})
	fake.Add(sysusers.Member{Name: builderName})
	id := h.scheduledProcess(builderName, "kitbash-weather-kim", eightDaily)
	h.server.loadJobs(context.Background())
	if got := h.server.jobs.count(); got != 1 {
		t.Fatalf("the daemon holds %d jobs, want 1", got)
	}

	res, body := h.do(http.MethodDelete, usersPath+"/"+builderName, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("users_remove status = %d, body %s", res.StatusCode, body)
	}

	if got := h.server.jobs.count(); got != 0 {
		t.Errorf("the daemon holds %d jobs of a member it no longer has", got)
	}
	if _, found, _ := h.store.Process(context.Background(), id); found {
		t.Errorf("the registration of a removed member's job is still there")
	}
}

// The token a run is given dies with the run. The container is kept so
// proc_logs can read it, and a container that has exited is not a producer.
func TestTheTokenOfARunIsRevokedWhenItEnds(t *testing.T) {
	h, fake := supervised(t)
	req := scheduleRegistration(everyMinute)
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}

	h.tick(time.Now().Add(time.Hour))

	token, found := envValue(fake.Runs()[0].Env, EnvTelemetryToken)
	if !found || token == "" {
		t.Fatalf("the run was given no %s", EnvTelemetryToken)
	}
	_, live, err := h.store.ProcessByToken(context.Background(), token)
	if err != nil {
		t.Fatalf("ProcessByToken: %v", err)
	}
	if live {
		t.Error("the token minted for the run still answers after the run ended")
	}
	// The registration is untouched: the job is still a job.
	held, found, err := h.store.Process(context.Background(), req.ID)
	if err != nil || !found {
		t.Fatalf("the registration is gone: %v, found %v", err, found)
	}
	if !held.Schedule.Declared() {
		t.Error("revoking the run's token took the schedule with it")
	}
}

// A wait that fails for a reason that is not the ceiling is a run this host
// lost track of, with the class of the failure on the record: reading it as
// the six hour stop would say the container ran for six hours and was killed.
func TestAWaitThatFailsIsRecordedAsLost(t *testing.T) {
	h, fake := supervised(t)
	req := scheduleRegistration(everyMinute)
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	fake.WaitErr = errors.New("podman wait: connection refused")

	h.tick(time.Now().UTC().Add(time.Hour).Truncate(time.Minute))

	records := h.scheduleRecords()
	if len(records) != 1 {
		t.Fatalf("the tick wrote %d records, want 1", len(records))
	}
	record := records[0]
	if got := record.Attributes.Other[AttrScheduleResult]; got != ScheduleLost {
		t.Errorf("%s = %v, want %s", AttrScheduleResult, got, ScheduleLost)
	}
	if got := record.Attributes.Other[AttrScheduleError]; got != lostRuntime {
		t.Errorf("%s = %v, want %s", AttrScheduleError, got, lostRuntime)
	}
	if _, held := record.Attributes.Other[AttrScheduleExitCode]; held {
		t.Errorf("a run nobody knows the end of carries an exit status: %v",
			record.Attributes.Other[AttrScheduleExitCode])
	}
	if record.Value != notRanValue {
		t.Errorf("value = %v, want %v", record.Value, float64(notRanValue))
	}
	// The container the runtime would not answer for is not stopped: nothing
	// here knows what state it is in.
	if got := len(fake.Stops()); got != 0 {
		t.Errorf("a lost run stopped %d containers, want none", got)
	}
}

// A daemon that is stopping while a container is running says so and nothing
// more: the run is not over, and a record of a run that ended would be wrong
// in both directions.
func TestARunInterruptedByTheDaemonIsRecordedAsSuch(t *testing.T) {
	h, fake := supervised(t)
	req := scheduleRegistration(everyMinute)
	fake.Waiting = map[string]chan int{req.Container: make(chan int)}
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	run, _, _ := h.server.jobs.due(time.Now().Add(time.Hour))
	if len(run) != 1 {
		t.Fatalf("%d jobs were due, want 1", len(run))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.server.runScheduled(ctx, run[0])
	}()
	waitFor(t, "the run to start", func() bool { return len(fake.Waits()) > 0 })

	cancel()
	if _, returned := recvWithin(done); !returned {
		t.Fatalf("waited %s for the run to give up with the daemon", waitBudget)
	}

	records := h.scheduleRecords()
	if len(records) != 1 {
		t.Fatalf("the interrupted run wrote %d records, want 1", len(records))
	}
	if got := records[0].Attributes.Other[AttrScheduleResult]; got != ScheduleInterrupted {
		t.Errorf("%s = %v, want %s", AttrScheduleResult, got, ScheduleInterrupted)
	}
	// The container was not stopped: the daemon is going, the run is not.
	if got := len(fake.Stops()); got != 0 {
		t.Errorf("a daemon that stopped also stopped %d containers of runs, want none", got)
	}
}

// One member's jobs take their share of the host and no more, so another
// member's tick is not skipped for reasons that have nothing to do with them.
// The order is fixed too: which job loses is the same answer every time.
func TestOneMemberTakesOnlyTheirShareOfTheHost(t *testing.T) {
	fake := sysusers.NewFake()
	h := serveWith(t, Options{
		Admin:  func(*user.User) (bool, error) { return false, nil },
		Users:  fake,
		Runner: fake,
	})
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid()})
	fake.Add(sysusers.Member{Name: builderName})
	// The noisy member holds more jobs than their share, and every run of
	// theirs is still going.
	fake.Waiting = map[string]chan int{}
	for i := range MaxRunsPerMember + 2 {
		container := fmt.Sprintf("kitbash-weather-noisy%d", i)
		fake.Waiting[container] = make(chan int)
		h.scheduledProcess(h.user, container, everyMinute)
	}
	quiet := h.scheduledProcess(builderName, "kitbash-weather-quiet", everyMinute)
	h.server.loadJobs(context.Background())

	at := time.Now().UTC().Add(time.Hour).Truncate(time.Minute)
	run, missed, _ := h.server.jobs.due(at)

	var mine, theirs int
	for _, target := range run {
		if target.owner == h.user {
			mine++
		} else {
			theirs++
		}
	}
	if mine != MaxRunsPerMember {
		t.Errorf("the noisy member took %d of the host's slots, want %d", mine, MaxRunsPerMember)
	}
	if theirs != 1 {
		t.Errorf("the other member's job ran %d times, want 1: their tick is not the noisy member's fault", theirs)
	}
	for _, target := range missed {
		if target.job.id == quiet {
			t.Errorf("the other member's tick was skipped as %s while one member held the slots", target.reason)
		}
	}
	if len(missed) != 2 {
		t.Errorf("%d ticks were skipped, want the noisy member's two", len(missed))
	}
	for _, target := range missed {
		if target.reason != SkipBusy {
			t.Errorf("a tick beyond the member's share reads %q, want %q", target.reason, SkipBusy)
		}
	}
	for _, held := range fake.Waiting {
		close(held)
	}
}

// The jobs due at one moment are taken in a fixed order, so which one loses on
// a busy host does not depend on what the runtime hashed last.
func TestTheJobsDueAtOneMomentAreTakenInAFixedOrder(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Minute)
	cron, err := manifest.ParseCron(everyMinute)
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	var seen []string
	for range 8 {
		sc := newScheduler(1, 8)
		for _, id := range []string{"01930000-c", "01930000-a", "01930000-b"} {
			sc.jobs[id] = &job{id: id, owner: "loki", cron: cron, next: at}
		}
		run, missed, _ := sc.due(at)
		if len(run) != 1 || len(missed) != 2 {
			t.Fatalf("%d ran and %d were skipped under a cap of 1", len(run), len(missed))
		}
		seen = append(seen, run[0].id)
	}
	for _, got := range seen {
		if got != seen[0] {
			t.Fatalf("the winner of a busy host is %v over eight passes, want the same job every time", seen)
		}
	}
	if seen[0] != "01930000-a" {
		t.Errorf("the job that ran is %q, want the lowest id of the three due at once", seen[0])
	}
}

// A job whose expression names no date the calendar has is put a day out
// rather than left due: a job that is due for ever is one every pass of the
// loop tries to run.
func TestAJobWithNoTickIsNotDueForEver(t *testing.T) {
	h, fake := supervised(t)
	// The 30th of February, which parses and matches no day.
	cron, err := manifest.ParseCron("0 0 30 2 *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	at := time.Now().UTC().Truncate(time.Minute)
	h.server.jobs.jobs["01930000-0000-7000-8000-00000000000a"] = &job{
		id: "01930000-0000-7000-8000-00000000000a", owner: h.user,
		pkg: "/home/tester/weather", container: "kitbash-weather-weather",
		cron: cron, next: at,
	}

	run, missed, wait := h.server.jobs.due(at)
	if len(run) != 1 || len(missed) != 0 {
		t.Fatalf("%d jobs ran and %d were skipped, want the one run", len(run), len(missed))
	}
	// The job is not due again at the next pass, which is the thing to prove.
	h.server.jobs.finished(run[0].id, run[0].owner, lastRun{})
	again, _, _ := h.server.jobs.due(at.Add(time.Minute))
	if len(again) != 0 {
		t.Errorf("the job is due again a minute later, so every pass of the loop would run it")
	}
	if wait < time.Minute {
		t.Errorf("the loop would look again in %s, want it to wait", wait)
	}
	_ = fake
}
