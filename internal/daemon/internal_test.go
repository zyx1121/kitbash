package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/otlp"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
)

// window is the time range a test queries with: wide enough for a fixed clock
// and for the wall clock both.
func window() store.Filter {
	return store.Filter{Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Hour)}
}

// causes waits for the queued writes to land and returns the causes in the
// store. Recording is asynchronous on purpose: the call that failed is already
// answering, so a test waits where a caller does not.
func causes(t *testing.T, h *harness, want int) []store.Log {
	t.Helper()
	yes := true
	filter := window()
	filter.Internal = &yes
	var logs []store.Log
	waitFor(t, fmt.Sprintf("%d causes to be recorded", want), func() bool {
		page, err := h.store.Query(context.Background(), store.SignalLogs, filter)
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		logs = page.Logs
		return len(logs) >= want
	})
	return logs
}

// seedCause writes one cause the way the daemon records its own, and one
// ordinary record of the same member beside it.
func seedCause(t *testing.T, h *harness, instance, cause string) {
	t.Helper()
	yes := true
	err := h.store.Insert(context.Background(), store.Export{Logs: []store.Log{
		{TimeNS: time.Now().UnixNano(), Severity: "ERROR", Body: cause,
			Attributes: store.Attributes{User: InternalProducer, Producer: InternalProducer,
				Path: instance, Internal: &yes}},
		{TimeNS: time.Now().UnixNano(), Severity: "INFO", Body: "an ordinary record",
			Attributes: store.Attributes{User: h.user, Producer: h.user}},
	}})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
}

// queryLogs runs one tel_query over the socket and returns the records.
func queryLogs(t *testing.T, h *harness, body map[string]any) []logRecord {
	t.Helper()
	res, out := h.postJSON(http.MethodPost, queryPath, body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("query status = %d, body %s", res.StatusCode, out)
	}
	var answer struct {
		Records []logRecord `json:"records"`
	}
	if err := json.Unmarshal(out, &answer); err != nil {
		t.Fatalf("query body %q: %v", out, err)
	}
	return answer.Records
}

// The daemon holds the store, so it writes the causes it raises itself. The
// record is the whole cause, stamped as the daemon's own.
func TestTheDaemonStoresItsOwnInternalCauses(t *testing.T) {
	h := serve(t, true)
	stop := h.server.RecordInternalCauses()
	defer stop()

	p := problem.Internal("/kitbash/v1/query", "store: query logs: disk I/O error", "")
	if strings.Contains(p.Detail, "disk I/O error") {
		t.Fatal("the cause reached the caller's detail, which is what it must never do")
	}

	got := causes(t, h, 1)[0]
	if got.Body != "store: query logs: disk I/O error" {
		t.Errorf("body = %q, want the cause", got.Body)
	}
	if got.Internal == nil || !*got.Internal {
		t.Error("the record does not carry internal true, so a member would read it")
	}
	if got.Producer != InternalProducer || got.User != InternalProducer {
		t.Errorf("producer %q and user %q, want %q for a cause the daemon raised",
			got.Producer, got.User, InternalProducer)
	}
	if got.Path != "/kitbash/v1/query" {
		t.Errorf("path = %q, want the problem instance the caller was given", got.Path)
	}
	if got.Severity != internalSeverity {
		t.Errorf("severity = %q, want %q", got.Severity, internalSeverity)
	}
	if got.Other[attrError] != problem.SlugInternal {
		t.Errorf("%s = %v, want %q", attrError, got.Other[attrError], problem.SlugInternal)
	}
}

// kitbash-mcp cannot write the store, so it reports its causes to this path
// and kitbashd stamps the record from the peer credentials.
func TestTheInternalPathRecordsForThePeer(t *testing.T) {
	h := serve(t, false)
	res, body := h.postJSON(http.MethodPost, internalPath, internalRequest{
		Instance: "/org/handbook/README.md",
		Cause:    "git commit: exit status 128",
		Tool:     "fs_write",
	})
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}

	got := causes(t, h, 1)[0]
	if got.Body != "git commit: exit status 128" {
		t.Errorf("body = %q, want the cause", got.Body)
	}
	if got.User != h.user || got.Producer != h.user {
		t.Errorf("user %q and producer %q, want the peer %q", got.User, got.Producer, h.user)
	}
	if got.Path != "/org/handbook/README.md" {
		t.Errorf("path = %q, want the problem instance", got.Path)
	}
	if got.Tool != "fs_write" {
		t.Errorf("tool = %q, want the call the cause happened in", got.Tool)
	}
	if got.Internal == nil || !*got.Internal {
		t.Error("the stored record does not carry internal true")
	}

	// The member who reported it does not read it back. What they have is the
	// instance they already quoted in the problem.
	if records := queryLogs(t, h, map[string]any{"signal": "logs"}); len(records) != 0 {
		t.Errorf("the member's own query returned %d records, want none", len(records))
	}
}

// A body without a cause is a request that would store an empty record.
func TestTheInternalPathRefusesAnEmptyCause(t *testing.T) {
	h := serve(t, false)
	res, body := h.postJSON(http.MethodPost, internalPath, internalRequest{Instance: "/org/handbook"})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	if slug := h.problemOf(res, body).Slug(); slug != problem.SlugBadRequest {
		t.Errorf("slug = %s, want bad-request", slug)
	}
}

// A cause longer than the budget is cut, so one caller cannot decide how much
// of the store a record takes.
func TestALongCauseIsClipped(t *testing.T) {
	h := serve(t, false)
	res, body := h.postJSON(http.MethodPost, internalPath, internalRequest{
		Instance: "/org/handbook",
		Cause:    strings.Repeat("x", InternalCauseLimit*2),
	})
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	got := causes(t, h, 1)[0]
	if len(got.Body) > InternalCauseLimit+64 {
		t.Errorf("the stored cause is %d bytes, want it cut to %d and a note", len(got.Body), InternalCauseLimit)
	}
	if !strings.Contains(got.Body, "truncated") {
		t.Error("the cut cause does not say it was cut")
	}
}

// A session in a loop is bounded: the causes over the rate are refused with a
// 429, which is the answer that says the same request works later.
func TestTheInternalPathIsRateLimited(t *testing.T) {
	h := serve(t, false)
	for i := range InternalRate {
		res, body := h.postJSON(http.MethodPost, internalPath, internalRequest{Cause: "a cause"})
		if res.StatusCode != http.StatusNoContent {
			t.Fatalf("cause %d: status = %d, body %s", i, res.StatusCode, body)
		}
	}
	res, body := h.postJSON(http.MethodPost, internalPath, internalRequest{Cause: "one too many"})
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body %s, want 429", res.StatusCode, body)
	}
	prob := h.problemOf(res, body)
	if prob.Slug() != problem.SlugConflict {
		t.Errorf("slug = %s, want conflict", prob.Slug())
	}
	if retry := res.Header.Get("Retry-After"); retry != RetryAfterSeconds {
		t.Errorf("Retry-After = %q, want %q", retry, RetryAfterSeconds)
	}
}

// Nobody asserts kitbash.internal over OTLP, member sessions included: a
// member with a shell on the host can reach the socket with curl, and a record
// they marked would be hidden from them and shown to every admin as kitbash's
// own words. The export is stored without it and stays the member's to read.
func TestAMemberExportMayNotClaimInternal(t *testing.T) {
	h := serve(t, false)
	res, body := h.do(http.MethodPost, pathTraces, otlp.ContentTypeProtobuf,
		processExport("fs_list", map[string]any{otlp.AttrInternal: true}, false, time.Now().Add(-time.Minute)))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("export: status %d, body %s", res.StatusCode, body)
	}

	page, err := h.store.Query(context.Background(), store.SignalTraces, window())
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Spans) != 1 {
		t.Fatalf("the store holds %d spans, want the one exported", len(page.Spans))
	}
	if page.Spans[0].Internal != nil {
		t.Errorf("the stored span carries internal %v, want none: no export may claim it",
			*page.Spans[0].Internal)
	}
	if raw, kept := page.Spans[0].Other[otlp.AttrInternal]; kept {
		t.Errorf("%s survived as %v among the other attributes", otlp.AttrInternal, raw)
	}

	// The record is the member's own, so the member still reads it.
	answer, out := h.postJSON(http.MethodPost, queryPath, map[string]any{"signal": "traces"})
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("query status = %d, body %s", answer.StatusCode, out)
	}
	var records struct {
		Records []spanRecord `json:"records"`
	}
	if err := json.Unmarshal(out, &records); err != nil {
		t.Fatalf("query body %q: %v", out, err)
	}
	if len(records.Records) != 1 {
		t.Fatalf("the member's query returned %d records, want their own span", len(records.Records))
	}
	if records.Records[0].Attributes.Internal != nil {
		t.Error("the answered span is marked internal, so its own owner would stop seeing it")
	}
}

// The same rule on the Process receiver, where the producer is a container.
func TestAProcessMayNotMarkItsRecordsInternal(t *testing.T) {
	h := serve(t, false)
	base := h.serveTCP()
	token, res, body := h.register(registration(""))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("register: status %d, body %s", res.StatusCode, body)
	}

	res, body = h.exportTCP(base, pathTraces, token,
		processExport("sensorium_read", map[string]any{otlp.AttrInternal: true}, false, time.Now().Add(-time.Minute)))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("export: status %d, body %s", res.StatusCode, body)
	}

	page, err := h.store.Query(context.Background(), store.SignalTraces, window())
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Spans) != 1 {
		t.Fatalf("the store holds %d spans, want the one the Process sent", len(page.Spans))
	}
	if page.Spans[0].Internal != nil {
		t.Errorf("the stored span carries internal %v, want none: a Process may not claim it",
			*page.Spans[0].Internal)
	}
}

// A member's query never returns a cause, whatever it asks for: the record
// carries host paths and the output of what kitbash ran.
func TestAMemberQueryExcludesInternalCauses(t *testing.T) {
	h := serve(t, false)
	seedCause(t, h, "/org/handbook/README.md", "git: exit status 128")

	for _, ask := range []map[string]any{
		{"signal": "logs"},
		{"signal": "logs", "internal": true},
	} {
		records := queryLogs(t, h, ask)
		for _, record := range records {
			if record.Body == "git: exit status 128" {
				t.Errorf("a member's query %v returned the cause of an internal problem", ask)
			}
			if record.Attributes.Internal != nil && *record.Attributes.Internal {
				t.Errorf("a member's query %v returned a record marked internal", ask)
			}
		}
		if len(records) != 1 && len(ask) == 1 {
			t.Errorf("a member's query returned %d records, want the ordinary one alone", len(records))
		}
		if len(records) != 0 && len(ask) == 2 {
			t.Errorf("a member asking for causes got %d records, want none", len(records))
		}
	}
}

// An admin reads them, which is the whole point: the member quotes the
// problem's instance and the admin queries by it.
func TestAnAdminQueryReturnsInternalCauses(t *testing.T) {
	h := serve(t, true)
	seedCause(t, h, "/org/handbook/README.md", "git: exit status 128")

	all := queryLogs(t, h, map[string]any{"signal": "logs"})
	if len(all) != 2 {
		t.Fatalf("an admin's query returned %d records, want both", len(all))
	}

	found := queryLogs(t, h, map[string]any{"signal": "logs", "internal": true})
	if len(found) != 1 {
		t.Fatalf("an admin asking for causes got %d records, want the one cause", len(found))
	}
	if found[0].Body != "git: exit status 128" {
		t.Errorf("body = %q, want the cause", found[0].Body)
	}
	if found[0].Attributes.Internal == nil || !*found[0].Attributes.Internal {
		t.Error("the answered record does not carry internal, so an admin cannot tell it apart")
	}

	// The instance the member was given is the path the admin queries by.
	byInstance := queryLogs(t, h, map[string]any{
		"signal": "logs", "internal": true, "path": "/org/handbook/README.md",
	})
	if len(byInstance) != 1 {
		t.Errorf("querying by the problem instance returned %d records, want the cause", len(byInstance))
	}
}

// The fan out follows the query rule: a member's subscriber never receives a
// cause, an admin's does. Otherwise a kit a member runs would read what
// tel_query refuses them.
func TestTheFanOutSendsCausesToAdminsOnly(t *testing.T) {
	yes := true
	export := store.Export{Logs: []store.Log{
		{TimeNS: time.Now().UnixNano(), Severity: "ERROR", Body: "git: exit status 128",
			Attributes: store.Attributes{User: "alice", Producer: InternalProducer, Internal: &yes}},
		{TimeNS: time.Now().UnixNano(), Severity: "INFO", Body: "an ordinary record",
			Attributes: store.Attributes{User: "alice", Producer: "alice"}},
	}}

	member := &subscriber{id: "p1", owner: "alice"}
	if got := parts(export, member); len(got) != 1 {
		t.Fatalf("a member's subscriber gets %d parts, want the ordinary records alone", len(got))
	}
	body, err := parts(export, member)[0].encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(body), "exit status 128") {
		t.Error("a member's subscriber was sent the cause of an internal problem")
	}

	admin := &subscriber{id: "p2", owner: "root", admin: true}
	body, err = parts(export, admin)[0].encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(body), "exit status 128") {
		t.Error("an admin's subscriber was not sent the cause")
	}
	if !strings.Contains(string(body), "kitbash.internal") {
		t.Error("the delivered record does not say it is internal")
	}
}

// One peer's rate does not spend another's, and a window that has passed is
// forgotten rather than kept for the life of the daemon.
func TestTheRateLimiterIsPerPeerAndPerWindow(t *testing.T) {
	at := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	limiter := newRateLimiter(2, time.Minute)
	if !limiter.allow("alice", at) || !limiter.allow("alice", at) {
		t.Fatal("the first two causes of a peer were refused")
	}
	if limiter.allow("alice", at) {
		t.Error("the third cause inside the window was allowed")
	}
	if !limiter.allow("bob", at) {
		t.Error("another peer was refused for the first peer's causes")
	}
	if !limiter.allow("alice", at.Add(time.Minute)) {
		t.Error("a peer was refused after their window had passed")
	}
	if len(limiter.peers) != 1 {
		t.Errorf("the limiter holds %d peers after the windows passed, want the live one alone",
			len(limiter.peers))
	}
}
