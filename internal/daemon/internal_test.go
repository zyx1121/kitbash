package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/otlp"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
)

// recordInternalCause seeds one cause the way the daemon records its own, and
// one ordinary record of the same member beside it.
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

// The daemon has no exporter, so it writes its own causes into its store. The
// record is the whole cause, stamped as the daemon's own.
func TestTheDaemonStoresItsOwnInternalCauses(t *testing.T) {
	h := serve(t, true)
	stop := h.server.RecordInternalCauses()
	defer stop()

	p := problem.Internal("/kitbash/v1/query", "store: query logs: disk I/O error", "")
	if p.Detail == "store: query logs: disk I/O error" {
		t.Fatal("the cause reached the caller's detail, which is what it must never do")
	}

	page, err := h.store.Query(context.Background(), store.SignalLogs, store.Filter{
		Since: time.Now().Add(-time.Minute), Until: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Logs) != 1 {
		t.Fatalf("the store holds %d records, want the one cause", len(page.Logs))
	}
	got := page.Logs[0]
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

	// Once the hook is removed the daemon records nothing more, which is what
	// the store being closed after it depends on.
	stop()
	problem.Internal("/kitbash/v1/query", "a second cause", "")
	page, err = h.store.Query(context.Background(), store.SignalLogs, store.Filter{
		Since: time.Now().Add(-time.Minute), Until: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Logs) != 1 {
		t.Errorf("the store holds %d records after the hook was removed, want the first one alone", len(page.Logs))
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

	causes := queryLogs(t, h, map[string]any{"signal": "logs", "internal": true})
	if len(causes) != 1 {
		t.Fatalf("an admin asking for causes got %d records, want the one cause", len(causes))
	}
	if causes[0].Body != "git: exit status 128" {
		t.Errorf("body = %q, want the cause", causes[0].Body)
	}
	if causes[0].Attributes.Internal == nil || !*causes[0].Attributes.Internal {
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

// kitbash.internal is not a Process's to claim. A kit that marked its records
// as internal causes would hide them from its own owner and put its words in
// front of every admin, so the attribute is dropped on the way in, the same
// rule kitbash.caller follows.
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

	page, err := h.store.Query(context.Background(), store.SignalTraces, store.Filter{
		Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Minute),
	})
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
	if raw, kept := page.Spans[0].Other[otlp.AttrInternal]; kept {
		t.Errorf("%s survived as %v among the other attributes", otlp.AttrInternal, raw)
	}

	// The record is the owner's, so the owner still reads it.
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
		t.Fatalf("the owner's query returned %d records, want the Process's span", len(records.Records))
	}
	if records.Records[0].Attributes.Internal != nil {
		t.Error("the answered span is marked internal, so its own owner would stop seeing it")
	}
}
