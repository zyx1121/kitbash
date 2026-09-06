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
