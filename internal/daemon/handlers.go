package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zyx1121/kitbash/internal/otlp"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
)

// writeProblem is the only way an error leaves this package. Nothing else is
// ever written to a client that is not an OTLP response or an API body.
func writeProblem(w http.ResponseWriter, p *problem.Problem) {
	w.Header().Set("Content-Type", ProblemContentType)
	// RFC 9110 requires a challenge with a 401, and the only 401 this API
	// answers is a Process without a usable token on the receiver.
	if p.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", BearerChallenge)
	}
	// RFC 9110 gives a 429 a Retry-After, and the only 429 this API answers is
	// a Process holding every MCP session slot it may have. A minute is long
	// enough for a session of that Process to end and short enough that a
	// client waiting it out is not stuck.
	if p.Status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", RetryAfterSeconds)
	}
	w.WriteHeader(p.Status)
	fmt.Fprintln(w, p.JSON())
}

// writeJSON renders one API response.
func writeJSON(w http.ResponseWriter, instance string, body any) {
	writeJSONStatus(w, instance, http.StatusOK, body)
}

// writeJSONStatus is writeJSON with the status named, which one call needs:
// users_remove answers 202, because what it starts is a job kitbashd runs for
// itself and not work the caller waits out, see removal.go.
func writeJSONStatus(w http.ResponseWriter, instance string, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		writeProblem(w, problem.Internal(instance, fmt.Sprintf("encode response: %v", err), ""))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

// decodeTraces, decodeLogs and decodeMetrics are the three OTLP paths. They
// differ only in what they decode, so they share everything else, stamping
// included.
func decodeTraces(f otlp.Format, body []byte, id identity) (store.Export, []byte, error) {
	spans, err := otlp.DecodeTraces(f, body)
	if err != nil {
		return store.Export{}, nil, err
	}
	for i := range spans {
		id.apply(&spans[i].Attributes)
	}
	res, err := otlp.TracesResponse(f)
	return store.Export{Spans: spans}, res, err
}

func decodeLogs(f otlp.Format, body []byte, id identity) (store.Export, []byte, error) {
	logs, err := otlp.DecodeLogs(f, body)
	if err != nil {
		return store.Export{}, nil, err
	}
	for i := range logs {
		id.apply(&logs[i].Attributes)
	}
	res, err := otlp.LogsResponse(f)
	return store.Export{Logs: logs}, res, err
}

func decodeMetrics(f otlp.Format, body []byte, id identity) (store.Export, []byte, error) {
	metrics, err := otlp.DecodeMetrics(f, body)
	if err != nil {
		return store.Export{}, nil, err
	}
	for i := range metrics {
		id.apply(&metrics[i].Attributes)
	}
	res, err := otlp.MetricsResponse(f)
	return store.Export{Metrics: metrics}, res, err
}

// decoder turns one request body into records to store and the response body
// that acknowledges them.
type decoder func(f otlp.Format, body []byte, id identity) (store.Export, []byte, error)

// export is the shared OTLP path: identity, size limit, decode, stamp, store,
// fan out. kitbash.user is taken from the connection on every record, replacing
// whatever the producer sent, see PLAN.md section 2.4.
//
// The same handler serves the socket and the Process receiver; only how the
// identity is read differs, so a Process and a member session cannot drift
// apart in what they are allowed to say about themselves.
func (s *Server) export(ident identifier, decode decoder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, prob := ident(r)
		if prob != nil {
			writeProblem(w, prob)
			return
		}
		format, err := otlp.ParseFormat(r.Header.Get("Content-Type"))
		if err != nil {
			writeProblem(w, problem.BadRequest(r.URL.Path, err.Error(),
				"Send the export as application/x-protobuf or application/json."))
			return
		}
		if r.ContentLength > otlp.MaxBodyBytes {
			writeProblem(w, tooLarge(r.URL.Path))
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, otlp.MaxBodyBytes))
		if err != nil {
			var limit *http.MaxBytesError
			if errors.As(err, &limit) {
				writeProblem(w, tooLarge(r.URL.Path))
				return
			}
			writeProblem(w, problem.BadRequest(r.URL.Path, fmt.Sprintf("could not read the request body: %v", err),
				"Send the export again."))
			return
		}

		export, response, err := decode(format, body, id)
		if err != nil {
			writeProblem(w, problem.BadRequest(r.URL.Path, err.Error(),
				"Send an OTLP export request the OpenTelemetry protocol defines."))
			return
		}
		if err := s.store.Insert(r.Context(), export); err != nil {
			writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
			return
		}
		// The store is the source of truth and the records are in it. What
		// the subscribers do with them cannot hold up this response, so the
		// fan out only queues here, see spec/kitbashd-api.yaml.
		s.fanout.dispatch(export, id.Producer)

		w.Header().Set("Content-Type", format.ContentType())
		w.WriteHeader(http.StatusOK)
		w.Write(response)
	}
}

func tooLarge(instance string) *problem.Problem {
	return problem.TooLarge(instance,
		fmt.Sprintf("a request body may carry at most %d bytes", otlp.MaxBodyBytes),
		"Send a smaller body, and export Telemetry in smaller batches.")
}

// queryRequest is the tel_query input of spec/mcp-surface.yaml.
type queryRequest struct {
	Signal   string `json:"signal"`
	User     string `json:"user,omitempty"`
	Package  string `json:"package,omitempty"`
	Process  string `json:"process,omitempty"`
	Unit     string `json:"unit,omitempty"`
	Path     string `json:"path,omitempty"`
	Tool     string `json:"tool,omitempty"`
	Eval     *bool  `json:"eval,omitempty"`
	Internal *bool  `json:"internal,omitempty"`
	Producer string `json:"producer,omitempty"`
	Caller   string `json:"caller,omitempty"`
	Since    string `json:"since,omitempty"`
	Until    string `json:"until,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// queryResponse is the tel_query output.
type queryResponse struct {
	Signal    string `json:"signal"`
	Truncated bool   `json:"truncated"`
	Records   []any  `json:"records"`
}

// query answers /kitbash/v1/query. A member reads their own records; an admin
// reads anyone's, see spec/kitbashd-api.yaml.
func (s *Server) query(w http.ResponseWriter, r *http.Request) {
	caller, prob := s.caller(r)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	var req queryRequest
	if prob := decodeBody(w, r, &req); prob != nil {
		writeProblem(w, prob)
		return
	}

	switch req.Signal {
	case store.SignalTraces, store.SignalLogs, store.SignalMetrics:
	default:
		writeProblem(w, problem.BadRequest(r.URL.Path,
			fmt.Sprintf("%q is not a signal", req.Signal),
			"Query one of traces, logs or metrics."))
		return
	}
	if !caller.Admin {
		if req.User != "" && req.User != caller.User {
			writeProblem(w, problem.NotPermitted(r.URL.Path,
				fmt.Sprintf("%s may not read the Telemetry of %s", caller.User, req.User),
				"Query your own records, or ask an administrator to run this query."))
			return
		}
		req.User = caller.User
	}

	now := s.now()
	filter := store.Filter{
		User:     req.User,
		Package:  req.Package,
		Process:  req.Process,
		Unit:     req.Unit,
		Path:     req.Path,
		Tool:     req.Tool,
		Eval:     req.Eval,
		Internal: req.Internal,
		Producer: req.Producer,
		Caller:   req.Caller,
		Since:    now.Add(-QueryWindow),
		Until:    now,
		Limit:    req.Limit,
	}
	if !caller.Admin {
		// The cause of an internal problem carries host paths and the output
		// of whatever kitbash ran, so only an admin reads one. A member's
		// query excludes them rather than being refused: what the member has
		// instead is the problem's instance to quote to an admin, see PLAN.md
		// section 2.4.
		if req.Internal != nil && *req.Internal {
			// Asking for exactly the records this member never reads is
			// answered with the empty page. Excluding them silently here
			// would answer with the ordinary records the filter did not ask
			// for, which reads as if those were the causes.
			writeJSON(w, r.URL.Path, queryResponse{Signal: req.Signal, Records: []any{}})
			return
		}
		filter.Internal = &notInternal
	}
	if req.Since != "" {
		since, err := time.Parse(time.RFC3339, req.Since)
		if err != nil {
			writeProblem(w, problem.BadRequest(r.URL.Path, fmt.Sprintf("since %q is not an RFC 3339 time", req.Since),
				"Send since as an RFC 3339 timestamp, such as 2026-09-06T12:00:00Z."))
			return
		}
		filter.Since = since
	}
	if req.Until != "" {
		until, err := time.Parse(time.RFC3339, req.Until)
		if err != nil {
			writeProblem(w, problem.BadRequest(r.URL.Path, fmt.Sprintf("until %q is not an RFC 3339 time", req.Until),
				"Send until as an RFC 3339 timestamp, such as 2026-09-06T12:00:00Z."))
			return
		}
		filter.Until = until
	}
	if req.Limit < 0 || req.Limit > store.MaxLimit {
		writeProblem(w, problem.BadRequest(r.URL.Path,
			fmt.Sprintf("limit %d is outside 1 to %d", req.Limit, store.MaxLimit),
			fmt.Sprintf("Ask for between 1 and %d records.", store.MaxLimit)))
		return
	}

	page, err := s.store.Query(r.Context(), req.Signal, filter)
	if err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	writeJSON(w, r.URL.Path, queryResponse{
		Signal:    req.Signal,
		Truncated: page.Truncated,
		Records:   records(page),
	})
}

// retention answers both verbs of /kitbash/v1/retention.
func (s *Server) retention(w http.ResponseWriter, r *http.Request) {
	caller, prob := s.caller(r)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	switch r.Method {
	case http.MethodGet:
		current, err := s.store.Retention(r.Context())
		if err != nil {
			writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
			return
		}
		writeJSON(w, r.URL.Path, current)
	case http.MethodPut:
		if !caller.Admin {
			writeProblem(w, problem.NotPermitted(r.URL.Path,
				fmt.Sprintf("%s is not a member of %s, so may not change retention", caller.User, AdminGroup),
				"Ask an administrator to set the retention window."))
			return
		}
		var set store.RetentionSet
		if prob := decodeBody(w, r, &set); prob != nil {
			writeProblem(w, prob)
			return
		}
		if set.Empty() {
			writeProblem(w, problem.BadRequest(r.URL.Path, "the request names no signal",
				"Send one or more of traces, logs and metrics, as a duration such as 30d."))
			return
		}
		updated, err := s.store.SetRetention(r.Context(), set)
		if err != nil {
			writeProblem(w, problem.BadRequest(r.URL.Path, err.Error(),
				"Send a window matching ^[0-9]+(h|d)$, such as 720h or 30d."))
			return
		}
		writeJSON(w, r.URL.Path, updated)
	default:
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s %s is not part of the kitbashd API", r.Method, r.URL.Path),
			"Call GET to read the retention or PUT to change it."))
	}
}

// healthResponse is what /kitbash/v1/health answers. The listeners say which
// of the two receivers is bound, subscribers how many Processes the fan out is
// delivering to, mcpSessions how many MCP sessions Processes hold open, and
// probes how many Processes declare a health path kitbashd requests.
//
// lastBackup is when the nightly store backup last succeeded and backups how
// many copies are kept; lastBackupError is the reason the last attempt failed
// and is absent while backups are working, see PLAN.md section 4.7.
type healthResponse struct {
	Version       string    `json:"version"`
	Store         string    `json:"store"`
	UptimeSeconds int64     `json:"uptimeSeconds"`
	Listeners     listeners `json:"listeners"`
	Subscribers   int       `json:"subscribers"`
	MCPSessions   int       `json:"mcpSessions"`
	Probes        int       `json:"probes"`
	// Jobs is how many scheduled Processes the ticker holds, see schedule.go.
	Jobs int `json:"jobs"`
	// Routes is how many host names the reverse proxy serves, zero on a host
	// with no domain, see proxy.go.
	Routes          int    `json:"routes"`
	LastBackup      string `json:"lastBackup,omitempty"`
	Backups         int    `json:"backups"`
	LastBackupError string `json:"lastBackupError,omitempty"`
}

// listeners are the addresses kitbashd is serving on, empty for one that is
// not bound.
type listeners struct {
	Socket string `json:"socket,omitempty"`
	TCP    string `json:"tcp,omitempty"`
	// Proxy and ProxyTLS are the reverse proxy's, empty on a host with no
	// domain, which binds neither, see proxy.go.
	Proxy    string `json:"proxy,omitempty"`
	ProxyTLS string `json:"proxyTls,omitempty"`
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if _, prob := s.caller(r); prob != nil {
		writeProblem(w, prob)
		return
	}
	backup := s.backupState()
	writeJSON(w, r.URL.Path, healthResponse{
		Version:         s.version,
		Store:           s.store.Path(),
		UptimeSeconds:   int64(s.now().Sub(s.started) / time.Second),
		Listeners:       s.listeners(),
		Subscribers:     s.fanout.count(),
		MCPSessions:     s.mcpSessions.count(),
		Probes:          s.probes.count(),
		Jobs:            s.jobs.count(),
		Routes:          s.proxy.count(),
		LastBackup:      backup.LastBackup,
		Backups:         backup.Backups,
		LastBackupError: backup.LastBackupError,
	})
}

// decodeBody reads one JSON request body. An unknown field is refused rather
// than ignored: a client sending a field this version does not know is asking
// for something it will not get. A body over the limit is too-large, the same
// answer the OTLP paths give.
//
// The decoder's own message names Go types and struct fields, which mean
// nothing to an agent, so it goes to the server log and the caller is told
// which field it got wrong instead.
func decodeBody(w http.ResponseWriter, r *http.Request, into any) *problem.Problem {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, otlp.MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			return tooLarge(r.URL.Path)
		}
		logger.Printf("bad request body at %s: %v", r.URL.Path, err)
		return problem.BadRequest(r.URL.Path, detailOf(err),
			"Send arguments that match the tool's input schema in spec/mcp-surface.yaml.")
	}
	return nil
}

// detailOf turns a JSON decoding failure into something an agent can act on,
// without the Go type names encoding/json puts in its messages.
func detailOf(err error) string {
	var unknown *json.SyntaxError
	if errors.As(err, &unknown) {
		return "the request body is not valid JSON"
	}
	var wrongType *json.UnmarshalTypeError
	if errors.As(err, &wrongType) && wrongType.Field != "" {
		return fmt.Sprintf("the field %q has the wrong type", wrongType.Field)
	}
	// The only remaining case worth naming is an unknown field, whose message
	// is the field name in quotes and nothing about Go.
	if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return fmt.Sprintf("the field %s is not part of this request", field)
	}
	return "the request body is not the JSON this path expects"
}
