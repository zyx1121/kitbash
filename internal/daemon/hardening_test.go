package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os/user"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	colmetrics "github.com/zyx1121/kitbash/internal/otlpproto/collector/metrics/v1"
	metricspb "github.com/zyx1121/kitbash/internal/otlpproto/metrics/v1"

	"github.com/zyx1121/kitbash/internal/otlp"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
)

// metricExport is one gauge point of the given value.
func metricExport(name string, value float64, at time.Time) []byte {
	req := &colmetrics.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{{
					Name: name,
					Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
						TimeUnixNano: uint64(at.UnixNano()),
						Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: value},
					}}}},
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

// TestNonFiniteMetricsKeepQueriesAnswerable is the regression for a metric
// value JSON cannot spell: it used to be stored and then every query of that
// window answered 500 for as long as the retention window lasted.
func TestNonFiniteMetricsKeepQueriesAnswerable(t *testing.T) {
	h := serve(t, false)
	at := time.Now().Add(-time.Minute)

	for _, tc := range []struct {
		name  string
		value float64
	}{
		{"infinity", math.Inf(1)},
		{"negative infinity", math.Inf(-1)},
		{"not a number", math.NaN()},
	} {
		res, body := h.do(http.MethodPost, "/v1/metrics", otlp.ContentTypeProtobuf,
			metricExport(tc.name, tc.value, at))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s export status = %d, body %s", tc.name, res.StatusCode, body)
		}
	}
	// A finite point in the same window still lands.
	res, body := h.do(http.MethodPost, "/v1/metrics", otlp.ContentTypeProtobuf,
		metricExport("finite", 12.5, at))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("finite export status = %d, body %s", res.StatusCode, body)
	}

	res, body = h.postJSON(http.MethodPost, "/kitbash/v1/query", queryRequest{Signal: store.SignalMetrics})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("query status = %d, want 200, body %s", res.StatusCode, body)
	}
	var got struct {
		Records []metricRecord `json:"records"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(got.Records) != 1 || got.Records[0].Name != "finite" {
		t.Fatalf("records = %+v, want only the finite point", got.Records)
	}
}

// TestNonFiniteAttributeKeepsTheRecord is the regression for a double
// attribute of NaN, which used to make the whole export fail.
func TestNonFiniteAttributeKeepsTheRecord(t *testing.T) {
	h := serve(t, false)
	body := []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{
		"traceId":"0102030405060708090a0b0c0d0e0f10",
		"spanId":"1112131415161718",
		"name":"fs_list",
		"startTimeUnixNano":"1700000000000000000",
		"endTimeUnixNano":"1700000000100000000",
		"attributes":[{"key":"ratio","value":{"doubleValue":"Infinity"}},
		              {"key":"kitbash.tool","value":{"stringValue":"fs_list"}}]}]}]}]}`)
	res, got := h.do(http.MethodPost, "/v1/traces", otlp.ContentTypeJSON, body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the record stored without the attribute, body %s", res.StatusCode, got)
	}
	page, err := h.store.Query(context.Background(), store.SignalTraces, store.Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Spans) != 1 {
		t.Fatalf("spans = %d, want the record kept", len(page.Spans))
	}
	if _, present := page.Spans[0].Other["ratio"]; present {
		t.Errorf("other still carries the infinite attribute: %v", page.Spans[0].Other)
	}
	if page.Spans[0].Tool != "fs_list" {
		t.Errorf("Tool = %q, want the rest of the record kept", page.Spans[0].Tool)
	}
}

// TestRetentionRefusesOverflowingWindows is the regression for a window that
// wrapped time.Duration: 213504d became about 25 minutes and the next sweep
// deleted almost everything.
func TestRetentionRefusesOverflowingWindows(t *testing.T) {
	admin := serve(t, true)
	for _, window := range []string{"213504d", "5124096h", "0h", "0d"} {
		value := window
		res, body := admin.postJSON(http.MethodPut, "/kitbash/v1/retention", store.RetentionSet{Traces: &value})
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT %s status = %d, want 400, body %s", window, res.StatusCode, body)
			continue
		}
		if slug := admin.problemOf(res, body).Slug(); slug != problem.SlugBadRequest {
			t.Errorf("slug for %s = %q", window, slug)
		}
	}
	// The refused windows left the default in place, so a sweep keeps recent
	// records rather than deleting them.
	res, body := admin.do(http.MethodGet, "/kitbash/v1/retention", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, body %s", res.StatusCode, body)
	}
	var current store.Retention
	if err := json.Unmarshal(body, &current); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if current.Traces != "30d" {
		t.Errorf("traces window = %q, want the default", current.Traces)
	}

	res, body = admin.do(http.MethodPost, "/v1/traces", otlp.ContentTypeProtobuf,
		exportRequest("fs_list", "", time.Now().Add(-time.Hour)))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d, body %s", res.StatusCode, body)
	}
	if _, err := admin.server.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	page, err := admin.store.Query(context.Background(), store.SignalTraces, store.Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Spans) != 1 {
		t.Errorf("spans left = %d, want the recent one kept", len(page.Spans))
	}
}

// TestIdleConnectionIsClosed is the regression for connections that lived
// forever: four hundred idle ones pinned four hundred file descriptors.
func TestIdleConnectionIsClosed(t *testing.T) {
	h := serveWith(t, Options{Timeouts: Timeouts{Idle: 200 * time.Millisecond}})

	conn, err := net.Dial("unix", h.socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprint(conn, "GET /kitbash/v1/health HTTP/1.1\r\nHost: kitbashd\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := make([]byte, 4096)
	conn.SetReadDeadline(time.Now().Add(waitBudget))
	n, err := conn.Read(reply)
	if err != nil {
		t.Fatalf("read the response: %v", err)
	}
	if !bytes.Contains(reply[:n], []byte("200")) {
		t.Fatalf("response = %q, want 200", reply[:n])
	}

	// The connection is now idle and keep alive. The daemon must hang up.
	conn.SetReadDeadline(time.Now().Add(waitBudget))
	if _, err := conn.Read(reply); err == nil {
		t.Error("the daemon kept an idle connection open")
	} else if !isClosed(err) {
		t.Errorf("read after the idle timeout = %v, want the connection closed", err)
	}
}

// TestSlowBodyIsCut is the regression for a body dribbled a byte at a time,
// which used to hold a connection and a handler open indefinitely.
func TestSlowBodyIsCut(t *testing.T) {
	h := serveWith(t, Options{Timeouts: Timeouts{Read: 300 * time.Millisecond}})

	conn, err := net.Dial("unix", h.socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// A body that promises far more than it will ever send.
	if _, err := fmt.Fprintf(conn, "POST /v1/traces HTTP/1.1\r\nHost: kitbashd\r\n"+
		"Content-Type: %s\r\nContent-Length: 4096\r\n\r\nx", otlp.ContentTypeProtobuf); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Within a few times the read timeout the daemon has either answered or
	// hung up. What it must not do is wait for the rest of the body.
	conn.SetReadDeadline(time.Now().Add(waitBudget))
	reply := make([]byte, 4096)
	n, err := conn.Read(reply)
	if err != nil {
		if !isClosed(err) {
			t.Fatalf("read = %v, want a response or a closed connection", err)
		}
		return
	}
	if !bytes.Contains(reply[:n], []byte("HTTP/1.1")) {
		t.Errorf("response = %q", reply[:n])
	}
}

// isClosed reports whether an error is the daemon hanging up rather than a
// deadline the test itself set.
func isClosed(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return false
	}
	return strings.Contains(err.Error(), "closed") || strings.Contains(err.Error(), "reset")
}

// TestJSONBodyTooLarge is the must-do for the JSON API: an oversize body is
// 413 with problem details, the same answer the OTLP paths give.
func TestJSONBodyTooLarge(t *testing.T) {
	h := serve(t, false)
	// Valid JSON, far over the limit, so the size is what refuses it.
	oversize := []byte(`{"signal":"traces","tool":"` + strings.Repeat("x", otlp.MaxBodyBytes+16) + `"}`)
	res, body := h.do(http.MethodPost, "/kitbash/v1/query", "application/json", oversize)
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body %s", res.StatusCode, body)
	}
	if slug := h.problemOf(res, body).Slug(); slug != problem.SlugTooLarge {
		t.Errorf("slug = %q, want %q", slug, problem.SlugTooLarge)
	}
}

// TestAdminReadsEveryMember is the must-do for the admin path: with no user
// filter an admin sees every member's records, not only their own.
func TestAdminReadsEveryMember(t *testing.T) {
	admin := serve(t, true)
	ctx := context.Background()
	start := time.Now().Add(-time.Minute)
	if err := admin.store.Insert(ctx, store.Export{Spans: []store.Span{
		{
			TraceID: strings.Repeat("11", 16), SpanID: strings.Repeat("22", 8), Name: "fs_read",
			StartNS: start.UnixNano(), EndNS: start.UnixNano(), Status: store.StatusOK,
			Attributes: store.Attributes{User: "alice"},
		},
		{
			TraceID: strings.Repeat("33", 16), SpanID: strings.Repeat("44", 8), Name: "pkg_build",
			StartNS: start.UnixNano(), EndNS: start.UnixNano(), Status: store.StatusOK,
			Attributes: store.Attributes{User: "bob"},
		},
	}}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// One record of the admin's own, exported over the socket.
	res, body := admin.do(http.MethodPost, "/v1/traces", otlp.ContentTypeProtobuf,
		exportRequest("proc_run", "", start))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d, body %s", res.StatusCode, body)
	}

	res, body = admin.postJSON(http.MethodPost, "/kitbash/v1/query", queryRequest{Signal: store.SignalTraces})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("query status = %d, body %s", res.StatusCode, body)
	}
	var got struct {
		Records []spanRecord `json:"records"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	users := map[string]bool{}
	for _, record := range got.Records {
		users[record.Attributes.User] = true
	}
	for _, want := range []string{"alice", "bob", admin.user} {
		if !users[want] {
			t.Errorf("an admin querying without a user did not see %s, saw %v", want, users)
		}
	}

	// Naming one member still narrows it to that member.
	res, body = admin.postJSON(http.MethodPost, "/kitbash/v1/query",
		queryRequest{Signal: store.SignalTraces, User: "alice"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("query status = %d, body %s", res.StatusCode, body)
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(got.Records) != 1 || got.Records[0].Attributes.User != "alice" {
		t.Errorf("records = %+v, want only alice's", got.Records)
	}
}

// TestErrorDetailsNameNoGoTypes is the must-do about what a member reads: an
// error says what to fix, never which Go type failed to decode.
func TestErrorDetailsNameNoGoTypes(t *testing.T) {
	h := serve(t, false)
	cases := []struct {
		name        string
		path        string
		contentType string
		body        []byte
	}{
		{"bad otlp json", "/v1/traces", otlp.ContentTypeJSON, []byte(`{"nonsense":1}`)},
		{"bad otlp protobuf", "/v1/traces", otlp.ContentTypeProtobuf, []byte{0xff, 0xff, 0xff}},
		{"unknown query field", "/kitbash/v1/query", "application/json", []byte(`{"signal":"traces","colour":"red"}`)},
		{"query field of the wrong type", "/kitbash/v1/query", "application/json", []byte(`{"signal":"traces","limit":"many"}`)},
		{"body that is not json", "/kitbash/v1/query", "application/json", []byte(`{`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, body := h.do(http.MethodPost, tc.path, tc.contentType, tc.body)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body %s", res.StatusCode, body)
			}
			detail := h.problemOf(res, body).Detail
			for _, leak := range []string{"Go struct", "v1.", "*v1", "queryRequest", "RetentionSet", "of type int"} {
				if strings.Contains(detail, leak) {
					t.Errorf("detail %q leaks %q", detail, leak)
				}
			}
		})
	}
}

// TestExportRefusesShortIdentifiers is the must-do about identifier width: the
// query surface publishes a pattern, so a record that would break it is
// refused at the door.
func TestExportRefusesShortIdentifiers(t *testing.T) {
	h := serve(t, false)
	body := []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{
		"traceId":"0102","spanId":"1112131415161718","name":"fs_list",
		"startTimeUnixNano":"1700000000000000000","endTimeUnixNano":"1700000000100000000"}]}]}]}`)
	res, got := h.do(http.MethodPost, "/v1/traces", otlp.ContentTypeJSON, body)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body %s", res.StatusCode, got)
	}
	if slug := h.problemOf(res, got).Slug(); slug != problem.SlugBadRequest {
		t.Errorf("slug = %q", slug)
	}
	page, err := h.store.Query(context.Background(), store.SignalTraces, store.Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Spans) != 0 {
		t.Errorf("a refused request stored %d spans", len(page.Spans))
	}
}

// TestConnectionsAreCapped checks the daemon serves a bounded number of
// connections at once: with a cap of one, a second caller waits for the first
// connection to go idle rather than being served in parallel.
func TestConnectionsAreCapped(t *testing.T) {
	h := serveWith(t, Options{
		MaxConnections: 1,
		Timeouts:       Timeouts{Idle: 300 * time.Millisecond},
		Admin:          func(*user.User) (bool, error) { return false, nil },
	})

	held, err := net.Dial("unix", h.socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer held.Close()
	if _, err := fmt.Fprint(held, "GET /kitbash/v1/health HTTP/1.1\r\nHost: kitbashd\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := make([]byte, 4096)
	held.SetReadDeadline(time.Now().Add(waitBudget))
	if _, err := held.Read(reply); err != nil {
		t.Fatalf("read: %v", err)
	}

	// The second caller gets served once the first connection times out, so
	// the cap delays rather than refuses.
	start := time.Now()
	res, body := h.do(http.MethodGet, "/kitbash/v1/health", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("second caller status = %d, body %s", res.StatusCode, body)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Errorf("the second connection was served in %v, so the cap did not apply", time.Since(start))
	}
}
