// Package teltest is a kitbashd standing in for the real one in tests: a unix
// socket that speaks the OTLP paths and the JSON API of spec/kitbashd-api.yaml
// and remembers what it was sent.
//
// It decodes the OTLP protobuf with the same generated types the daemon uses,
// so a test asserts on the records that actually crossed the socket rather
// than on what the exporter was asked to send.
package teltest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"google.golang.org/protobuf/proto"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// maxBodyBytes is the request limit of spec/kitbashd-api.yaml.
const maxBodyBytes = 4 << 20

// Span is one received span, flattened to what a test asserts on.
type Span struct {
	Name          string
	TraceID       string
	SpanID        string
	ParentSpanID  string
	Status        string
	StatusMessage string
	Attributes    map[string]string
	Resource      map[string]string
}

// LogRecord is one received log record, flattened the same way.
type LogRecord struct {
	Severity   string
	Body       string
	TraceID    string
	SpanID     string
	Attributes map[string]string
}

// Call is one request to the JSON API.
type Call struct {
	Method string
	Path   string
	Body   string
}

// Response is what the daemon answers on the JSON API.
type Response struct {
	Status      int
	ContentType string
	Body        string
}

// Daemon is the fake. Its zero value is not usable; call Start.
type Daemon struct {
	// Socket is the path to give KITBASH_SOCKET.
	Socket string

	dir      string
	listener net.Listener

	mu           sync.Mutex
	spans        []Span
	logs         []LogRecord
	contentTypes []string
	calls        []Call
	query        Response
	retention    Response
}

// Start listens on a unix socket in a temporary directory of its own. The
// directory is short on purpose: a unix socket path is capped near 100 bytes
// and a test name is not.
func Start() (*Daemon, error) {
	dir, err := os.MkdirTemp("", "kbd")
	if err != nil {
		return nil, err
	}
	socket := filepath.Join(dir, "kitbashd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	d := &Daemon{
		Socket:   socket,
		dir:      dir,
		listener: listener,
		query: Response{Status: http.StatusOK, ContentType: "application/json",
			Body: `{"signal":"traces","records":[],"truncated":false}`},
		retention: Response{Status: http.StatusOK, ContentType: "application/json",
			Body: `{"traces":"30d","logs":"14d","metrics":"30d"}`},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/traces", d.traces)
	mux.HandleFunc("POST /v1/logs", d.logRecords)
	mux.HandleFunc("/kitbash/v1/query", d.jsonAPI)
	mux.HandleFunc("/kitbash/v1/retention", d.jsonAPI)
	go func() {
		if err := http.Serve(listener, mux); err != nil && !errors.Is(err, net.ErrClosed) {
			fmt.Fprintf(os.Stderr, "teltest: %v\n", err)
		}
	}()
	return d, nil
}

// Close stops the daemon and removes its socket.
func (d *Daemon) Close() {
	d.listener.Close()
	os.RemoveAll(d.dir)
}

// AnswerQuery replaces what /kitbash/v1/query returns.
func (d *Daemon) AnswerQuery(r Response) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.query = r
}

// AnswerRetention replaces what /kitbash/v1/retention returns.
func (d *Daemon) AnswerRetention(r Response) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.retention = r
}

// Spans are the spans received so far, in arrival order.
func (d *Daemon) Spans() []Span {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Span(nil), d.spans...)
}

// Span is the one received span with this name, and false when there is not
// exactly one.
func (d *Daemon) Span(name string) (Span, bool) {
	var found Span
	seen := 0
	for _, span := range d.Spans() {
		if span.Name == name {
			found = span
			seen++
		}
	}
	return found, seen == 1
}

// Logs are the log records received so far, in arrival order.
func (d *Daemon) Logs() []LogRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]LogRecord(nil), d.logs...)
}

// ContentTypes are the content types the OTLP requests carried.
func (d *Daemon) ContentTypes() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.contentTypes...)
}

// Calls are the JSON API requests received so far.
func (d *Daemon) Calls() []Call {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Call(nil), d.calls...)
}

func (d *Daemon) traces(w http.ResponseWriter, r *http.Request) {
	var req coltracepb.ExportTraceServiceRequest
	if !d.decode(w, r, &req) {
		return
	}
	d.mu.Lock()
	for _, resource := range req.GetResourceSpans() {
		res := attributes(resource.GetResource().GetAttributes())
		for _, scope := range resource.GetScopeSpans() {
			for _, span := range scope.GetSpans() {
				d.spans = append(d.spans, Span{
					Name:          span.GetName(),
					TraceID:       fmt.Sprintf("%x", span.GetTraceId()),
					SpanID:        fmt.Sprintf("%x", span.GetSpanId()),
					ParentSpanID:  fmt.Sprintf("%x", span.GetParentSpanId()),
					Status:        status(span.GetStatus()),
					StatusMessage: span.GetStatus().GetMessage(),
					Attributes:    attributes(span.GetAttributes()),
					Resource:      res,
				})
			}
		}
	}
	d.mu.Unlock()
	reply(w, &coltracepb.ExportTraceServiceResponse{})
}

func (d *Daemon) logRecords(w http.ResponseWriter, r *http.Request) {
	var req collogspb.ExportLogsServiceRequest
	if !d.decode(w, r, &req) {
		return
	}
	d.mu.Lock()
	for _, resource := range req.GetResourceLogs() {
		for _, scope := range resource.GetScopeLogs() {
			for _, record := range scope.GetLogRecords() {
				d.logs = append(d.logs, LogRecord{
					Severity:   record.GetSeverityText(),
					Body:       record.GetBody().GetStringValue(),
					TraceID:    fmt.Sprintf("%x", record.GetTraceId()),
					SpanID:     fmt.Sprintf("%x", record.GetSpanId()),
					Attributes: attributes(record.GetAttributes()),
				})
			}
		}
	}
	d.mu.Unlock()
	reply(w, &collogspb.ExportLogsServiceResponse{})
}

// decode reads one OTLP protobuf request, recording the content type it came
// with so a test can hold the exporter to application/x-protobuf.
func (d *Daemon) decode(w http.ResponseWriter, r *http.Request, into proto.Message) bool {
	body, err := readAll(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	d.mu.Lock()
	d.contentTypes = append(d.contentTypes, r.Header.Get("Content-Type"))
	d.mu.Unlock()
	if err := proto.Unmarshal(body, into); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func (d *Daemon) jsonAPI(w http.ResponseWriter, r *http.Request) {
	body, err := readAll(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	answer := d.query
	if r.URL.Path == "/kitbash/v1/retention" {
		answer = d.retention
	}
	d.mu.Unlock()

	contentType := answer.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(answer.Status)
	fmt.Fprint(w, answer.Body)
}

// Problem renders an RFC 9457 body the way kitbashd answers a refusal.
func Problem(status int, slug, title, detail, fix string) Response {
	body, _ := json.Marshal(map[string]any{
		"type":   "https://kitbash.zyx.tw/errors/" + slug,
		"title":  title,
		"status": status,
		"detail": detail,
		"fix":    fix,
	})
	return Response{Status: status, ContentType: "application/problem+json", Body: string(body)}
}

func reply(w http.ResponseWriter, message proto.Message) {
	body, err := proto.Marshal(message)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Write(body)
}

// readAll reads one request body, bounded by the limit kitbashd enforces.
func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
}

func status(s *tracepb.Status) string {
	switch s.GetCode() {
	case tracepb.Status_STATUS_CODE_OK:
		return "ok"
	case tracepb.Status_STATUS_CODE_ERROR:
		return "error"
	default:
		return "unset"
	}
}

func attributes(kvs []*commonpb.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		out[kv.GetKey()] = value(kv.GetValue())
	}
	return out
}

func value(v *commonpb.AnyValue) string {
	switch inner := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return inner.StringValue
	case *commonpb.AnyValue_BoolValue:
		return fmt.Sprint(inner.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return fmt.Sprint(inner.IntValue)
	case *commonpb.AnyValue_DoubleValue:
		return fmt.Sprint(inner.DoubleValue)
	default:
		return v.String()
	}
}
