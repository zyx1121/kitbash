package server_test

import (
	"testing"

	"github.com/zyx1121/kitbash/internal/telemetry"
)

// caller is the Process a session was opened for, which kitbashd puts in the
// environment of the kitbash-mcp it starts, see PLAN.md section 2.3.
const caller = "01a07f95-df01-7073-add4-29e769a336f4"

// TestSpansCarryTheCallerWhenAProcessOpenedTheSession is the second half of
// "every span such a session records carries kitbash.caller": a session
// kitbashd opened for a Process records which Process it was, alongside the
// owner it acted as.
func TestSpansCarryTheCallerWhenAProcessOpenedTheSession(t *testing.T) {
	t.Setenv(telemetry.EnvCaller, caller)
	tr := newTracedWithDaemon(t)

	ok(t, call(t, tr.session, "fs_list", map[string]any{}), "fs_list")
	tr.flush(t)

	span, found := tr.daemon.Span("fs_list")
	if !found {
		t.Fatalf("the call produced no fs_list span: %+v", tr.daemon.Spans())
	}
	if got := span.Attributes[telemetry.AttrCaller]; got != caller {
		t.Errorf("%s is %q, want the Process that opened the session", telemetry.AttrCaller, got)
	}
	if got := span.Attributes[telemetry.AttrUser]; got != "tester" {
		t.Errorf("%s is %q, want the owner the session acts as", telemetry.AttrUser, got)
	}
}

// TestSpansCarryNoCallerForAMemberSession is the other half: a member's own
// session over SSH acts for nobody, so nothing claims it did.
func TestSpansCarryNoCallerForAMemberSession(t *testing.T) {
	tr := newTracedWithDaemon(t)

	ok(t, call(t, tr.session, "fs_list", map[string]any{}), "fs_list")
	tr.flush(t)

	span, found := tr.daemon.Span("fs_list")
	if !found {
		t.Fatalf("the call produced no fs_list span: %+v", tr.daemon.Spans())
	}
	if got, held := span.Attributes[telemetry.AttrCaller]; held {
		t.Errorf("%s is %q, want no caller at all", telemetry.AttrCaller, got)
	}
}
