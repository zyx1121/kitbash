package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	coltrace "github.com/zyx1121/kitbash/internal/otlpproto/collector/trace/v1"
	commonpb "github.com/zyx1121/kitbash/internal/otlpproto/common/v1"
	tracepb "github.com/zyx1121/kitbash/internal/otlpproto/trace/v1"

	"github.com/zyx1121/kitbash/internal/otlp"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// serveTCP starts the Process receiver of this daemon on a loopback port and
// returns its base URL. It is the same server the socket tests use, so a test
// exercises both listeners of one daemon.
func (h *harness) serveTCP() string {
	h.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.server.ServeTCP(ctx, ln) }()
	h.t.Cleanup(func() {
		// The callers of this listener are done, so they hang up before the
		// daemon stops: a keep alive connection left open is one shutdown
		// waits out, and on a loaded host that wait is what expires.
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		cancel()
		err, returned := recvWithin(done)
		switch {
		case !returned:
			h.t.Errorf("waited %s for ServeTCP to return after the context was cancelled", waitBudget)
		case err != nil:
			h.t.Errorf("ServeTCP: %v", err)
		}
	})
	return "http://" + ln.Addr().String()
}

// exportTCP sends one export to the Process receiver with the token a Process
// holds. An empty token sends no Authorization header at all.
func (h *harness) exportTCP(base, path, token string, body []byte) (*http.Response, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", otlp.ContentTypeProtobuf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := (&http.Client{Timeout: waitBudget}).Do(req)
	if err != nil {
		h.t.Fatalf("POST %s: %v", path, err)
	}
	defer res.Body.Close()
	got, err := io.ReadAll(res.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	return res, got
}

// register registers one Process over the socket and returns its token.
func (h *harness) register(req processRequest) (string, *http.Response, []byte) {
	h.t.Helper()
	res, body := h.postJSON(http.MethodPost, processesPath, req)
	if res.StatusCode != http.StatusOK {
		return "", res, body
	}
	var got processResponse
	if err := json.Unmarshal(body, &got); err != nil {
		h.t.Fatalf("body %q: %v", body, err)
	}
	if got.ID != req.ID {
		h.t.Errorf("id = %q, want %q", got.ID, req.ID)
	}
	return got.Token, res, body
}

// registration is one valid processes_register input, with an endpoint only
// when the Process is a subscriber.
func registration(endpoint string) processRequest {
	req := processRequest{
		ID:      uuid.V7(),
		Package: "/org/sensorium",
		Name:    "sensorium",
		Expose:  ExposeNone,
	}
	if endpoint != "" {
		req.Expose = ExposeHTTP
		req.Endpoint = endpoint
		req.Subscriptions = []string{store.SubscriptionTelemetry}
	}
	return req
}

// processExport is one span as a Process would send it, claiming a user, a
// package and a process kitbashd must replace unless the eval rules apply.
func processExport(name string, claims map[string]any, eval bool, start time.Time) []byte {
	attrs := []*commonpb.KeyValue{{
		Key:   otlp.AttrTool,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: name}},
	}}
	for key, value := range claims {
		switch v := value.(type) {
		case string:
			attrs = append(attrs, &commonpb.KeyValue{
				Key:   key,
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}},
			})
		case bool:
			attrs = append(attrs, &commonpb.KeyValue{
				Key:   key,
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v}},
			})
		}
	}
	if eval {
		attrs = append(attrs, &commonpb.KeyValue{
			Key:   otlp.AttrEval,
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}},
		})
	}
	req := &coltrace.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: []*tracepb.Span{{
					TraceId:           bytes.Repeat([]byte{0x1a}, 16),
					SpanId:            bytes.Repeat([]byte{0x2b}, 8),
					Name:              name,
					StartTimeUnixNano: uint64(start.UnixNano()),
					EndTimeUnixNano:   uint64(start.Add(time.Millisecond).UnixNano()),
					Attributes:        attrs,
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

// spans reads what the store holds, newest first.
func (h *harness) spans() []store.Span {
	h.t.Helper()
	page, err := h.store.Query(context.Background(), store.SignalTraces, store.Filter{})
	if err != nil {
		h.t.Fatalf("Query: %v", err)
	}
	return page.Spans
}

// TestRegisterMintsATokenTheStoreOnlyHashes is the must-do of the Process
// receiver: the token is returned once and the store holds no copy of it.
func TestRegisterMintsATokenTheStoreOnlyHashes(t *testing.T) {
	h := serve(t, false)
	req := registration("")
	token, res, body := h.register(req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	if len(token) < 40 {
		t.Fatalf("token = %q, want at least 32 bytes of entropy", token)
	}

	p, found, err := h.store.ProcessByToken(context.Background(), token)
	if err != nil || !found {
		t.Fatalf("ProcessByToken: %v, found %v", err, found)
	}
	if p.Owner != h.user || p.Admin {
		t.Errorf("process = %+v, want the peer as a member owner", p)
	}
	if _, found, _ := h.store.ProcessByToken(context.Background(), store.HashToken(token)); found {
		t.Error("the stored hash works as a token; the store must hold nothing replayable")
	}

	// The list carries the record and no token.
	res, body = h.do(http.MethodGet, processesPath, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, body %s", res.StatusCode, body)
	}
	if strings.Contains(string(body), token) {
		t.Errorf("the list carries the token: %s", body)
	}
	var list processList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(list.Processes) != 1 || list.Processes[0].ID != req.ID {
		t.Fatalf("list = %+v", list.Processes)
	}
}

// TestRegisterAgainRevokesTheOldToken is proc_run for a Process that was run
// before: the same id keeps one record, and only the newest token exports.
func TestRegisterAgainRevokesTheOldToken(t *testing.T) {
	h := serve(t, false)
	base := h.serveTCP()
	req := registration("")

	first, _, _ := h.register(req)
	second, _, _ := h.register(req)
	if first == second {
		t.Fatal("the second registration returned the same token")
	}

	export := processExport("echo", nil, false, time.Now().Add(-time.Minute))
	res, body := h.exportTCP(base, pathTraces, first, export)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the revoked token exported %d, want 401", res.StatusCode)
	}
	if slug := h.problemOf(res, body).Slug(); slug != problem.SlugNotPermitted {
		t.Errorf("slug = %q, want %q", slug, problem.SlugNotPermitted)
	}
	res, body = h.exportTCP(base, pathTraces, second, export)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the new token exported %d, body %s", res.StatusCode, body)
	}
}

// TestRegisterAnotherOwnersIDConflicts keeps one member from taking over
// another member's Process by naming its id.
func TestRegisterAnotherOwnersIDConflicts(t *testing.T) {
	h := serve(t, false)
	req := registration("")
	_, hash, err := store.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if err := h.store.RegisterProcess(context.Background(), store.Process{
		ID: req.ID, Owner: "someone-else", Package: "/home/someone-else/echo",
		Expose: ExposeNone, RegisteredAt: time.Now(),
	}, hash, 0); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}

	_, res, body := h.register(req)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res.StatusCode)
	}
	if slug := h.problemOf(res, body).Slug(); slug != problem.SlugConflict {
		t.Errorf("slug = %q, want %q", slug, problem.SlugConflict)
	}
}

// TestRegistrationsAreCappedPerMember bounds what one member costs the daemon:
// every registration is a live token and, when it subscribes, a queue.
func TestRegistrationsAreCappedPerMember(t *testing.T) {
	h := serve(t, false)
	var last processRequest
	for i := range MaxProcessesPerMember {
		last = registration("")
		if _, res, body := h.register(last); res.StatusCode != http.StatusOK {
			t.Fatalf("registration %d = %d, body %s", i, res.StatusCode, body)
		}
	}

	_, res, body := h.register(registration(""))
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("the registration past the cap = %d, want 409", res.StatusCode)
	}
	p := h.problemOf(res, body)
	if p.Slug() != problem.SlugConflict {
		t.Errorf("slug = %q, want %q", p.Slug(), problem.SlugConflict)
	}
	if !strings.Contains(p.Fix, "Stop a Process") {
		t.Errorf("fix = %q, want it to say what to do about the cap", p.Fix)
	}

	// Re-running a Process the member already holds is a replacement, not one
	// more, so the cap never stops a restart.
	if _, res, body := h.register(last); res.StatusCode != http.StatusOK {
		t.Fatalf("replacing at the cap = %d, body %s", res.StatusCode, body)
	}
}

// TestUnregisterRevokesTheToken is proc_stop: the container may still be
// exporting, and it is refused from the moment the Process is gone.
func TestUnregisterRevokesTheToken(t *testing.T) {
	h := serve(t, false)
	base := h.serveTCP()
	req := registration("")
	token, _, _ := h.register(req)

	res, body := h.do(http.MethodDelete, processesPath+"/"+req.ID, "", nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, body %s", res.StatusCode, body)
	}
	res, _ = h.exportTCP(base, pathTraces, token, processExport("echo", nil, false, time.Now()))
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("the revoked token exported %d, want 401", res.StatusCode)
	}

	res, body = h.do(http.MethodDelete, processesPath+"/"+req.ID, "", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("deleting again = %d, want 404", res.StatusCode)
	}
	if slug := h.problemOf(res, body).Slug(); slug != problem.SlugNotFound {
		t.Errorf("slug = %q, want %q", slug, problem.SlugNotFound)
	}
}

// TestUnregisterAnotherOwnersProcessIsRefused is the member boundary on the
// delete path.
func TestUnregisterAnotherOwnersProcessIsRefused(t *testing.T) {
	h := serve(t, false)
	id := uuid.V7()
	_, hash, _ := store.NewToken()
	if err := h.store.RegisterProcess(context.Background(), store.Process{
		ID: id, Owner: "someone-else", Package: "/home/someone-else/echo",
		Expose: ExposeNone, RegisteredAt: time.Now(),
	}, hash, 0); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}
	res, body := h.do(http.MethodDelete, processesPath+"/"+id, "", nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.StatusCode)
	}
	if slug := h.problemOf(res, body).Slug(); slug != problem.SlugNotPermitted {
		t.Errorf("slug = %q, want %q", slug, problem.SlugNotPermitted)
	}
}

// TestProcessExportIsStampedFromTheToken is the must-do of the TCP receiver: a
// Process is whoever its token says, and what it claims about itself is
// replaced.
func TestProcessExportIsStampedFromTheToken(t *testing.T) {
	h := serve(t, false)
	base := h.serveTCP()
	req := registration("")
	token, _, _ := h.register(req)

	claims := map[string]any{
		otlp.AttrUser:    "someone-else",
		otlp.AttrPackage: "/org/not-this-one",
		otlp.AttrProcess: "not-this-process",
	}
	res, body := h.exportTCP(base, pathTraces, token, processExport("echo", claims, false, time.Now().Add(-time.Minute)))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}

	spans := h.spans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	got := spans[0]
	if got.User != h.user {
		t.Errorf("user = %q, want the owner %q", got.User, h.user)
	}
	if got.Package != req.Package || got.Process != req.ID {
		t.Errorf("package = %q, process = %q, want the token's", got.Package, got.Process)
	}
	if got.Producer != req.ID {
		t.Errorf("producer = %q, want the Process id %q", got.Producer, req.ID)
	}
}

// TestTCPRefusesWithoutAToken is the other half: no token, no records, and the
// JSON API is not served on this listener at all.
func TestTCPRefusesWithoutAToken(t *testing.T) {
	h := serve(t, false)
	base := h.serveTCP()
	export := processExport("echo", nil, false, time.Now())

	for _, token := range []string{"", "not-a-token"} {
		res, body := h.exportTCP(base, pathTraces, token, export)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q exported %d, want 401", token, res.StatusCode)
		}
		p := h.problemOf(res, body)
		if p.Slug() != problem.SlugNotPermitted {
			t.Errorf("slug = %q, want %q", p.Slug(), problem.SlugNotPermitted)
		}
		if got := res.Header.Get("WWW-Authenticate"); got != BearerChallenge {
			t.Errorf("WWW-Authenticate = %q, want %q; RFC 9110 requires a challenge with a 401",
				got, BearerChallenge)
		}
	}
	if spans := h.spans(); len(spans) != 0 {
		t.Errorf("a refused export stored %d spans", len(spans))
	}

	res, _ := h.exportTCP(base, healthPath, "", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("the JSON API answered %d over TCP, want 404", res.StatusCode)
	}
}

// TestEvalClaimsOnlyFromAnAdminsProcess is the evaluation rule of PLAN.md
// section 2.4: a judgment written by an admin's kit keeps the subject's
// identity, and everybody else's is stamped like any other record.
func TestEvalClaimsOnlyFromAnAdminsProcess(t *testing.T) {
	claims := map[string]any{
		otlp.AttrUser:              "judged-member",
		otlp.AttrPackage:           "/org/ffmpeg",
		otlp.AttrProcess:           "0192f2c0-1234-7abc-8def-0123456789ab",
		otlp.AttrPath:              "/org/ffmpeg",
		otlp.AttrSubjectTraceID:    strings.Repeat("ab", 16),
		otlp.AttrSubjectSpanID:     strings.Repeat("cd", 8),
		"kitbash.eval.score":       "0.9",
		otlp.AttrProducer + ".lie": "ignored",
	}

	t.Run("admin", func(t *testing.T) {
		h := serve(t, true)
		base := h.serveTCP()
		req := registration("")
		token, _, _ := h.register(req)
		res, body := h.exportTCP(base, pathTraces, token,
			processExport("judgement", claims, true, time.Now().Add(-time.Minute)))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body %s", res.StatusCode, body)
		}
		got := h.spans()[0]
		if got.User != "judged-member" || got.Package != "/org/ffmpeg" {
			t.Errorf("an admin's judgement was stamped over: %+v", got.Attributes)
		}
		if got.Process != "0192f2c0-1234-7abc-8def-0123456789ab" || got.Path != "/org/ffmpeg" {
			t.Errorf("process = %q, path = %q, want the claims as sent", got.Process, got.Path)
		}
		if got.Producer != req.ID {
			t.Errorf("producer = %q, want the kit's Process id", got.Producer)
		}
		if s := subjectOf(got.Other); s == nil || s.TraceID != strings.Repeat("ab", 16) {
			t.Errorf("subject = %+v, want the judged span", s)
		}

		// The query surface publishes the subject as an object.
		res, body = h.postJSON(http.MethodPost, queryPath, queryRequest{Signal: store.SignalTraces})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("query status = %d, body %s", res.StatusCode, body)
		}
		var page struct {
			Records []spanRecord `json:"records"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatalf("body %q: %v", body, err)
		}
		if len(page.Records) != 1 {
			t.Fatalf("records = %d", len(page.Records))
		}
		attrs := page.Records[0].Attributes
		if attrs.Subject == nil || attrs.Subject.SpanID != strings.Repeat("cd", 8) {
			t.Errorf("attributes.subject = %+v", attrs.Subject)
		}
		if attrs.Producer != req.ID || attrs.Eval == nil || !*attrs.Eval {
			t.Errorf("attributes = %+v", attrs)
		}
	})

	t.Run("member", func(t *testing.T) {
		h := serve(t, false)
		base := h.serveTCP()
		req := registration("")
		token, _, _ := h.register(req)
		res, body := h.exportTCP(base, pathTraces, token,
			processExport("judgement", claims, true, time.Now().Add(-time.Minute)))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body %s", res.StatusCode, body)
		}
		got := h.spans()[0]
		if got.User != h.user {
			t.Errorf("user = %q, want the member %q; a member's kit may not claim another", got.User, h.user)
		}
		if got.Package != req.Package || got.Process != req.ID {
			t.Errorf("package = %q, process = %q, want the token's", got.Package, got.Process)
		}
	})

	t.Run("session", func(t *testing.T) {
		h := serve(t, true)
		res, body := h.do(http.MethodPost, pathTraces, otlp.ContentTypeProtobuf,
			processExport("judgement", claims, true, time.Now().Add(-time.Minute)))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body %s", res.StatusCode, body)
		}
		got := h.spans()[0]
		if got.User != h.user {
			t.Errorf("user = %q, want the peer %q; a session is stamped whatever it claims", got.User, h.user)
		}
		if got.Producer != h.user {
			t.Errorf("producer = %q, want the member name", got.Producer)
		}
	})
}

// TestQueryFiltersByProducer is the new filter of tel_query: it separates a
// judgment from the act it judges, which share every other attribute.
func TestQueryFiltersByProducer(t *testing.T) {
	h := serve(t, false)
	base := h.serveTCP()
	req := registration("")
	token, _, _ := h.register(req)

	if res, body := h.do(http.MethodPost, pathTraces, otlp.ContentTypeProtobuf,
		exportRequest("fs_list", h.user, time.Now().Add(-time.Minute))); res.StatusCode != http.StatusOK {
		t.Fatalf("session export = %d, body %s", res.StatusCode, body)
	}
	if res, body := h.exportTCP(base, pathTraces, token,
		processExport("echo", nil, false, time.Now().Add(-time.Minute))); res.StatusCode != http.StatusOK {
		t.Fatalf("process export = %d, body %s", res.StatusCode, body)
	}

	for producer, want := range map[string]string{h.user: "fs_list", req.ID: "echo"} {
		res, body := h.postJSON(http.MethodPost, queryPath,
			queryRequest{Signal: store.SignalTraces, Producer: producer})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("query status = %d, body %s", res.StatusCode, body)
		}
		var page struct {
			Records []spanRecord `json:"records"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatalf("body %q: %v", body, err)
		}
		if len(page.Records) != 1 || page.Records[0].Name != want {
			t.Fatalf("producer %q returned %+v, want the %s span", producer, page.Records, want)
		}
		if page.Records[0].Attributes.Producer != producer {
			t.Errorf("producer = %q, want %q", page.Records[0].Attributes.Producer, producer)
		}
	}
}

// TestEndpointMustBeLoopback is the rule that keeps the fan out from being
// pointed at the network by a registration.
func TestEndpointMustBeLoopback(t *testing.T) {
	h := serve(t, false)
	for _, endpoint := range []string{
		"http://10.10.10.116:4318",
		"http://example.org:80",
		"https://127.0.0.1:4318",
		"http://127.0.0.1:4318/collect",
		"http://localhost:4318",
		"http://127.0.0.1",
		"http://[::1]:4318",
		"tcp://127.0.0.1:4318",
	} {
		req := registration("http://127.0.0.1:40275")
		req.Endpoint = endpoint
		_, res, body := h.register(req)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("endpoint %q registered %d, want 400", endpoint, res.StatusCode)
			continue
		}
		if slug := h.problemOf(res, body).Slug(); slug != problem.SlugBadRequest {
			t.Errorf("endpoint %q slug = %q", endpoint, slug)
		}
	}

	// The rest of the input is checked the same way.
	for _, req := range []processRequest{
		{ID: "not-a-uuid", Package: "/org/x", Expose: ExposeNone},
		{ID: uuid.V7(), Package: "relative/path", Expose: ExposeNone},
		{ID: uuid.V7(), Package: "/org/x", Expose: "sideways"},
		{ID: uuid.V7(), Package: "/org/x", Expose: ExposeNone, Subscriptions: []string{"everything"}},
	} {
		_, res, _ := h.register(req)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%+v registered %d, want 400", req, res.StatusCode)
		}
	}
}

// subscriberStub is a Process that subscribes to the fan out: it records what
// it receives and can be made slow.
type subscriberStub struct {
	server   *httptest.Server
	requests chan fanoutRequest
	release  chan struct{}
	slow     bool
}

// fanoutRequest is one delivery a subscriber received.
type fanoutRequest struct {
	path string
	// auth is the Authorization header of the delivery, which is how a
	// subscriber tells kitbashd from anything else on the host, see
	// fan_out.authentication in spec/kitbashd-api.yaml.
	auth string
	body map[string]any
}

func newSubscriberStub(t *testing.T, slow bool) *subscriberStub {
	t.Helper()
	sub := &subscriberStub{
		requests: make(chan fanoutRequest, 64),
		release:  make(chan struct{}),
		slow:     slow,
	}
	sub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		json.Unmarshal(body, &decoded)
		select {
		case sub.requests <- fanoutRequest{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: decoded}:
		default:
		}
		if sub.slow {
			<-sub.release
		}
		w.Header().Set("Content-Type", otlp.ContentTypeJSON)
		w.Write([]byte("{}"))
	}))
	t.Cleanup(func() {
		close(sub.release)
		sub.server.Close()
	})
	return sub
}

// await reads the next delivery, or fails the test if none arrives.
func (s *subscriberStub) await(t *testing.T) fanoutRequest {
	t.Helper()
	return waitRecv(t, "the subscriber to receive a delivery", s.requests)
}

// silent fails the test if anything arrives within the window.
func (s *subscriberStub) silent(t *testing.T, window time.Duration) {
	t.Helper()
	select {
	case req := <-s.requests:
		t.Fatalf("the subscriber received %s, which it is not eligible for", req.path)
	case <-time.After(window):
	}
}

// addSubscriber registers a subscriber owned by whoever the test names, which
// is how one daemon gets records of two members' Processes. It answers the
// Process id and the fan out secret its deliveries carry.
func (h *harness) addSubscriber(owner string, admin bool, endpoint string) (string, string) {
	h.t.Helper()
	secret, err := store.NewFanoutSecret()
	if err != nil {
		h.t.Fatalf("NewFanoutSecret: %v", err)
	}
	return h.addSubscriberWithSecret(owner, admin, endpoint, secret), secret
}

// addSubscriberWithSecret is the same with the secret given, so a test can
// register the row a store written before the fan out was authenticated holds:
// one with no secret at all.
func (h *harness) addSubscriberWithSecret(owner string, admin bool, endpoint, secret string) string {
	h.t.Helper()
	id := uuid.V7()
	_, hash, err := store.NewToken()
	if err != nil {
		h.t.Fatalf("NewToken: %v", err)
	}
	if err := h.store.RegisterProcess(context.Background(), store.Process{
		ID:            id,
		Owner:         owner,
		Admin:         admin,
		Package:       "/org/sensorium",
		Expose:        ExposeHTTP,
		Endpoint:      endpoint,
		Subscriptions: []string{store.SubscriptionTelemetry},
		FanoutSecret:  secret,
		RegisteredAt:  time.Now(),
	}, hash, 0); err != nil {
		h.t.Fatalf("RegisterProcess: %v", err)
	}
	if err := h.server.LoadSubscribers(context.Background()); err != nil {
		h.t.Fatalf("LoadSubscribers: %v", err)
	}
	return id
}

// TestFanOutFollowsTheReadingRule is the fan out of PLAN.md section 2.4: an
// admin's subscriber receives every member's records, a member's receives only
// their own, and the records arrive as OTLP JSON on the standard path.
func TestFanOutFollowsTheReadingRule(t *testing.T) {
	h := serve(t, false)
	adminSub := newSubscriberStub(t, false)
	memberSub := newSubscriberStub(t, false)
	otherSub := newSubscriberStub(t, false)

	_, adminSecret := h.addSubscriber("an-admin", true, adminSub.server.URL)
	_, memberSecret := h.addSubscriber(h.user, false, memberSub.server.URL)
	h.addSubscriber("someone-else", false, otherSub.server.URL)
	secrets := map[string]string{"admin": adminSecret, "owner": memberSecret}

	res, body := h.do(http.MethodPost, pathTraces, otlp.ContentTypeProtobuf,
		exportRequest("fs_list", h.user, time.Now().Add(-time.Minute)))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d, body %s", res.StatusCode, body)
	}

	for name, sub := range map[string]*subscriberStub{"admin": adminSub, "owner": memberSub} {
		got := sub.await(t)
		if got.path != pathTraces {
			t.Errorf("%s subscriber received %s, want %s", name, got.path, pathTraces)
		}
		// Each subscriber's own secret, so one kit's deliveries are not
		// something another kit could have replayed.
		if want := "Bearer " + secrets[name]; got.auth != want {
			t.Errorf("%s subscriber received %q, want %q", name, got.auth, want)
		}
		span := onlySpan(t, got.body)
		if span["traceId"] != strings.Repeat("ab", 16) {
			t.Errorf("%s subscriber received traceId %v, want hex", name, span["traceId"])
		}
		if span["startTimeUnixNano"] == nil {
			t.Errorf("%s subscriber received no start time: %v", name, span)
		}
		attrs := attributeMap(span)
		if attrs[otlp.AttrUser] != h.user || attrs[otlp.AttrProducer] != h.user {
			t.Errorf("%s subscriber received attributes %v, want the stamped ones", name, attrs)
		}
	}
	otherSub.silent(t, 300*time.Millisecond)
}

// TestFanOutStopsAtRecordsFromSubscribers is the loop guard. A record a
// subscriber wrote is stored and queryable, and pushed to nobody: not back to
// its own producer, and not to a second kit, whose own records would otherwise
// wake the first one again for as long as both run.
func TestFanOutStopsAtRecordsFromSubscribers(t *testing.T) {
	h := serve(t, true)
	base := h.serveTCP()
	itself := newSubscriberStub(t, false)
	other := newSubscriberStub(t, false)

	req := registration(itself.server.URL)
	token, res, body := h.register(req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	h.addSubscriber(h.user, true, other.server.URL)

	res, body = h.exportTCP(base, pathTraces, token,
		processExport("judgement", nil, true, time.Now().Add(-time.Minute)))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d, body %s", res.StatusCode, body)
	}

	itself.silent(t, 300*time.Millisecond)
	other.silent(t, 300*time.Millisecond)

	// The store still has it: the push is what stops, not the record.
	spans := h.spans()
	if len(spans) != 1 || spans[0].Producer != req.ID {
		t.Fatalf("the store holds %+v, want the kit's record", spans)
	}

	// A record from a member's session still fans out, so the guard is about
	// who produced the record and not about the subscribers being asleep.
	res, body = h.do(http.MethodPost, pathTraces, otlp.ContentTypeProtobuf,
		exportRequest("fs_list", h.user, time.Now().Add(-time.Minute)))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("session export = %d, body %s", res.StatusCode, body)
	}
	if got := other.await(t); got.path != pathTraces {
		t.Errorf("the second subscriber received %s", got.path)
	}
}

// TestFanOutDoesNotFollowRedirects is the network boundary: a member's Process
// answers 307 for another host, and the daemon delivers there. It must not.
// kitbashd runs as root, so a subscriber deciding where records go next would
// hand one member's Telemetry to whatever address another member names.
func TestFanOutDoesNotFollowRedirects(t *testing.T) {
	h := serve(t, false)
	elsewhere := newSubscriberStub(t, false)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.server.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	h.addSubscriber(h.user, false, redirector.URL)
	res, body := h.do(http.MethodPost, pathTraces, otlp.ContentTypeProtobuf,
		exportRequest("fs_list", h.user, time.Now().Add(-time.Minute)))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d, body %s", res.StatusCode, body)
	}
	elsewhere.silent(t, time.Second)
}

// TestFanOutRefusesToLeaveLoopback is the same boundary one layer down: the
// endpoint check at registration is not the only thing standing between a
// store row and the network.
func TestFanOutRefusesToLeaveLoopback(t *testing.T) {
	for _, address := range []string{"10.10.10.116:4318", "example.org:80"} {
		if _, err := dialLoopback(context.Background(), "tcp", address); err == nil {
			t.Errorf("the fan out dialled %s", address)
		}
	}
	// The one address it does dial is refused only because nothing listens.
	if _, err := dialLoopback(context.Background(), "udp", "127.0.0.1:9"); err == nil {
		t.Error("the fan out dialled a datagram socket")
	}
}

// TestQueueIsBoundedInBytes is the second bound on a subscriber that never
// answers: a thousand requests of four megabytes each would be gigabytes of
// the daemon's memory held for one Process that stopped reading.
func TestQueueIsBoundedInBytes(t *testing.T) {
	sub := &subscriber{
		id:    "a-process",
		queue: make(chan delivery, QueueDepth),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	body := bytes.Repeat([]byte{'x'}, 1<<20)
	// Nothing drains this queue: the subscriber never answers.
	for range 200 {
		sub.enqueue(delivery{path: pathTraces, body: body})
	}
	if held := sub.held.Load(); held > QueueBytes {
		t.Errorf("the queue holds %d bytes, over the budget of %d", held, QueueBytes)
	}
	if held := sub.held.Load(); held < QueueBytes-int64(len(body)) {
		t.Errorf("the queue holds %d bytes, want it filled to about %d", held, QueueBytes)
	}
	if dropped := sub.dropped.Load(); dropped == 0 {
		t.Error("nothing was dropped, so the budget did not bind")
	}
	if len(sub.queue) != int(sub.held.Load()/int64(len(body))) {
		t.Errorf("queue holds %d requests and %d bytes, which do not agree",
			len(sub.queue), sub.held.Load())
	}

	// What the worker takes off the queue is given back to the budget.
	before := sub.held.Load()
	d := <-sub.queue
	sub.release(d)
	if got := sub.held.Load(); got != before-int64(len(body)) {
		t.Errorf("held = %d after one delivery left the queue, want %d", got, before-int64(len(body)))
	}
}

// TestFanOutNeverDelaysTheProducer is the promise the queue exists for: a
// subscriber that does not answer cannot hold up the 200 that says the records
// are stored.
func TestFanOutNeverDelaysTheProducer(t *testing.T) {
	h := serve(t, false)
	slow := newSubscriberStub(t, true)
	h.addSubscriber(h.user, false, slow.server.URL)

	start := time.Now()
	for range 3 {
		res, body := h.do(http.MethodPost, pathTraces, otlp.ContentTypeProtobuf,
			exportRequest("fs_list", h.user, time.Now().Add(-time.Minute)))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("export status = %d, body %s", res.StatusCode, body)
		}
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("three exports took %s while a subscriber hung; the fan out must not block them", elapsed)
	}
	slow.await(t)
}

// TestQueueDropsTheOldest is what a full queue does: it loses the oldest
// request rather than the producer's time, and it counts what it lost.
func TestQueueDropsTheOldest(t *testing.T) {
	sub := &subscriber{
		id:    "a-process",
		queue: make(chan delivery, QueueDepth),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	// Nothing drains the queue, so everything past its depth must be dropped
	// without blocking.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range QueueDepth * 2 {
			sub.enqueue(delivery{path: pathTraces, body: []byte(strings.Repeat("x", i%8))})
		}
	}()
	if _, finished := recvWithin(done); !finished {
		t.Fatalf("waited %s for enqueue to fill the queue: it blocked on a full one", waitBudget)
	}
	if len(sub.queue) != QueueDepth {
		t.Errorf("queue holds %d, want it full at %d", len(sub.queue), QueueDepth)
	}
	if dropped := sub.dropped.Load(); dropped != QueueDepth {
		t.Errorf("dropped = %d, want the %d that did not fit", dropped, QueueDepth)
	}
}

// TestHealthReportsListenersAndSubscribers is the operator's answer to whether
// the Process receiver is up and whether anything is subscribed.
func TestHealthReportsListenersAndSubscribers(t *testing.T) {
	h := serve(t, false)
	base := h.serveTCP()
	sub := newSubscriberStub(t, false)
	h.addSubscriber(h.user, false, sub.server.URL)

	res, body := h.do(http.MethodGet, healthPath, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	var got healthResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if !strings.HasSuffix(got.Listeners.Socket, "kitbashd.sock") {
		t.Errorf("listeners.socket = %q", got.Listeners.Socket)
	}
	if got.Listeners.TCP != strings.TrimPrefix(base, "http://") {
		t.Errorf("listeners.tcp = %q, want %q", got.Listeners.TCP, base)
	}
	if got.Subscribers != 1 {
		t.Errorf("subscribers = %d, want 1", got.Subscribers)
	}
}

// onlySpan reads the one span of a fanned out export request.
func onlySpan(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	resource, ok := body["resourceSpans"].([]any)
	if !ok || len(resource) != 1 {
		t.Fatalf("body = %v, want one resourceSpans", body)
	}
	scope, ok := resource[0].(map[string]any)["scopeSpans"].([]any)
	if !ok || len(scope) != 1 {
		t.Fatalf("body = %v, want one scopeSpans", body)
	}
	spans, ok := scope[0].(map[string]any)["spans"].([]any)
	if !ok || len(spans) != 1 {
		t.Fatalf("body = %v, want one span", body)
	}
	return spans[0].(map[string]any)
}

// attributeMap flattens the OTLP KeyValue list of a record.
func attributeMap(span map[string]any) map[string]string {
	out := map[string]string{}
	list, _ := span["attributes"].([]any)
	for _, item := range list {
		kv, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key, _ := kv["key"].(string)
		value, _ := kv["value"].(map[string]any)
		if s, ok := value["stringValue"].(string); ok {
			out[key] = s
		}
		if b, ok := value["boolValue"].(bool); ok && b {
			out[key] = "true"
		}
	}
	return out
}

// daemonLog captures what the package logger writes for one test. The fan out
// logs from its own goroutine, so the buffer is guarded.
type daemonLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *daemonLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *daemonLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// captureDaemonLog points the daemon log at a buffer for the length of one
// test, which is how a test reads a line the operator would read.
func captureDaemonLog(t *testing.T) *daemonLog {
	t.Helper()
	captured := &daemonLog{}
	logger.SetOutput(captured)
	t.Cleanup(func() { logger.SetOutput(os.Stderr) })
	return captured
}

// TestRegistrationAnswersAFanOutSecretTheListHides is the mint of
// fan_out.authentication: a registration returns the secret its container is
// started with, a re-registration replaces it, and no listing carries it.
func TestRegistrationAnswersAFanOutSecretTheListHides(t *testing.T) {
	h := serve(t, false)
	req := registration("")

	res, body := h.postJSON(http.MethodPost, processesPath, req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	var first processResponse
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(first.FanoutSecret) < 40 {
		t.Fatalf("fanoutSecret = %q, want at least 32 bytes of entropy", first.FanoutSecret)
	}
	if first.FanoutSecret == first.Token {
		t.Error("the secret is the token; they are two credentials, not one")
	}

	// Unlike the token the store holds it, because kitbashd is the one that
	// sends it on every delivery.
	p, found, err := h.store.Process(context.Background(), req.ID)
	if err != nil || !found {
		t.Fatalf("Process: %v, found %v", err, found)
	}
	if p.FanoutSecret != first.FanoutSecret {
		t.Errorf("the store holds %q, want the secret the registration answered", p.FanoutSecret)
	}

	res, body = h.postJSON(http.MethodPost, processesPath, req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("register again status = %d, body %s", res.StatusCode, body)
	}
	var second processResponse
	if err := json.Unmarshal(body, &second); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if second.FanoutSecret == first.FanoutSecret {
		t.Error("registering again returned the same secret; it is minted with the token")
	}

	res, body = h.do(http.MethodGet, processesPath, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, body %s", res.StatusCode, body)
	}
	if strings.Contains(string(body), second.FanoutSecret) || strings.Contains(string(body), first.FanoutSecret) {
		t.Errorf("the list carries a fan out secret: %s", body)
	}
	if strings.Contains(string(body), "fanoutSecret") {
		t.Errorf("the list carries a fanoutSecret key: %s", body)
	}
}

// TestFanOutMovesToTheSecretOfTheNewestRegistration is what a re-run of the
// same Process does to a subscriber that is already being delivered to: the
// endpoint has not moved, so the queue is kept and the deliveries carry the
// secret the newest registration minted.
func TestFanOutMovesToTheSecretOfTheNewestRegistration(t *testing.T) {
	h := serve(t, false)
	sub := newSubscriberStub(t, false)
	req := registration(sub.server.URL)

	_, res, body := h.register(req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	var first processResponse
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}

	export := exportRequest("fs_list", h.user, time.Now().Add(-time.Minute))
	if res, body := h.do(http.MethodPost, pathTraces, otlp.ContentTypeProtobuf, export); res.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d, body %s", res.StatusCode, body)
	}
	if got := sub.await(t); got.auth != "Bearer "+first.FanoutSecret {
		t.Errorf("delivery carried %q, want the secret of the registration", got.auth)
	}

	_, res, body = h.register(req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("register again status = %d, body %s", res.StatusCode, body)
	}
	var second processResponse
	if err := json.Unmarshal(body, &second); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if res, body := h.do(http.MethodPost, pathTraces, otlp.ContentTypeProtobuf, export); res.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d, body %s", res.StatusCode, body)
	}
	if got := sub.await(t); got.auth != "Bearer "+second.FanoutSecret {
		t.Errorf("delivery carried %q, want the secret of the newest registration", got.auth)
	}
}

// TestALegacySubscriberIsDeliveredToWithoutASecret is the upgrade path: a
// registration written before the fan out was authenticated keeps receiving,
// without a bearer, and the operator is told once rather than once per
// delivery.
func TestALegacySubscriberIsDeliveredToWithoutASecret(t *testing.T) {
	captured := captureDaemonLog(t)
	h := serve(t, false)
	sub := newSubscriberStub(t, false)
	id := h.addSubscriberWithSecret(h.user, false, sub.server.URL, "")

	export := exportRequest("fs_list", h.user, time.Now().Add(-time.Minute))
	for i := 0; i < 3; i++ {
		if res, body := h.do(http.MethodPost, pathTraces, otlp.ContentTypeProtobuf, export); res.StatusCode != http.StatusOK {
			t.Fatalf("export status = %d, body %s", res.StatusCode, body)
		}
		if got := sub.await(t); got.auth != "" {
			t.Fatalf("delivery %d carried %q, want no bearer for a registration that has no secret", i, got.auth)
		}
	}

	var said int
	for _, line := range strings.Split(captured.String(), "\n") {
		if strings.Contains(line, "carries no secret") && strings.Contains(line, id) {
			said++
		}
	}
	if said != 1 {
		t.Errorf("the missing secret was logged %d times, want once: %s", said, captured.String())
	}
}
