package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/zyx1121/kitbash/internal/otlp"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
)

// writeProblem is the only way an error leaves this package. Nothing else is
// ever written to a client that is not an OTLP response or an API body.
func writeProblem(w http.ResponseWriter, p *problem.Problem) {
	w.Header().Set("Content-Type", ProblemContentType)
	w.WriteHeader(p.Status)
	fmt.Fprintln(w, p.JSON())
}

// writeJSON renders one API response.
func writeJSON(w http.ResponseWriter, instance string, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		writeProblem(w, problem.Internal(instance, fmt.Sprintf("encode response: %v", err), ""))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(b)
}

// exportTraces, exportLogs and exportMetrics are the three OTLP paths. They
// differ only in what they decode, so they share everything else.
func (s *Server) exportTraces(w http.ResponseWriter, r *http.Request) {
	s.export(w, r, func(f otlp.Format, body []byte, caller Caller) (store.Export, []byte, error) {
		spans, err := otlp.DecodeTraces(f, body)
		if err != nil {
			return store.Export{}, nil, err
		}
		for i := range spans {
			spans[i].User = caller.User
		}
		res, err := otlp.TracesResponse(f)
		return store.Export{Spans: spans}, res, err
	})
}

func (s *Server) exportLogs(w http.ResponseWriter, r *http.Request) {
	s.export(w, r, func(f otlp.Format, body []byte, caller Caller) (store.Export, []byte, error) {
		logs, err := otlp.DecodeLogs(f, body)
		if err != nil {
			return store.Export{}, nil, err
		}
		for i := range logs {
			logs[i].User = caller.User
		}
		res, err := otlp.LogsResponse(f)
		return store.Export{Logs: logs}, res, err
	})
}

func (s *Server) exportMetrics(w http.ResponseWriter, r *http.Request) {
	s.export(w, r, func(f otlp.Format, body []byte, caller Caller) (store.Export, []byte, error) {
		metrics, err := otlp.DecodeMetrics(f, body)
		if err != nil {
			return store.Export{}, nil, err
		}
		for i := range metrics {
			metrics[i].User = caller.User
		}
		res, err := otlp.MetricsResponse(f)
		return store.Export{Metrics: metrics}, res, err
	})
}

// decoder turns one request body into records to store and the response body
// that acknowledges them.
type decoder func(f otlp.Format, body []byte, caller Caller) (store.Export, []byte, error)

// export is the shared OTLP path: identity, size limit, decode, stamp, store.
// kitbash.user is taken from the peer on every record, replacing whatever the
// producer sent, see PLAN.md section 2.4.
func (s *Server) export(w http.ResponseWriter, r *http.Request, decode decoder) {
	caller, prob := s.caller(r)
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

	export, response, err := decode(format, body, caller)
	if err != nil {
		writeProblem(w, problem.BadRequest(r.URL.Path, err.Error(),
			"Send an OTLP export request the OpenTelemetry protocol defines."))
		return
	}
	if err := s.store.Insert(r.Context(), export); err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}

	w.Header().Set("Content-Type", format.ContentType())
	w.WriteHeader(http.StatusOK)
	w.Write(response)
}

func tooLarge(instance string) *problem.Problem {
	return problem.TooLarge(instance,
		fmt.Sprintf("an export request may carry at most %d bytes", otlp.MaxBodyBytes),
		"Export in smaller batches.")
}

// queryRequest is the tel_query input of spec/mcp-surface.yaml.
type queryRequest struct {
	Signal  string `json:"signal"`
	User    string `json:"user,omitempty"`
	Package string `json:"package,omitempty"`
	Process string `json:"process,omitempty"`
	Path    string `json:"path,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Eval    *bool  `json:"eval,omitempty"`
	Since   string `json:"since,omitempty"`
	Until   string `json:"until,omitempty"`
	Limit   int    `json:"limit,omitempty"`
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
	if prob := decodeBody(r, &req); prob != nil {
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
		User:    req.User,
		Package: req.Package,
		Process: req.Process,
		Path:    req.Path,
		Tool:    req.Tool,
		Eval:    req.Eval,
		Since:   now.Add(-QueryWindow),
		Until:   now,
		Limit:   req.Limit,
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
		if prob := decodeBody(r, &set); prob != nil {
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

// healthResponse is what /kitbash/v1/health answers.
type healthResponse struct {
	Version       string `json:"version"`
	Store         string `json:"store"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if _, prob := s.caller(r); prob != nil {
		writeProblem(w, prob)
		return
	}
	writeJSON(w, r.URL.Path, healthResponse{
		Version:       s.version,
		Store:         s.store.Path(),
		UptimeSeconds: int64(s.now().Sub(s.started) / time.Second),
	})
}

// decodeBody reads one JSON request body. An unknown field is refused rather
// than ignored: a client sending a field this version does not know is asking
// for something it will not get.
func decodeBody(r *http.Request, into any) *problem.Problem {
	dec := json.NewDecoder(io.LimitReader(r.Body, otlp.MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return problem.BadRequest(r.URL.Path, fmt.Sprintf("the request body is not the JSON this path expects: %v", err),
			"Send arguments that match the tool's input schema in spec/mcp-surface.yaml.")
	}
	return nil
}
