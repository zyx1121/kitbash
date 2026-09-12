package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/bridge"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/pkg"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/proc"
	"github.com/zyx1121/kitbash/internal/server"
	"github.com/zyx1121/kitbash/internal/telemetry"
	"github.com/zyx1121/kitbash/internal/telemetry/teltest"
)

// traced is the whole surface with Telemetry pointed at a fake kitbashd.
type traced struct {
	*whole
	session  *mcp.ClientSession
	server   *mcp.Server
	daemon   *teltest.Daemon
	provider *telemetry.Provider
	bridge   *bridge.Bridge
	logs     *bytes.Buffer
}

// newTraced serves every family with a Provider on the given socket. A socket
// nothing is listening on is how the missing daemon is exercised.
func newTraced(t *testing.T, socket string) *traced {
	t.Helper()
	return newTracedPermits(t, socket, nil)
}

// newTracedPermits is newTraced narrowed by a permits block, which is how the
// session of a Process reaches the same surface. The block is built from the
// fixture because a prefix is a real path.
func newTracedPermits(t *testing.T, socket string, permitsFor func(w *whole) *manifest.Permits) *traced {
	t.Helper()
	w := newWhole(t)
	var permits *manifest.Permits
	if permitsFor != nil {
		permits = permitsFor(w)
	}
	logs := &bytes.Buffer{}
	provider, err := telemetry.New(telemetry.Options{
		Version: "test",
		Socket:  socket,
		Logger:  log.New(logs, "kitbash: ", 0),
	})
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}

	ctx := context.Background()
	// kitbashd is the one that runs a Process, so the registry is the same
	// client the tel family forwards through, see PLAN.md section 2.3.
	processes := proc.New(w.files, w.runner, provider.Client())
	tools := bridge.New(w.files, processes, w.runner)
	// The Package runs in this process instead of in a container, the way
	// internal/bridge tests reach one.
	packageServer := mcp.NewServer(&mcp.Implementation{Name: "ffmpeg", Version: "1"}, nil)
	packageServer.AddTool(&mcp.Tool{
		Name:        "transcode",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, transcode)
	tools.SetTransport(func(ctx context.Context, _ *proc.Process) (mcp.Transport, error) {
		clientSide, serverSide := mcp.NewInMemoryTransports()
		if _, err := packageServer.Connect(ctx, serverSide, nil); err != nil {
			return nil, err
		}
		return clientSide, nil
	})

	srv := server.New("test", server.Deps{
		Files:     w.files,
		Packages:  pkg.New(w.files, w.runner, tools),
		Processes: processes,
		Bridge:    tools,
		Telemetry: provider,
		Permits:   permits,
	})
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "kitbash-test", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		session.Close()
		serverSession.Wait()
		tools.Close()
	})
	return &traced{whole: w, session: session, server: srv, provider: provider, bridge: tools, logs: logs}
}

// newTracedWithDaemon is newTraced against a fake kitbashd that records what
// it receives.
func newTracedWithDaemon(t *testing.T) *traced {
	t.Helper()
	return newTracedWithDaemonPermits(t, nil)
}

// newTracedWithDaemonPermits is newTracedWithDaemon for the session of a
// Process: the same fake kitbashd, narrowed by a permits block.
func newTracedWithDaemonPermits(t *testing.T, permitsFor func(w *whole) *manifest.Permits) *traced {
	t.Helper()
	daemon, err := teltest.Start()
	if err != nil {
		t.Fatalf("teltest.Start: %v", err)
	}
	t.Cleanup(daemon.Close)
	tr := newTracedPermits(t, daemon.Socket, permitsFor)
	tr.daemon = daemon
	// A container the fake daemon starts shows up in the member's own runtime,
	// which is where the session reads a Process back from.
	daemon.MirrorRuns(tr.runner)
	return tr
}

// flush ends the session's export the way kitbash-mcp does, so every record is
// at the daemon before a test reads it.
func (tr *traced) flush(t *testing.T) {
	t.Helper()
	if err := tr.provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("telemetry shutdown: %v", err)
	}
}

// transcode is the Package tool the proxied call reaches.
func transcode(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil
}

// buildAndRun puts the ffmpeg Package on the surface through the surface, the
// way an agent would.
func buildAndRun(t *testing.T, tr *traced) proc.Process {
	t.Helper()
	ok(t, call(t, tr.session, "fs_write", map[string]any{
		"path":    filepath.Join(tr.folder, "kitbash.yaml"),
		"content": packageManifest,
		"message": "Add the ffmpeg Package",
	}), "fs_write")
	ok(t, call(t, tr.session, "fs_write", map[string]any{
		"path":    filepath.Join(tr.folder, "Containerfile"),
		"content": "FROM alpine\n",
		"message": "Add the Containerfile",
	}), "fs_write")
	res := call(t, tr.session, "pkg_build", map[string]any{"path": tr.folder})
	ok(t, res, "pkg_build")
	built := structured[pkg.BuildResult](t, res)
	res = call(t, tr.session, "proc_run", map[string]any{"package": tr.folder, "digest": built.Digest})
	ok(t, res, "proc_run")
	return structured[proc.Process](t, res)
}

func TestToolCallOpensExactlyOneSpan(t *testing.T) {
	tr := newTracedWithDaemon(t)

	ok(t, call(t, tr.session, "fs_list", map[string]any{}), "fs_list")
	tr.flush(t)

	spans := tr.daemon.Spans()
	if len(spans) != 1 {
		t.Fatalf("the call produced %d spans, want exactly one: %+v", len(spans), spans)
	}
	span := spans[0]
	if span.Name != "fs_list" {
		t.Errorf("span is named %q, want fs_list", span.Name)
	}
	if got := span.Attributes[telemetry.AttrUser]; got != "tester" {
		t.Errorf("%s is %q, want tester", telemetry.AttrUser, got)
	}
	if got := span.Attributes[telemetry.AttrTool]; got != "fs_list" {
		t.Errorf("%s is %q, want fs_list", telemetry.AttrTool, got)
	}
	if span.Status != "ok" {
		t.Errorf("status is %q, want ok", span.Status)
	}
	if got := span.Resource["service.name"]; got != telemetry.ServiceName {
		t.Errorf("service.name is %q, want %s", got, telemetry.ServiceName)
	}
	if got := span.Resource["service.version"]; got != "test" {
		t.Errorf("service.version is %q, want test", got)
	}
	// spec/kitbashd-api.yaml offers protobuf and JSON; the producer sends
	// protobuf, and the daemon has to be built for what it is actually sent.
	for _, contentType := range tr.daemon.ContentTypes() {
		if contentType != "application/x-protobuf" {
			t.Errorf("OTLP request carried content type %q, want application/x-protobuf", contentType)
		}
	}
}

func TestFailedCallRecordsTheProblemSlug(t *testing.T) {
	tr := newTracedWithDaemon(t)

	res := call(t, tr.session, "fs_list", map[string]any{"path": "/etc"})
	p := problemOf(t, res)
	if p.Slug() != problem.SlugInvalidPath {
		t.Fatalf("problem is %s, want invalid-path", p.Slug())
	}
	tr.flush(t)

	span, only := tr.daemon.Span("fs_list")
	if !only {
		t.Fatalf("want exactly one fs_list span, got %+v", tr.daemon.Spans())
	}
	if span.Status != "error" {
		t.Errorf("status is %q, want error", span.Status)
	}
	if span.StatusMessage != p.Title {
		t.Errorf("status message is %q, want the problem title %q", span.StatusMessage, p.Title)
	}
	if got := span.Attributes[telemetry.AttrError]; got != problem.SlugInvalidPath {
		t.Errorf("%s is %q, want invalid-path", telemetry.AttrError, got)
	}
	// The path the call was about is on the span even though it was refused.
	if got := span.Attributes[telemetry.AttrPath]; got != "/etc" {
		t.Errorf("%s is %q, want /etc", telemetry.AttrPath, got)
	}
}

// A call the SDK refuses before any handler runs is still one span: the
// middleware wraps the validation, which is what makes the surface untraceable
// nowhere, see PLAN.md section 2.6.
func TestSchemaViolationIsStillTraced(t *testing.T) {
	tr := newTracedWithDaemon(t)

	res := call(t, tr.session, "pkg_build", map[string]any{"path": "/org/ffmpeg", "extra": true})
	if p := problemOf(t, res); p.Slug() != problem.SlugBadRequest {
		t.Fatalf("problem is %s, want bad-request", p.Slug())
	}
	tr.flush(t)

	span, only := tr.daemon.Span("pkg_build")
	if !only {
		t.Fatalf("want exactly one pkg_build span, got %+v", tr.daemon.Spans())
	}
	if span.Status != "error" {
		t.Errorf("status is %q, want error", span.Status)
	}
	if got := span.Attributes[telemetry.AttrError]; got != problem.SlugBadRequest {
		t.Errorf("%s is %q, want bad-request", telemetry.AttrError, got)
	}
}

// An argument is a caller's to choose the size of, and a telemetry record is
// not: a path larger than kitbashd accepts would take the whole batch down
// with it, so it is cut before it reaches the span.
func TestAHugePathIsTruncatedAndTheSpanStillArrives(t *testing.T) {
	tr := newTracedWithDaemon(t)

	huge := "/org/" + strings.Repeat("a", 5<<20)
	res := call(t, tr.session, "fs_list", map[string]any{"path": huge})
	if !res.IsError {
		t.Fatal("a 5 MiB path was accepted as a folder")
	}
	tr.flush(t)

	span, only := tr.daemon.Span("fs_list")
	if !only {
		t.Fatalf("want exactly one fs_list span, got %d spans", len(tr.daemon.Spans()))
	}
	path := span.Attributes[telemetry.AttrPath]
	if path == "" {
		t.Fatal("the span carries no path at all")
	}
	if len(path) > telemetry.AttributeValueLimit {
		t.Errorf("%s is %d bytes, want at most %d", telemetry.AttrPath, len(path), telemetry.AttributeValueLimit)
	}
	if !strings.HasPrefix(path, "/org/aaa") {
		t.Errorf("%s is %q, want the start of the path the caller sent", telemetry.AttrPath, path)
	}
}

// A tel call is about records other calls left, so the filters it names are
// not paths this call touched.
func TestTelQueryDoesNotRecordItsPathFilter(t *testing.T) {
	tr := newTracedWithDaemon(t)

	ok(t, call(t, tr.session, "tel_query", map[string]any{
		"signal": "traces",
		"path":   "/org/ffmpeg",
	}), "tel_query")
	tr.flush(t)

	span, only := tr.daemon.Span("tel_query")
	if !only {
		t.Fatalf("want exactly one tel_query span, got %+v", tr.daemon.Spans())
	}
	if got := span.Attributes[telemetry.AttrPath]; got != "" {
		t.Errorf("%s is %q, want the query's filter not to be recorded as a path this call touched",
			telemetry.AttrPath, got)
	}
}

// A handler that panics is a problem to the agent and an error span to
// Telemetry, not a lost call.
func TestAPanicIsTracedAsAnError(t *testing.T) {
	tr := newTracedWithDaemon(t)
	mcp.AddTool(tr.server, &mcp.Tool{
		Name:        "fs_panic",
		Description: "A tool that fails the way a bug fails.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ json.RawMessage) (*mcp.CallToolResult, any, error) {
		panic("the handler broke")
	})

	res := call(t, tr.session, "fs_panic", map[string]any{})
	p := problemOf(t, res)
	if p.Slug() != problem.SlugInternal {
		t.Fatalf("a panic came back as %s, want internal", p.Slug())
	}
	tr.flush(t)

	span, only := tr.daemon.Span("fs_panic")
	if !only {
		t.Fatalf("want exactly one fs_panic span, got %+v", tr.daemon.Spans())
	}
	if span.Status != "error" {
		t.Errorf("status is %q, want error", span.Status)
	}
	if got := span.Attributes[telemetry.AttrError]; got != problem.SlugInternal {
		t.Errorf("%s is %q, want internal", telemetry.AttrError, got)
	}
}

// A call for a tool that is not on the surface fails inside the SDK rather
// than in a handler, and it is still one span.
func TestAnUnknownToolIsTraced(t *testing.T) {
	tr := newTracedWithDaemon(t)

	if _, err := tr.session.CallTool(context.Background(),
		&mcp.CallToolParams{Name: "fs_teleport", Arguments: map[string]any{}}); err == nil {
		t.Fatal("calling a tool that does not exist succeeded")
	}
	tr.flush(t)

	span, only := tr.daemon.Span("fs_teleport")
	if !only {
		t.Fatalf("want exactly one fs_teleport span, got %+v", tr.daemon.Spans())
	}
	if span.Status != "error" {
		t.Errorf("status is %q, want error", span.Status)
	}
}

func TestProxiedCallCarriesThePackageAndTheProcess(t *testing.T) {
	tr := newTracedWithDaemon(t)
	process := buildAndRun(t, tr)

	ok(t, call(t, tr.session, "ffmpeg_transcode", map[string]any{}), "ffmpeg_transcode")
	tr.flush(t)

	span, only := tr.daemon.Span("ffmpeg_transcode")
	if !only {
		t.Fatalf("want exactly one ffmpeg_transcode span, got %+v", tr.daemon.Spans())
	}
	if got := span.Attributes[telemetry.AttrPackage]; got != tr.folder {
		t.Errorf("%s is %q, want %s", telemetry.AttrPackage, got, tr.folder)
	}
	if got := span.Attributes[telemetry.AttrProcess]; got != process.ID {
		t.Errorf("%s is %q, want the Process id %s", telemetry.AttrProcess, got, process.ID)
	}
	if got := span.Attributes[telemetry.AttrTool]; got != "ffmpeg_transcode" {
		t.Errorf("%s is %q, want ffmpeg_transcode", telemetry.AttrTool, got)
	}
}

func TestBuildOpensAChildSpanAndOneLogRecord(t *testing.T) {
	tr := newTracedWithDaemon(t)
	tr.runner.Log = "STEP 1/2: FROM alpine\nCOMMIT localhost/kitbash/ffmpeg:test\n"

	ok(t, call(t, tr.session, "fs_write", map[string]any{
		"path":    filepath.Join(tr.folder, "kitbash.yaml"),
		"content": packageManifest,
		"message": "Add the ffmpeg Package",
	}), "fs_write")
	ok(t, call(t, tr.session, "fs_write", map[string]any{
		"path":    filepath.Join(tr.folder, "Containerfile"),
		"content": "FROM alpine\n",
		"message": "Add the Containerfile",
	}), "fs_write")
	res := call(t, tr.session, "pkg_build", map[string]any{"path": tr.folder})
	ok(t, res, "pkg_build")
	built := structured[pkg.BuildResult](t, res)
	tr.flush(t)

	tool, only := tr.daemon.Span("pkg_build")
	if !only {
		t.Fatalf("want exactly one pkg_build span, got %+v", tr.daemon.Spans())
	}
	// A query by package has to find the call, not only the child span that
	// happened to learn the Package's name first.
	if got := tool.Attributes[telemetry.AttrPackage]; got != tr.folder {
		t.Errorf("the pkg_build span's %s is %q, want %s", telemetry.AttrPackage, got, tr.folder)
	}
	if got := tool.Attributes[telemetry.AttrPath]; got != tr.folder {
		t.Errorf("the pkg_build span's %s is %q, want %s", telemetry.AttrPath, got, tr.folder)
	}
	child, only := tr.daemon.Span("build")
	if !only {
		t.Fatalf("want exactly one build span, got %+v", tr.daemon.Spans())
	}
	if child.Status != "ok" {
		t.Errorf("the build span's status is %q, want ok", child.Status)
	}
	if got := child.Attributes[telemetry.AttrTool]; got != "pkg_build" {
		t.Errorf("the build span's %s is %q, want pkg_build", telemetry.AttrTool, got)
	}
	if child.ParentSpanID != tool.SpanID {
		t.Errorf("the build span's parent is %s, want the pkg_build span %s", child.ParentSpanID, tool.SpanID)
	}
	if child.TraceID != tool.TraceID {
		t.Errorf("the build span is on trace %s, want %s", child.TraceID, tool.TraceID)
	}
	if got := child.Attributes[telemetry.AttrPackage]; got != tr.folder {
		t.Errorf("%s is %q, want %s", telemetry.AttrPackage, got, tr.folder)
	}
	if got := child.Attributes[telemetry.AttrPath]; got != tr.folder {
		t.Errorf("%s is %q, want %s", telemetry.AttrPath, got, tr.folder)
	}
	if got := child.Attributes[telemetry.AttrDigest]; got != built.Digest {
		t.Errorf("%s is %q, want %s", telemetry.AttrDigest, got, built.Digest)
	}

	records := tr.daemon.Logs()
	if len(records) != 1 {
		t.Fatalf("the build produced %d log records, want exactly one: %+v", len(records), records)
	}
	record := records[0]
	if record.Severity != "INFO" {
		t.Errorf("severity is %q, want INFO", record.Severity)
	}
	if record.Body != built.Log {
		t.Errorf("body is %q, want the build log tail %q", record.Body, built.Log)
	}
	if !strings.Contains(record.Body, "COMMIT") {
		t.Errorf("body is %q, want the tail of the build log", record.Body)
	}
	if record.SpanID != child.SpanID {
		t.Errorf("the record is on span %s, want the build span %s", record.SpanID, child.SpanID)
	}
	if got := record.Attributes[telemetry.AttrPackage]; got != tr.folder {
		t.Errorf("the record's %s is %q, want %s", telemetry.AttrPackage, got, tr.folder)
	}
	// A query for the logs of pkg_build has to find the build log.
	if got := record.Attributes[telemetry.AttrTool]; got != "pkg_build" {
		t.Errorf("the record's %s is %q, want pkg_build", telemetry.AttrTool, got)
	}
	if got := record.Attributes[telemetry.AttrUser]; got != "tester" {
		t.Errorf("the record's %s is %q, want tester", telemetry.AttrUser, got)
	}
}

func TestProcRunOpensAChildSpan(t *testing.T) {
	tr := newTracedWithDaemon(t)
	process := buildAndRun(t, tr)
	tr.flush(t)

	tool, only := tr.daemon.Span("proc_run")
	if !only {
		t.Fatalf("want exactly one proc_run span, got %+v", tr.daemon.Spans())
	}
	// A query by process has to find the call that started it.
	if got := tool.Attributes[telemetry.AttrProcess]; got != process.ID {
		t.Errorf("the proc_run span's %s is %q, want %s", telemetry.AttrProcess, got, process.ID)
	}
	if got := tool.Attributes[telemetry.AttrPackage]; got != tr.folder {
		t.Errorf("the proc_run span's %s is %q, want %s", telemetry.AttrPackage, got, tr.folder)
	}
	child, only := tr.daemon.Span("run")
	if !only {
		t.Fatalf("want exactly one run span, got %+v", tr.daemon.Spans())
	}
	if child.Status != "ok" {
		t.Errorf("the run span's status is %q, want ok", child.Status)
	}
	if got := child.Attributes[telemetry.AttrTool]; got != "proc_run" {
		t.Errorf("the run span's %s is %q, want proc_run", telemetry.AttrTool, got)
	}
	if got := child.Attributes[telemetry.AttrProcess]; got != process.ID {
		t.Errorf("%s is %q, want %s", telemetry.AttrProcess, got, process.ID)
	}
	if got := child.Attributes[telemetry.AttrPackage]; got != tr.folder {
		t.Errorf("%s is %q, want %s", telemetry.AttrPackage, got, tr.folder)
	}
	if got := child.Attributes[telemetry.AttrDigest]; got != process.Digest {
		t.Errorf("%s is %q, want %s", telemetry.AttrDigest, got, process.Digest)
	}
}

// A host without kitbashd serves the whole surface. The session pays one line
// in the server log, and the agent never learns that anything was dropped.
func TestAMissingSocketDoesNotFailTheCall(t *testing.T) {
	tr := newTraced(t, filepath.Join(t.TempDir(), "absent.sock"))

	ok(t, call(t, tr.session, "fs_list", map[string]any{}), "fs_list")
	ok(t, call(t, tr.session, "fs_list", map[string]any{}), "fs_list")
	tr.flush(t)

	lines := strings.Count(strings.TrimSpace(tr.logs.String()), "\n") + 1
	if strings.TrimSpace(tr.logs.String()) == "" {
		t.Fatal("a dropping session logged nothing")
	}
	if lines != 1 {
		t.Errorf("a dropping session logged %d lines, want one:\n%s", lines, tr.logs.String())
	}
}

func TestTelQueryForwardsTheInputAndReturnsTheAnswer(t *testing.T) {
	tr := newTracedWithDaemon(t)
	answer := `{"signal":"logs","records":[{"time":"2026-09-06T10:00:00Z","severity":"INFO","body":"built","attributes":{"user":"tester"}}],"truncated":false}`
	tr.daemon.AnswerQuery(teltest.Response{Status: http.StatusOK, ContentType: "application/json", Body: answer})

	res := call(t, tr.session, "tel_query", map[string]any{"signal": "logs", "limit": 5})
	ok(t, res, "tel_query")

	calls := tr.daemon.Calls()
	if len(calls) != 1 {
		t.Fatalf("the daemon saw %d calls, want one: %+v", len(calls), calls)
	}
	if calls[0].Method != http.MethodPost || calls[0].Path != telemetry.QueryPath {
		t.Errorf("the call was %s %s, want POST %s", calls[0].Method, calls[0].Path, telemetry.QueryPath)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(calls[0].Body), &sent); err != nil {
		t.Fatalf("the daemon was sent %q: %v", calls[0].Body, err)
	}
	if sent["signal"] != "logs" || sent["limit"] != float64(5) {
		t.Errorf("the daemon was sent %v, want the input verbatim", sent)
	}
	// The text block is the daemon's body byte for byte; the structured
	// content is the same document, which the SDK re-encodes on the way out.
	if got := textOf(t, res); got != answer {
		t.Errorf("the text block is %s, want the daemon's body %s", got, answer)
	}
	got, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshaling structured content: %v", err)
	}
	var want, have any
	if err := json.Unmarshal([]byte(answer), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &have); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, have) {
		t.Errorf("structured content is %s, want the daemon's body %s", got, answer)
	}
}

func TestTelQueryReturnsTheDaemonsProblemUnchanged(t *testing.T) {
	tr := newTracedWithDaemon(t)
	tr.daemon.AnswerQuery(teltest.Problem(http.StatusForbidden, problem.SlugNotPermitted,
		"Not permitted", "only an admin may query another member's records",
		"Ask an administrator, or query your own records."))

	res := call(t, tr.session, "tel_query", map[string]any{"signal": "traces", "user": "someone-else"})
	p := problemOf(t, res)
	if p.Slug() != problem.SlugNotPermitted {
		t.Errorf("problem is %s, want not-permitted", p.Slug())
	}
	if p.Status != http.StatusForbidden {
		t.Errorf("status is %d, want 403", p.Status)
	}
	if !strings.Contains(p.Detail, "another member") {
		t.Errorf("detail is %q, want the daemon's own detail", p.Detail)
	}
	if !strings.Contains(p.Fix, "administrator") {
		t.Errorf("fix is %q, want the daemon's own fix", p.Fix)
	}
}

func TestTelRetentionReadsWithGetAndSetsWithPut(t *testing.T) {
	tr := newTracedWithDaemon(t)

	ok(t, call(t, tr.session, "tel_retention", map[string]any{}), "tel_retention")
	res := call(t, tr.session, "tel_retention", map[string]any{"set": map[string]any{"logs": "7d"}})
	ok(t, res, "tel_retention")

	calls := tr.daemon.Calls()
	if len(calls) != 2 {
		t.Fatalf("the daemon saw %d calls, want two: %+v", len(calls), calls)
	}
	if calls[0].Method != http.MethodGet || calls[0].Body != "" {
		t.Errorf("reading was %s with body %q, want GET with no body", calls[0].Method, calls[0].Body)
	}
	if calls[1].Method != http.MethodPut {
		t.Errorf("setting was %s, want PUT", calls[1].Method)
	}
	if calls[1].Path != telemetry.RetentionPath {
		t.Errorf("setting went to %s, want %s", calls[1].Path, telemetry.RetentionPath)
	}
	var sent map[string]string
	if err := json.Unmarshal([]byte(calls[1].Body), &sent); err != nil {
		t.Fatalf("the daemon was sent %q: %v", calls[1].Body, err)
	}
	if sent["logs"] != "7d" {
		t.Errorf("the daemon was sent %v, want the set object", sent)
	}
}

func TestTelToolsAreOnTheSurface(t *testing.T) {
	tr := newTracedWithDaemon(t)

	names := toolNames(t, tr.session)
	for _, want := range []string{"tel_query", "tel_retention"} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is missing from the surface: %v", want, names)
		}
	}
}
