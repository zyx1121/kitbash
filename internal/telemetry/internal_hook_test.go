package telemetry_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
)

// The cause of an internal problem never reaches the agent, so the one way an
// admin reads it is as a record kitbashd stores. No export may claim
// kitbash.internal, so the session reports the cause to the daemon's own path
// instead, see PLAN.md section 2.4.
func TestAnInternalCauseIsReportedToTheDaemon(t *testing.T) {
	daemon := newDaemon(t)
	p, _ := newProvider(t, daemon.Socket)
	defer problem.OnInternal(nil)

	_, span := p.StartTool(context.Background(), "fs_write", "tester")
	prob := problem.Internal("/org/handbook/README.md", "git commit: exit status 128", "")
	span.Fail(prob.Slug(), prob.Title)
	span.End()
	if strings.Contains(prob.Detail, "exit status 128") {
		t.Fatal("the cause reached the detail the agent reads")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	causes := daemon.InternalCauses()
	if len(causes) != 1 {
		t.Fatalf("want one reported cause, got %+v", causes)
	}
	if causes[0].Cause != "git commit: exit status 128" {
		t.Errorf("the reported cause is %q, want the cause", causes[0].Cause)
	}
	if causes[0].Instance != "/org/handbook/README.md" {
		t.Errorf("instance is %q, want the one the agent was given", causes[0].Instance)
	}
	if causes[0].Tool != "fs_write" {
		t.Errorf("tool is %q, want the call in flight", causes[0].Tool)
	}
	// The cause is not exported: an export claiming kitbash.internal is
	// dropped by the daemon, so sending one would only be a record nobody
	// could read as a cause.
	if records := daemon.Logs(); len(records) != 0 {
		t.Errorf("the session exported %+v, want the cause reported and not exported", records)
	}
}

// The tool is read from the call in flight, so a cause raised with none open
// is still reported, with the instance and no tool.
func TestAnInternalCauseOutsideACallCarriesNoTool(t *testing.T) {
	daemon := newDaemon(t)
	p, _ := newProvider(t, daemon.Socket)
	defer problem.OnInternal(nil)

	problem.Internal("/kitbash/v1/processes", "dial unix /run/kitbash/kitbashd.sock: no such file", "")
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	causes := daemon.InternalCauses()
	if len(causes) != 1 {
		t.Fatalf("want one reported cause, got %+v", causes)
	}
	if causes[0].Tool != "" {
		t.Errorf("tool is %q, want none when no call is in flight", causes[0].Tool)
	}
	if causes[0].Instance != "/kitbash/v1/processes" {
		t.Errorf("instance is %q, want the problem instance", causes[0].Instance)
	}
}

// A cause longer than the budget is cut rather than sent whole: kitbashd
// applies the same bound, and a request over its body limit would be refused
// entirely.
func TestALongInternalCauseIsClipped(t *testing.T) {
	daemon := newDaemon(t)
	p, _ := newProvider(t, daemon.Socket)
	defer problem.OnInternal(nil)

	problem.Internal("/org/handbook", strings.Repeat("x", telemetry.InternalCauseLimit*2), "")
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	causes := daemon.InternalCauses()
	if len(causes) != 1 {
		t.Fatalf("want one reported cause, got %d", len(causes))
	}
	if len(causes[0].Cause) > telemetry.InternalCauseLimit+64 {
		t.Errorf("the reported cause is %d bytes, want it cut to %d and a note",
			len(causes[0].Cause), telemetry.InternalCauseLimit)
	}
	if !strings.Contains(causes[0].Cause, "truncated") {
		t.Error("the cut cause does not say it was cut")
	}
}

// A session that has ended reports nothing, so a cause raised after it is not
// a request over a socket nobody is serving any more.
func TestShutdownStopsReportingCauses(t *testing.T) {
	daemon := newDaemon(t)
	p, _ := newProvider(t, daemon.Socket)
	defer problem.OnInternal(nil)

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	problem.Internal("/org/handbook/README.md", "a cause nobody records", "")
	if causes := daemon.InternalCauses(); len(causes) != 0 {
		t.Errorf("a session that has ended reported %+v", causes)
	}
}

// A daemon that is not there costs the session one line and no call: the
// surface still works, and the agent never learns that a cause was dropped.
func TestAMissingDaemonCostsOneLinePerSession(t *testing.T) {
	p, logs := newProvider(t, t.TempDir()+"/absent.sock")
	defer problem.OnInternal(nil)

	for range 3 {
		problem.Internal("/org/handbook/README.md", "a cause nobody stores", "")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	written := logs.String()
	if !strings.Contains(written, "not recording the causes") {
		t.Errorf("the session log is %q, want one line about the causes", written)
	}
	if lines := strings.Count(strings.TrimSpace(written), "\n") + 1; lines > 2 {
		t.Errorf("the session logged %d lines, want one per reason:\n%s", lines, written)
	}
}
