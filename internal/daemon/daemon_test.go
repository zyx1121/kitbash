package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	coltrace "github.com/zyx1121/kitbash/internal/otlpproto/collector/trace/v1"
	commonpb "github.com/zyx1121/kitbash/internal/otlpproto/common/v1"
	tracepb "github.com/zyx1121/kitbash/internal/otlpproto/trace/v1"

	"github.com/zyx1121/kitbash/internal/cgroups"
	"github.com/zyx1121/kitbash/internal/otlp"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
)

// harness is one daemon on a real unix socket, with a client that speaks to
// it. The peer credentials are the test process's own, so the caller is
// whoever runs the tests.
type harness struct {
	t      *testing.T
	client *http.Client
	store  *store.Store
	server *Server
	user   string
	socket string
	// cgroups is the placement this daemon was given, and envDir where it
	// writes the environment file of a Process. Both are the test's own: a
	// unit test writes no cgroup filesystem and nothing under /run.
	cgroups *cgroups.Fake
	envDir  string
}

// serve starts a daemon whose admin answer is fixed, which is how a test gets
// an administrator without creating a group on the host.
func serve(t *testing.T, admin bool) *harness {
	t.Helper()
	return serveWith(t, Options{Admin: func(*user.User) (bool, error) { return admin, nil }})
}

// serveWith starts a daemon with the options a test needs, filling in the ones
// every test shares.
func serveWith(t *testing.T, opts Options) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "kitbashd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	me, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	if opts.Version == "" {
		opts.Version = "v0.3.0-test"
	}
	if opts.Admin == nil {
		opts.Admin = func(*user.User) (bool, error) { return false, nil }
	}
	// No test touches the real cgroup filesystem or /run/kitbash: placement is
	// a fake and the environment files go into the test's own directory.
	if opts.Cgroups == nil {
		opts.Cgroups = &cgroups.Fake{Base: filepath.Join(dir, "cgroup")}
	}
	if opts.EnvDir == "" {
		opts.EnvDir = filepath.Join(dir, "env")
	}
	srv := New(st, opts)
	t.Cleanup(srv.Close)

	socket := filepath.Join(dir, "kitbashd.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after the context was cancelled")
		}
	})

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
		Timeout: 10 * time.Second,
	}
	h := &harness{t: t, client: client, store: st, server: srv, user: me.Username, socket: socket,
		envDir: opts.EnvDir}
	h.cgroups, _ = opts.Cgroups.(*cgroups.Fake)
	return h
}

// do sends one request to the daemon. The host name is ignored: the transport
// dials the socket.
func (h *harness) do(method, path, contentType string, body []byte) (*http.Response, []byte) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://kitbashd"+path, reader)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	got, err := io.ReadAll(res.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	return res, got
}

// postJSON sends one JSON API request.
func (h *harness) postJSON(method, path string, body any) (*http.Response, []byte) {
	h.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		h.t.Fatalf("marshal: %v", err)
	}
	return h.do(method, path, "application/json", encoded)
}

// problemOf reads the problem details of a refused request and checks the
// media type every error of this API carries.
func (h *harness) problemOf(res *http.Response, body []byte) *problem.Problem {
	h.t.Helper()
	if ct := res.Header.Get("Content-Type"); ct != ProblemContentType {
		h.t.Errorf("Content-Type = %q, want %q", ct, ProblemContentType)
	}
	var p problem.Problem
	if err := json.Unmarshal(body, &p); err != nil {
		h.t.Fatalf("problem body %q: %v", body, err)
	}
	if p.Status != res.StatusCode {
		h.t.Errorf("problem status %d, HTTP status %d", p.Status, res.StatusCode)
	}
	return &p
}

// exportRequest is one span with the attributes a producer would send,
// including a kitbash.user the daemon must overwrite.
func exportRequest(name, claimedUser string, start time.Time) []byte {
	req := &coltrace.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: []*tracepb.Span{{
					TraceId:           bytes.Repeat([]byte{0xab}, 16),
					SpanId:            bytes.Repeat([]byte{0xcd}, 8),
					Name:              name,
					StartTimeUnixNano: uint64(start.UnixNano()),
					EndTimeUnixNano:   uint64(start.Add(20 * time.Millisecond).UnixNano()),
					Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
					Attributes: []*commonpb.KeyValue{
						{Key: otlp.AttrUser, Value: &commonpb.AnyValue{
							Value: &commonpb.AnyValue_StringValue{StringValue: claimedUser}}},
						{Key: otlp.AttrPackage, Value: &commonpb.AnyValue{
							Value: &commonpb.AnyValue_StringValue{StringValue: "/org/ffmpeg"}}},
						{Key: otlp.AttrTool, Value: &commonpb.AnyValue{
							Value: &commonpb.AnyValue_StringValue{StringValue: name}}},
					},
				}},
			}},
		}},
	}
	body, err := proto.Marshal(req)
	if err != nil {
		panic(err)
	}
	return body
}

func TestHealth(t *testing.T) {
	h := serve(t, false)
	res, body := h.do(http.MethodGet, "/kitbash/v1/health", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	var got healthResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if got.Version != "v0.3.0-test" {
		t.Errorf("version = %q", got.Version)
	}
	if !strings.HasSuffix(got.Store, "kitbashd.db") {
		t.Errorf("store = %q", got.Store)
	}
	if got.UptimeSeconds < 0 {
		t.Errorf("uptimeSeconds = %d", got.UptimeSeconds)
	}
}

func TestExportStampsThePeer(t *testing.T) {
	h := serve(t, false)
	res, body := h.do(http.MethodPost, "/v1/traces", otlp.ContentTypeProtobuf,
		exportRequest("fs_list", "someone-else", time.Now().Add(-time.Minute)))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	if ct := res.Header.Get("Content-Type"); ct != otlp.ContentTypeProtobuf {
		t.Errorf("Content-Type = %q, want the request's", ct)
	}
	var ack coltrace.ExportTraceServiceResponse
	if err := proto.Unmarshal(body, &ack); err != nil {
		t.Fatalf("response is not an ExportTraceServiceResponse: %v", err)
	}

	page, err := h.store.Query(context.Background(), store.SignalTraces, store.Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(page.Spans))
	}
	if page.Spans[0].User != h.user {
		t.Errorf("user = %q, want the peer %q; the producer's kitbash.user must be replaced",
			page.Spans[0].User, h.user)
	}
	if page.Spans[0].Package != "/org/ffmpeg" {
		t.Errorf("package = %q, want the attribute as sent", page.Spans[0].Package)
	}
}

func TestExportJSON(t *testing.T) {
	h := serve(t, false)
	// The JSON encoding of an empty request is an empty object.
	res, body := h.do(http.MethodPost, "/v1/logs", otlp.ContentTypeJSON, []byte(`{}`))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	if ct := res.Header.Get("Content-Type"); ct != otlp.ContentTypeJSON {
		t.Errorf("Content-Type = %q, want the request's", ct)
	}
	if string(body) != "{}" {
		t.Errorf("body = %q, want an empty response", body)
	}
}

func TestExportRefusals(t *testing.T) {
	h := serve(t, false)

	res, body := h.do(http.MethodPost, "/v1/traces", otlp.ContentTypeJSON, []byte("this is not otlp"))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("garbage body status = %d, want 400", res.StatusCode)
	}
	if slug := h.problemOf(res, body).Slug(); slug != problem.SlugBadRequest {
		t.Errorf("slug = %q, want %q", slug, problem.SlugBadRequest)
	}

	res, body = h.do(http.MethodPost, "/v1/traces", "text/csv", []byte("a,b"))
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown content type status = %d, want 400", res.StatusCode)
	}
	h.problemOf(res, body)

	oversize := bytes.Repeat([]byte{'x'}, otlp.MaxBodyBytes+1)
	res, body = h.do(http.MethodPost, "/v1/traces", otlp.ContentTypeProtobuf, oversize)
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d, want 413", res.StatusCode)
	}
	if slug := h.problemOf(res, body).Slug(); slug != problem.SlugTooLarge {
		t.Errorf("slug = %q, want %q", slug, problem.SlugTooLarge)
	}
	page, err := h.store.Query(context.Background(), store.SignalTraces, store.Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Spans) != 0 {
		t.Errorf("a refused request stored %d spans", len(page.Spans))
	}
}

func TestQueryOwnRecords(t *testing.T) {
	h := serve(t, false)
	start := time.Now().Add(-time.Minute)
	res, body := h.do(http.MethodPost, "/v1/traces", otlp.ContentTypeProtobuf,
		exportRequest("fs_list", "someone-else", start))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d, body %s", res.StatusCode, body)
	}

	res, body = h.postJSON(http.MethodPost, "/kitbash/v1/query", queryRequest{Signal: store.SignalTraces})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("query status = %d, body %s", res.StatusCode, body)
	}
	var got struct {
		Signal    string       `json:"signal"`
		Truncated bool         `json:"truncated"`
		Records   []spanRecord `json:"records"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if got.Signal != store.SignalTraces || got.Truncated {
		t.Errorf("envelope = %+v", got)
	}
	if len(got.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(got.Records))
	}
	record := got.Records[0]
	if record.Name != "fs_list" || record.Status != store.StatusOK {
		t.Errorf("record = %+v", record)
	}
	if record.TraceID != strings.Repeat("ab", 16) || record.SpanID != strings.Repeat("cd", 8) {
		t.Errorf("ids = %q %q", record.TraceID, record.SpanID)
	}
	if record.DurationMs != 20 {
		t.Errorf("durationMs = %v, want 20", record.DurationMs)
	}
	if _, err := time.Parse(time.RFC3339, record.Start); err != nil {
		t.Errorf("start %q is not RFC 3339: %v", record.Start, err)
	}
	if record.Attributes.User != h.user || record.Attributes.Tool != "fs_list" {
		t.Errorf("attributes = %+v", record.Attributes)
	}
	if record.Attributes.Eval != nil {
		t.Errorf("eval = %v, want absent", *record.Attributes.Eval)
	}

	// A member may name themselves, and gets nothing for a filter that misses.
	res, body = h.postJSON(http.MethodPost, "/kitbash/v1/query",
		queryRequest{Signal: store.SignalTraces, User: h.user, Tool: "pkg_build"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("query status = %d, body %s", res.StatusCode, body)
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(got.Records) != 0 {
		t.Errorf("records = %d, want none", len(got.Records))
	}
	if !bytes.Contains(body, []byte(`"records":[]`)) {
		t.Errorf("empty page = %s, want an empty array", body)
	}
}

func TestQueryAnotherUser(t *testing.T) {
	member := serve(t, false)
	res, body := member.postJSON(http.MethodPost, "/kitbash/v1/query",
		queryRequest{Signal: store.SignalTraces, User: "someone-else"})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.StatusCode)
	}
	if slug := member.problemOf(res, body).Slug(); slug != problem.SlugNotPermitted {
		t.Errorf("slug = %q, want %q", slug, problem.SlugNotPermitted)
	}

	admin := serve(t, true)
	res, body = admin.postJSON(http.MethodPost, "/kitbash/v1/query",
		queryRequest{Signal: store.SignalTraces, User: "someone-else"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("admin status = %d, body %s", res.StatusCode, body)
	}
}

func TestQueryForcesTheCaller(t *testing.T) {
	h := serve(t, false)
	// A record of another member, written straight to the store.
	if err := h.store.Insert(context.Background(), store.Export{Spans: []store.Span{{
		TraceID: strings.Repeat("11", 16), SpanID: strings.Repeat("22", 8), Name: "fs_read",
		StartNS: time.Now().Add(-time.Minute).UnixNano(), EndNS: time.Now().UnixNano(),
		Status:     store.StatusOK,
		Attributes: store.Attributes{User: "someone-else"},
	}}}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	res, body := h.postJSON(http.MethodPost, "/kitbash/v1/query", queryRequest{Signal: store.SignalTraces})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	if bytes.Contains(body, []byte("someone-else")) {
		t.Errorf("a member read another member's record: %s", body)
	}
}

func TestQueryBadRequests(t *testing.T) {
	h := serve(t, false)
	cases := []struct {
		name string
		body any
	}{
		{"unknown signal", queryRequest{Signal: "traffic"}},
		{"no signal", queryRequest{}},
		{"limit over the maximum", queryRequest{Signal: store.SignalTraces, Limit: store.MaxLimit + 1}},
		{"since is not a time", queryRequest{Signal: store.SignalTraces, Since: "yesterday"}},
		{"until is not a time", queryRequest{Signal: store.SignalTraces, Until: "soon"}},
		{"unknown field", map[string]any{"signal": "traces", "colour": "red"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, body := h.postJSON(http.MethodPost, "/kitbash/v1/query", tc.body)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body %s", res.StatusCode, body)
			}
			if slug := h.problemOf(res, body).Slug(); slug != problem.SlugBadRequest {
				t.Errorf("slug = %q", slug)
			}
		})
	}
}

func TestRetention(t *testing.T) {
	member := serve(t, false)
	res, body := member.do(http.MethodGet, "/kitbash/v1/retention", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	var got store.Retention
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if got != store.DefaultRetention() {
		t.Errorf("retention = %+v, want the defaults", got)
	}

	value := "7d"
	res, body = member.postJSON(http.MethodPut, "/kitbash/v1/retention", store.RetentionSet{Logs: &value})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("member PUT status = %d, want 403, body %s", res.StatusCode, body)
	}
	if slug := member.problemOf(res, body).Slug(); slug != problem.SlugNotPermitted {
		t.Errorf("slug = %q", slug)
	}

	admin := serve(t, true)
	res, body = admin.postJSON(http.MethodPut, "/kitbash/v1/retention", store.RetentionSet{Logs: &value})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("admin PUT status = %d, body %s", res.StatusCode, body)
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if got.Logs != "7d" || got.Traces != "30d" || got.Metrics != "30d" {
		t.Errorf("retention after set = %+v, want the full retention", got)
	}

	bad := "10m"
	res, body = admin.postJSON(http.MethodPut, "/kitbash/v1/retention", store.RetentionSet{Traces: &bad})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad window status = %d, want 400", res.StatusCode)
	}
	admin.problemOf(res, body)

	res, body = admin.postJSON(http.MethodPut, "/kitbash/v1/retention", store.RetentionSet{})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty set status = %d, want 400, body %s", res.StatusCode, body)
	}
}

func TestUnknownPathAndMethod(t *testing.T) {
	h := serve(t, false)
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/"},
		{http.MethodGet, "/kitbash/v1/nothing"},
		{http.MethodGet, "/v1/traces"},
		{http.MethodPost, "/kitbash/v1/health"},
		{http.MethodDelete, "/kitbash/v1/retention"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			res, body := h.do(tc.method, tc.path, "", nil)
			if res.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404, body %s", res.StatusCode, body)
			}
			if slug := h.problemOf(res, body).Slug(); slug != problem.SlugNotFound {
				t.Errorf("slug = %q, want %q", slug, problem.SlugNotFound)
			}
		})
	}
}

func TestSweepDeletesOldRecords(t *testing.T) {
	h := serve(t, false)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	res, body := h.do(http.MethodPost, "/v1/traces", otlp.ContentTypeProtobuf, exportRequest("fs_list", "", old))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d, body %s", res.StatusCode, body)
	}
	res, body = h.do(http.MethodPost, "/v1/traces", otlp.ContentTypeProtobuf,
		exportRequest("fs_read", "", time.Now().Add(-time.Minute)))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d, body %s", res.StatusCode, body)
	}

	window := "24h"
	if _, err := h.store.SetRetention(ctx, store.RetentionSet{Traces: &window}); err != nil {
		t.Fatalf("SetRetention: %v", err)
	}
	counts, err := h.server.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if counts.Traces != 1 {
		t.Fatalf("swept %d traces, want 1", counts.Traces)
	}
	page, err := h.store.Query(ctx, store.SignalTraces, store.Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Spans) != 1 || page.Spans[0].Name != "fs_read" {
		t.Errorf("spans left = %+v, want only the recent one", page.Spans)
	}
}

func TestSweepLoopStops(t *testing.T) {
	h := serve(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.server.SweepLoop(ctx, 10*time.Millisecond)
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SweepLoop did not stop with its context")
	}
}

func TestPeerIsRequired(t *testing.T) {
	// Served over TCP rather than the unix socket, a request carries no peer
	// credentials and is refused: identity is the kernel's, not a header's.
	h := serve(t, false)
	tcp := httptest.NewServer(h.server.Handler())
	defer tcp.Close()

	res, err := tcp.Client().Get(tcp.URL + "/kitbash/v1/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body %s", res.StatusCode, body)
	}
	if slug := h.problemOf(res, body).Slug(); slug != problem.SlugNotPermitted {
		t.Errorf("slug = %q, want %q", slug, problem.SlugNotPermitted)
	}
}
