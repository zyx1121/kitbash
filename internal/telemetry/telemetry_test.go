package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
	"github.com/zyx1121/kitbash/internal/telemetry/teltest"
)

// newProvider builds a session's telemetry over one socket and returns the log
// a dropping session would write to.
func newProvider(t *testing.T, socket string) (*telemetry.Provider, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	p, err := telemetry.New(telemetry.Options{
		Version: "test",
		Socket:  socket,
		Logger:  log.New(logs, "kitbash: ", 0),
	})
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	return p, logs
}

func newDaemon(t *testing.T) *teltest.Daemon {
	t.Helper()
	daemon, err := teltest.Start()
	if err != nil {
		t.Fatalf("teltest.Start: %v", err)
	}
	t.Cleanup(daemon.Close)
	return daemon
}

func TestSocketPathIsOverridableOnlyOutsideAnSSHSession(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv(telemetry.SocketEnv, "/tmp/kitbashd-test.sock")
	if got := telemetry.SocketPath(); got != "/tmp/kitbashd-test.sock" {
		t.Errorf("outside SSH the socket is %q, want the override", got)
	}
	// A member connecting through ForceCommand does not choose the socket,
	// the same rule fs.RootsEnv follows.
	t.Setenv("SSH_CONNECTION", "10.0.0.1 52000 10.0.0.2 22")
	if got := telemetry.SocketPath(); got != telemetry.Socket {
		t.Errorf("inside SSH the socket is %q, want %s", got, telemetry.Socket)
	}
}

func TestSpansAndLogRecordsReachTheSocket(t *testing.T) {
	daemon := newDaemon(t)
	p, _ := newProvider(t, daemon.Socket)

	ctx, span := p.StartTool(context.Background(), "pkg_build", "tester")
	span.SetPackage("/org/ffmpeg")
	span.SetPath("/org/ffmpeg")
	_, child := telemetry.Start(ctx, "build")
	child.SetDigest("sha256:abc")
	child.Info("COMMIT localhost/kitbash/ffmpeg:test")
	child.End()
	span.OK()
	span.End()

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	tool, only := daemon.Span("pkg_build")
	if !only {
		t.Fatalf("want one pkg_build span, got %+v", daemon.Spans())
	}
	if tool.Attributes[telemetry.AttrUser] != "tester" || tool.Attributes[telemetry.AttrTool] != "pkg_build" {
		t.Errorf("the tool span carries %v, want the user and the tool", tool.Attributes)
	}
	build, only := daemon.Span("build")
	if !only {
		t.Fatalf("want one build span, got %+v", daemon.Spans())
	}
	if build.ParentSpanID != tool.SpanID {
		t.Errorf("the build span's parent is %s, want %s", build.ParentSpanID, tool.SpanID)
	}
	records := daemon.Logs()
	if len(records) != 1 {
		t.Fatalf("want one log record, got %+v", records)
	}
	if records[0].Body != "COMMIT localhost/kitbash/ffmpeg:test" {
		t.Errorf("the record body is %q", records[0].Body)
	}
	if records[0].Attributes[telemetry.AttrDigest] != "sha256:abc" {
		t.Errorf("the record carries %v, want the span's own attributes", records[0].Attributes)
	}
}

// A helper called outside a call is a helper that does nothing, so a service
// never has to ask whether this call is traced.
func TestTheHelpersAreNoOpsWithoutASpan(t *testing.T) {
	ctx := context.Background()
	telemetry.SetPackage(ctx, "/org/ffmpeg")
	telemetry.SetProcess(ctx, "01a0")
	telemetry.SetPath(ctx, "/org/ffmpeg")
	if _, span := telemetry.Start(ctx, "build"); span != nil {
		t.Error("Start opened a span outside a call")
	}
	var span *telemetry.Span
	span.SetPackage("/org/ffmpeg")
	span.Info("nothing is listening")
	span.Fail("internal", "Internal error")
	span.End()
}

func TestAMissingSocketLogsOneLinePerSession(t *testing.T) {
	p, logs := newProvider(t, filepath.Join(t.TempDir(), "absent.sock"))

	for range 5 {
		_, span := p.StartTool(context.Background(), "fs_list", "tester")
		span.Info("a record nobody will store")
		span.End()
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	written := strings.TrimSpace(logs.String())
	if written == "" {
		t.Fatal("a dropping session logged nothing")
	}
	if lines := strings.Count(written, "\n") + 1; lines != 1 {
		t.Errorf("a dropping session logged %d lines, want one:\n%s", lines, written)
	}
	if !strings.Contains(written, "dropping") {
		t.Errorf("the line is %q, want it to say records are dropped", written)
	}
}

// A daemon that goes away and comes back is exported to again. Dropping is a
// state of the socket, not a verdict on the session: an upgrade or a restart
// mid session must not leave an agent's work unrecorded until it reconnects.
func TestExportResumesWhenTheDaemonComesBack(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "kbd.sock")
	first, err := teltest.StartAt(socket)
	if err != nil {
		t.Fatalf("teltest.StartAt: %v", err)
	}
	p, logs := newProvider(t, socket)
	ctx := context.Background()

	record := func(tool string) {
		_, span := p.StartTool(ctx, tool, "tester")
		span.OK()
		span.End()
		if err := p.ForceFlush(ctx); err != nil {
			t.Fatalf("flushing %s: %v", tool, err)
		}
	}

	record("fs_list")
	first.Close()
	record("fs_read")

	second, err := teltest.StartAt(socket)
	if err != nil {
		t.Fatalf("restarting the daemon: %v", err)
	}
	defer second.Close()
	record("fs_write")
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if _, only := second.Span("fs_write"); !only {
		t.Fatalf("the call after the restart did not reach the daemon, it saw %+v", second.Spans())
	}
	if _, seen := second.Span("fs_read"); seen {
		t.Error("the daemon that was down received the call it missed")
	}
	written := logs.String()
	if !strings.Contains(written, "dropping") {
		t.Errorf("the log is %q, want the line that says records were dropped", written)
	}
	if !strings.Contains(written, "resumed") {
		t.Errorf("the log is %q, want the line that says export resumed", written)
	}
}

// A daemon that accepts the connection and never answers must not hold the
// session open past the shutdown budget.
func TestAHungDaemonDoesNotOutlastTheShutdownBudget(t *testing.T) {
	daemon := newDaemon(t)
	daemon.Delay(30 * time.Second)
	p, _ := newProvider(t, daemon.Socket)

	_, span := p.StartTool(context.Background(), "fs_list", "tester")
	span.Info("a record the daemon will never acknowledge")
	span.OK()
	span.End()

	start := time.Now()
	// The error is the point: what matters is that it comes back.
	_ = p.Shutdown(context.Background())
	if elapsed := time.Since(start); elapsed > telemetry.ShutdownTimeout+2*time.Second {
		t.Errorf("shutdown took %s, want it bounded by the %s budget", elapsed, telemetry.ShutdownTimeout)
	}
}

func TestQueryPostsTheInputAndReturnsTheBody(t *testing.T) {
	daemon := newDaemon(t)
	answer := `{"signal":"traces","records":[],"truncated":false}`
	daemon.AnswerQuery(teltest.Response{Status: http.StatusOK, ContentType: "application/json", Body: answer})
	client := telemetry.NewClient(daemon.Socket)

	body, prob := client.Query(context.Background(), json.RawMessage(`{"signal":"traces"}`))
	if prob != nil {
		t.Fatalf("query: %s", prob.Detail)
	}
	if string(body) != answer {
		t.Errorf("query returned %s, want %s", body, answer)
	}
	calls := daemon.Calls()
	if len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Path != telemetry.QueryPath {
		t.Fatalf("the daemon saw %+v, want one POST to %s", calls, telemetry.QueryPath)
	}
	if calls[0].Body != `{"signal":"traces"}` {
		t.Errorf("the daemon was sent %q, want the input verbatim", calls[0].Body)
	}
}

func TestADaemonProblemIsReturnedUnchanged(t *testing.T) {
	daemon := newDaemon(t)
	daemon.AnswerQuery(teltest.Problem(http.StatusForbidden, problem.SlugNotPermitted,
		"Not permitted", "only an admin may name another member", "Query your own records."))
	client := telemetry.NewClient(daemon.Socket)

	_, prob := client.Query(context.Background(), json.RawMessage(`{"signal":"traces","user":"root"}`))
	if prob == nil {
		t.Fatal("a 403 came back as a success")
	}
	if prob.Slug() != problem.SlugNotPermitted || prob.Status != http.StatusForbidden {
		t.Errorf("problem is %s %d, want not-permitted 403", prob.Slug(), prob.Status)
	}
	if prob.Detail != "only an admin may name another member" || prob.Fix != "Query your own records." {
		t.Errorf("problem is %+v, want the daemon's own detail and fix", prob)
	}
}

func TestRetentionReadsAndSets(t *testing.T) {
	daemon := newDaemon(t)
	client := telemetry.NewClient(daemon.Socket)

	if _, prob := client.Retention(context.Background(), nil); prob != nil {
		t.Fatalf("reading retention: %s", prob.Detail)
	}
	if _, prob := client.Retention(context.Background(), json.RawMessage(`{"logs":"7d"}`)); prob != nil {
		t.Fatalf("setting retention: %s", prob.Detail)
	}
	calls := daemon.Calls()
	if len(calls) != 2 {
		t.Fatalf("the daemon saw %+v, want two calls", calls)
	}
	if calls[0].Method != http.MethodGet {
		t.Errorf("reading was %s, want GET", calls[0].Method)
	}
	if calls[1].Method != http.MethodPut || calls[1].Body != `{"logs":"7d"}` {
		t.Errorf("setting was %s %q, want PUT with the set object", calls[1].Method, calls[1].Body)
	}
}

// A daemon that answers 200 with something that is not JSON is broken, not
// permitted to hand the agent whatever it sent.
func TestANonJSONAnswerIsAnInternalProblem(t *testing.T) {
	daemon := newDaemon(t)
	daemon.AnswerQuery(teltest.Response{Status: http.StatusOK, ContentType: "text/html",
		Body: "<html>not the daemon you were looking for</html>"})
	client := telemetry.NewClient(daemon.Socket)

	body, prob := client.Query(context.Background(), json.RawMessage(`{"signal":"traces"}`))
	if prob == nil {
		t.Fatalf("a body that is not JSON came back as a success: %s", body)
	}
	if prob.Slug() != problem.SlugInternal {
		t.Errorf("problem is %s, want internal", prob.Slug())
	}
	if strings.Contains(prob.Detail, "html") {
		t.Errorf("detail is %q, want the caller not to be handed the broken body", prob.Detail)
	}
}

// A daemon that accepts the connection and never answers is an internal
// problem with the same advice as one that is not there, and the call comes
// back rather than hanging the agent.
func TestAHungDaemonIsAnInternalProblem(t *testing.T) {
	daemon := newDaemon(t)
	daemon.Delay(30 * time.Second)
	client := telemetry.NewClient(daemon.Socket)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, prob := client.Query(ctx, json.RawMessage(`{"signal":"traces"}`))
	if prob == nil {
		t.Fatal("querying a hung daemon succeeded")
	}
	if prob.Slug() != problem.SlugInternal {
		t.Errorf("problem is %s, want internal", prob.Slug())
	}
	if prob.Fix != telemetry.NotRunningFix {
		t.Errorf("fix is %q, want %q", prob.Fix, telemetry.NotRunningFix)
	}
}

func TestAnAbsentDaemonIsAnInternalProblemWithAFix(t *testing.T) {
	client := telemetry.NewClient(filepath.Join(t.TempDir(), "absent.sock"))

	_, prob := client.Query(context.Background(), json.RawMessage(`{"signal":"traces"}`))
	if prob == nil {
		t.Fatal("querying an absent daemon succeeded")
	}
	if prob.Slug() != problem.SlugInternal {
		t.Errorf("problem is %s, want internal", prob.Slug())
	}
	if prob.Fix != telemetry.NotRunningFix {
		t.Errorf("fix is %q, want %q", prob.Fix, telemetry.NotRunningFix)
	}
	if prob.Instance != "tel_query" {
		t.Errorf("instance is %q, want tel_query", prob.Instance)
	}
}
