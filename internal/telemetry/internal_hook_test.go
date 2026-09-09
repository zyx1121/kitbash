package telemetry_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
)

// The cause of an internal problem never reaches the agent, so the one way an
// admin reads it is as a log record. The provider of the session is what puts
// it on the wire, see PLAN.md section 2.4.
func TestAnInternalCauseIsExportedAsALogRecord(t *testing.T) {
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

	records := daemon.Logs()
	if len(records) != 1 {
		t.Fatalf("want one log record, got %+v", records)
	}
	got := records[0]
	if got.Body != "git commit: exit status 128" {
		t.Errorf("the record body is %q, want the cause", got.Body)
	}
	if got.Severity != "ERROR" {
		t.Errorf("severity is %q, want ERROR", got.Severity)
	}
	if got.Attributes[telemetry.AttrInternal] != "true" {
		t.Errorf("the record carries %v, want %s true so only admins are answered it",
			got.Attributes, telemetry.AttrInternal)
	}
	if got.Attributes[telemetry.AttrError] != problem.SlugInternal {
		t.Errorf("%s is %q, want %q", telemetry.AttrError,
			got.Attributes[telemetry.AttrError], problem.SlugInternal)
	}
	if got.Attributes[telemetry.AttrTool] != "fs_write" {
		t.Errorf("%s is %q, want the call in flight", telemetry.AttrTool, got.Attributes[telemetry.AttrTool])
	}
	if got.Attributes[telemetry.AttrPath] != "/org/handbook/README.md" {
		t.Errorf("%s is %q, want the problem instance the agent was given",
			telemetry.AttrPath, got.Attributes[telemetry.AttrPath])
	}
}

// The tool is read from the call in flight, so a cause raised with none open
// still carries the instance and is still exported.
func TestAnInternalCauseOutsideACallCarriesNoTool(t *testing.T) {
	daemon := newDaemon(t)
	p, _ := newProvider(t, daemon.Socket)
	defer problem.OnInternal(nil)

	problem.Internal("/kitbash/v1/processes", "dial unix /run/kitbash/kitbashd.sock: no such file", "")
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	records := daemon.Logs()
	if len(records) != 1 {
		t.Fatalf("want one log record, got %+v", records)
	}
	if tool, sent := records[0].Attributes[telemetry.AttrTool]; sent {
		t.Errorf("%s is %q, want no tool when no call is in flight", telemetry.AttrTool, tool)
	}
	if records[0].Attributes[telemetry.AttrInternal] != "true" {
		t.Errorf("the record carries %v, want %s true", records[0].Attributes, telemetry.AttrInternal)
	}
}

// A provider that has shut down exports nothing, so a cause raised after the
// session is over is not a write to a closed exporter.
func TestShutdownStopsRecordingCauses(t *testing.T) {
	daemon := newDaemon(t)
	p, _ := newProvider(t, daemon.Socket)
	defer problem.OnInternal(nil)

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	problem.Internal("/org/handbook/README.md", "a cause nobody records", "")
	if records := daemon.Logs(); len(records) != 0 {
		t.Errorf("a session that has ended exported %+v", records)
	}
}
