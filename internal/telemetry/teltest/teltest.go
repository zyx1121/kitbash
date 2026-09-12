// Package teltest is a kitbashd standing in for the real one in tests: a unix
// socket that speaks the OTLP paths and the JSON API of spec/kitbashd-api.yaml
// and remembers what it was sent.
//
// It decodes the OTLP protobuf with the same generated types the daemon uses,
// so a test asserts on the records that actually crossed the socket rather
// than on what the exporter was asked to send.
package teltest

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/zyx1121/kitbash/internal/manifest"
	collogspb "github.com/zyx1121/kitbash/internal/otlpproto/collector/logs/v1"
	coltracepb "github.com/zyx1121/kitbash/internal/otlpproto/collector/trace/v1"
	commonpb "github.com/zyx1121/kitbash/internal/otlpproto/common/v1"
	tracepb "github.com/zyx1121/kitbash/internal/otlpproto/trace/v1"
	"github.com/zyx1121/kitbash/internal/podman"
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

// InternalCause is one cause reported to POST /kitbash/v1/internal, the path
// kitbash-mcp reports the cause of an internal problem to. The real daemon
// stamps the record from the peer credentials; a test asserts on what the
// session sent.
type InternalCause struct {
	Instance string `json:"instance,omitempty"`
	Cause    string `json:"cause"`
	Tool     string `json:"tool,omitempty"`
}

// Registration is one Process the fake has registered, as the request body of
// processes_register carries it.
type Registration struct {
	ID            string   `json:"id"`
	Package       string   `json:"package"`
	Name          string   `json:"name"`
	Container     string   `json:"container,omitempty"`
	Digest        string   `json:"digest,omitempty"`
	Expose        string   `json:"expose,omitempty"`
	Endpoint      string   `json:"endpoint,omitempty"`
	Subscriptions []string `json:"subscriptions,omitempty"`
	// Runner is the Package path of the run kit that owns the Process, as a
	// session whose manifest named a runner registers it.
	Runner string `json:"runner,omitempty"`
	// Permits is what the Package declared its Process may call over /mcp. A
	// test reads it to see what the session child would be given.
	Permits manifest.Permits `json:"permits,omitempty"`
	// Owner is the member kitbashd recorded from the socket's peer
	// credentials, and Admin whether they were an admin at registration. A
	// client sends neither; a test seeds Owner to stand in for another
	// member's Process in an admin's list.
	Owner string `json:"owner,omitempty"`
	Admin bool   `json:"admin,omitempty"`
	// Health is the probe the Package declared, as the registration carries
	// it, plus the reading a test seeds to stand in for a probe kitbashd ran.
	Health *Health `json:"health,omitempty"`
}

// Health is one Process's probe on the wire: the declaration a registration
// sends, and the reading processes_list answers with.
type Health struct {
	HTTP     string `json:"http,omitempty"`
	Interval string `json:"interval,omitempty"`
	Last     string `json:"last,omitempty"`
	Healthy  *bool  `json:"healthy,omitempty"`
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
	server   *http.Server

	mu           sync.Mutex
	spans        []Span
	logs         []LogRecord
	contentTypes []string

	// The export requests as they decoded off the wire, for a test that
	// asserts on a field the flattened Span and LogRecord do not carry.
	traceRequests []*coltracepb.ExportTraceServiceRequest
	logRequests   []*collogspb.ExportLogsServiceRequest
	calls         []Call
	query         Response
	retention     Response
	pause         time.Duration

	// The Process registry: what was registered, in arrival order, what was
	// unregistered, and the answer that replaces the registry's own.
	registrations map[string]Registration
	minted        map[string]string
	secrets       map[string]string
	order         []string
	unregistered  []string
	processes     Response
	tokens        int

	// The users family: who the caller is, since one fake serves one session,
	// and the member list users_list answers with.
	identity Identity
	members  []Member
	users    Response

	// The supervisor half: the runtime a start is mirrored into, the calls it
	// served, the answer that replaces them and the address it gives its
	// Processes, see run.go.
	runtime    *podman.Fake
	startCalls []StartCall
	starts     Response
	endpoint   string

	// The approval queue and its state machine, mirroring the approvals
	// family of spec/kitbashd-api.yaml.
	approvals       map[string]Approval
	approvalOrder   []string
	approvalIDs     int
	approvalsAnswer Response
	resultAnswer    Response

	// The causes reported to POST /kitbash/v1/internal, in arrival order.
	internalCauses []InternalCause

	// The build registry: what has been built, what a fetch was asked for,
	// the answers that replace the registry's own, and the counter that gives
	// a seeded build a time, see builds.go.
	builds       []Build
	fetches      []Fetch
	buildsAnswer Response
	fetchAnswer  Response
	buildSeq     int
}

// Start listens on a unix socket in a temporary directory of its own. The
// directory is short on purpose: a unix socket path is capped near 100 bytes
// and a test name is not.
func Start() (*Daemon, error) {
	dir, err := os.MkdirTemp("", "kbd")
	if err != nil {
		return nil, err
	}
	d, err := StartAt(filepath.Join(dir, "kitbashd.sock"))
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	d.dir = dir
	return d, nil
}

// StartAt listens on a socket the caller names, which is how a test stops a
// daemon and starts another one where the first was.
func StartAt(socket string) (*Daemon, error) {
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	d := &Daemon{
		Socket:   socket,
		listener: listener,
		query: Response{Status: http.StatusOK, ContentType: "application/json",
			Body: `{"signal":"traces","records":[],"truncated":false}`},
		retention: Response{Status: http.StatusOK, ContentType: "application/json",
			Body: `{"traces":"30d","logs":"14d","metrics":"30d"}`},
		registrations: map[string]Registration{},
		minted:        map[string]string{},
		secrets:       map[string]string{},
		approvals:     map[string]Approval{},
		// The caller is a member until a test says otherwise, which is the
		// side every rule in PLAN.md section 2.1 is written from.
		identity: Identity{User: "tester", UID: 1001, Groups: []string{"kitbash-users"}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/traces", d.traces)
	mux.HandleFunc("POST /v1/logs", d.logRecords)
	mux.HandleFunc("POST /kitbash/v1/internal", d.internalCause)
	mux.HandleFunc("/kitbash/v1/query", d.jsonAPI)
	mux.HandleFunc("/kitbash/v1/retention", d.jsonAPI)
	mux.HandleFunc("POST /kitbash/v1/processes", d.register)
	mux.HandleFunc("GET /kitbash/v1/processes", d.listProcesses)
	mux.HandleFunc("DELETE /kitbash/v1/processes/{id}", d.unregister)
	mux.HandleFunc("POST /kitbash/v1/processes/{id}/start", d.start)
	mux.HandleFunc("POST /kitbash/v1/processes/{id}/stop", d.stop)
	mux.HandleFunc("POST /kitbash/v1/processes/{id}/remove", d.remove)
	mux.HandleFunc("POST /kitbash/v1/builds", d.recordBuild)
	mux.HandleFunc("GET /kitbash/v1/builds", d.listBuilds)
	mux.HandleFunc("POST /kitbash/v1/images/{digest}/fetch", d.fetchImage)
	mux.HandleFunc("GET /kitbash/v1/users/me", d.me)
	mux.HandleFunc("POST /kitbash/v1/users", d.createUser)
	mux.HandleFunc("GET /kitbash/v1/users", d.listUsers)
	mux.HandleFunc("POST /kitbash/v1/users/{name}/keys", d.addKey)
	mux.HandleFunc("DELETE /kitbash/v1/users/{name}", d.removeUser)
	mux.HandleFunc("POST /kitbash/v1/approvals", d.createApproval)
	mux.HandleFunc("GET /kitbash/v1/approvals", d.listApprovals)
	mux.HandleFunc("POST /kitbash/v1/approvals/{id}/claim", d.claimApproval)
	mux.HandleFunc("PUT /kitbash/v1/approvals/{id}/result", d.storeResult)
	mux.HandleFunc("POST /kitbash/v1/approvals/{id}/reject", d.rejectApproval)
	d.server = &http.Server{Handler: mux}
	go func() {
		if err := d.server.Serve(listener); err != nil &&
			!errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "teltest: %v\n", err)
		}
	}()
	return d, nil
}

// Close stops the daemon and removes its socket. Open connections go with it,
// the way they would if the process had been restarted: a client holding a
// keep alive connection to a daemon that is gone has to notice. It is safe to
// call twice, so a test can stop a daemon in the middle and still defer this.
func (d *Daemon) Close() {
	d.server.Close()
	if d.dir != "" {
		os.RemoveAll(d.dir)
	}
}

// Delay makes every handler wait before it answers, which is how a daemon that
// has stopped answering is exercised. A delay longer than the client's timeout
// is a hung daemon.
func (d *Daemon) Delay(pause time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pause = pause
}

// wait honours Delay.
func (d *Daemon) wait() {
	d.mu.Lock()
	pause := d.pause
	d.mu.Unlock()
	if pause > 0 {
		time.Sleep(pause)
	}
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

// TraceRequests are the trace export requests received so far, exactly as
// they decoded off the wire. Spans is the flattened view a test usually wants;
// this is for the fields it does not carry, such as a span's kind, its events
// and its links.
func (d *Daemon) TraceRequests() []*coltracepb.ExportTraceServiceRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*coltracepb.ExportTraceServiceRequest(nil), d.traceRequests...)
}

// LogRequests are the log export requests received so far, decoded the same
// way.
func (d *Daemon) LogRequests() []*collogspb.ExportLogsServiceRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*collogspb.ExportLogsServiceRequest(nil), d.logRequests...)
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
	d.traceRequests = append(d.traceRequests, &req)
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
	d.logRequests = append(d.logRequests, &req)
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
	d.wait()
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

// internalCause records one reported cause and answers as the daemon does,
// with 204 and no body.
func (d *Daemon) internalCause(w http.ResponseWriter, r *http.Request) {
	d.wait()
	body, err := readAll(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var cause InternalCause
	if err := json.Unmarshal(body, &cause); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.internalCauses = append(d.internalCauses, cause)
	d.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// InternalCauses are the causes this daemon was told about, in order.
func (d *Daemon) InternalCauses() []InternalCause {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]InternalCause, len(d.internalCauses))
	copy(out, d.internalCauses)
	return out
}

func (d *Daemon) jsonAPI(w http.ResponseWriter, r *http.Request) {
	d.wait()
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
	write(w, answer)
}

// AnswerProcesses replaces what the Process registry returns, which is how a
// daemon that refuses a registration is exercised. The zero Response restores
// the registry's own behaviour.
func (d *Daemon) AnswerProcesses(r Response) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.processes = r
}

// AddProcess seeds the registry, standing in for a Process registered in an
// earlier session.
func (d *Daemon) AddProcess(reg Registration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.store(reg)
}

// Registrations are the Processes the registry holds, in the order they were
// registered.
func (d *Daemon) Registrations() []Registration {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Registration, 0, len(d.order))
	for _, id := range d.order {
		out = append(out, d.registrations[id])
	}
	return out
}

// Registration is the registry entry for one Process id.
func (d *Daemon) Registration(id string) (Registration, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	reg, ok := d.registrations[id]
	return reg, ok
}

// Token is the token minted for one Process id, so a test can hold the
// environment of a container to the token its registration returned.
func (d *Daemon) Token(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.minted[id]
}

// FanoutSecret is the fan out secret minted for one Process id, so a test can
// hold the environment of a container to the secret its registration returned.
func (d *Daemon) FanoutSecret(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.secrets[id]
}

// Unregistered are the Process ids a DELETE named, in arrival order, including
// the ones the registry did not know.
func (d *Daemon) Unregistered() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.unregistered...)
}

// store records one registration. The caller holds the lock.
func (d *Daemon) store(reg Registration) {
	if _, held := d.registrations[reg.ID]; !held {
		d.order = append(d.order, reg.ID)
	}
	d.registrations[reg.ID] = reg
}

// register answers processes_register: it records the Process and mints a
// token and a fan out secret for it. Registering an id again replaces the
// record and mints both again, the way spec/kitbashd-api.yaml says the daemon
// does.
func (d *Daemon) register(w http.ResponseWriter, r *http.Request) {
	d.wait()
	body, err := readAll(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	if override, ok := d.override(); ok {
		d.mu.Unlock()
		write(w, override)
		return
	}
	var reg Registration
	if err := json.Unmarshal(body, &reg); err != nil || reg.ID == "" {
		d.mu.Unlock()
		write(w, Problem(http.StatusBadRequest, "bad-request", "Bad request",
			"the body is not a Process registration", ""))
		return
	}
	d.store(reg)
	d.tokens++
	token := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s-%d", reg.ID, d.tokens))))
	secret := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s-%d-fanout", reg.ID, d.tokens))))
	d.minted[reg.ID] = token
	d.secrets[reg.ID] = secret
	d.mu.Unlock()
	answer, _ := json.Marshal(map[string]string{"id": reg.ID, "token": token, "fanoutSecret": secret})
	write(w, Response{Status: http.StatusOK, ContentType: "application/json", Body: string(answer)})
}

// listProcesses answers processes_list, without tokens.
func (d *Daemon) listProcesses(w http.ResponseWriter, r *http.Request) {
	d.wait()
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path})
	if override, ok := d.override(); ok {
		d.mu.Unlock()
		write(w, override)
		return
	}
	list := make([]Registration, 0, len(d.order))
	for _, id := range d.order {
		list = append(list, d.registrations[id])
	}
	d.mu.Unlock()
	answer, _ := json.Marshal(map[string]any{"processes": list})
	write(w, Response{Status: http.StatusOK, ContentType: "application/json", Body: string(answer)})
}

// unregister answers processes_unregister: 204 for a Process it knew, 404 for
// one it did not.
func (d *Daemon) unregister(w http.ResponseWriter, r *http.Request) {
	d.wait()
	id := r.PathValue("id")
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path})
	d.unregistered = append(d.unregistered, id)
	if override, ok := d.override(); ok {
		d.mu.Unlock()
		write(w, override)
		return
	}
	_, known := d.registrations[id]
	if known {
		delete(d.registrations, id)
		kept := d.order[:0]
		for _, held := range d.order {
			if held != id {
				kept = append(kept, held)
			}
		}
		d.order = kept
	}
	d.mu.Unlock()
	if !known {
		write(w, Problem(http.StatusNotFound, "not-found", "Not found",
			"no Process of yours has this id", ""))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// override is the answer AnswerProcesses installed, if any. The caller holds
// the lock.
func (d *Daemon) override() (Response, bool) {
	if d.processes.Status == 0 {
		return Response{}, false
	}
	return d.processes, true
}

// write sends one canned response.
func write(w http.ResponseWriter, answer Response) {
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
