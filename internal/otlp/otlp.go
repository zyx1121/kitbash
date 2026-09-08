// Package otlp decodes OTLP/HTTP export requests into store records. It
// implements the receiver half of spec/kitbashd-api.yaml: the standard OTLP
// paths, protobuf or JSON, stored whole or refused whole.
//
// The wire types are the OpenTelemetry ones, so nothing about the protocol is
// invented here, see PLAN.md section 2.7. What this package adds is the
// flattening kitbash queries by: resource attributes merged into every record,
// then the six kitbash attributes lifted into typed columns.
package otlp

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	collogs "github.com/zyx1121/kitbash/internal/otlpproto/collector/logs/v1"
	colmetrics "github.com/zyx1121/kitbash/internal/otlpproto/collector/metrics/v1"
	coltrace "github.com/zyx1121/kitbash/internal/otlpproto/collector/trace/v1"
	commonpb "github.com/zyx1121/kitbash/internal/otlpproto/common/v1"
	logspb "github.com/zyx1121/kitbash/internal/otlpproto/logs/v1"
	metricspb "github.com/zyx1121/kitbash/internal/otlpproto/metrics/v1"
	tracepb "github.com/zyx1121/kitbash/internal/otlpproto/trace/v1"

	"github.com/zyx1121/kitbash/internal/store"
)

// MaxBodyBytes is the largest export request kitbashd accepts, see
// spec/kitbashd-api.yaml. A larger body is refused whole.
const MaxBodyBytes = 4 << 20

// Content types the receiver speaks.
const (
	ContentTypeProtobuf = "application/x-protobuf"
	ContentTypeJSON     = "application/json"
)

// Format is the encoding of one export request. The response goes back in the
// same format the request arrived in.
type Format int

// The two formats spec/kitbashd-api.yaml lists.
const (
	Protobuf Format = iota
	JSON
)

// ContentType is the media type of this format.
func (f Format) ContentType() string {
	if f == JSON {
		return ContentTypeJSON
	}
	return ContentTypeProtobuf
}

// ParseFormat reads the Content-Type header of an export request. Parameters
// such as a charset are ignored; anything else is refused, because guessing at
// an unknown encoding would store a request the producer did not send.
func ParseFormat(contentType string) (Format, error) {
	base := contentType
	for i := 0; i < len(base); i++ {
		if base[i] == ';' {
			base = base[:i]
			break
		}
	}
	switch trimSpace(base) {
	case ContentTypeProtobuf:
		return Protobuf, nil
	case ContentTypeJSON:
		return JSON, nil
	case "":
		return Protobuf, errors.New("otlp: no Content-Type; send application/x-protobuf or application/json")
	default:
		return Protobuf, fmt.Errorf("otlp: unsupported Content-Type %q; send application/x-protobuf or application/json", contentType)
	}
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// unmarshal reads one request message in either format. protojson is strict
// about unknown fields on purpose: a body kitbashd does not understand is a
// bad request, not a silently truncated record.
//
// The signal names what failed to decode. A Go type name never appears in an
// error a member reads: it says nothing the agent can act on.
func unmarshal(f Format, body []byte, msg proto.Message, signal string) error {
	if f == JSON {
		body, err := hexIdentifiers(body)
		if err != nil {
			return err
		}
		if err := protojson.Unmarshal(body, msg); err != nil {
			return fmt.Errorf("otlp: body is not a JSON %s export request: %w", signal, err)
		}
		return nil
	}
	if err := proto.Unmarshal(body, msg); err != nil {
		return fmt.Errorf("otlp: body is not a protobuf %s export request: %w", signal, err)
	}
	return nil
}

// marshal renders a response message in the format the request used.
func marshal(f Format, msg proto.Message) ([]byte, error) {
	if f == JSON {
		b, err := protojson.Marshal(msg)
		if err != nil {
			return nil, fmt.Errorf("otlp: encode response: %w", err)
		}
		return b, nil
	}
	b, err := proto.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("otlp: encode response: %w", err)
	}
	return b, nil
}

// DecodeTraces turns an ExportTraceServiceRequest into spans. A span whose
// identifiers are not the width the spanRecord schema promises makes the whole
// request a bad request: the surface publishes those identifiers with a
// pattern, so storing a short one would break the schema an agent matches
// against.
func DecodeTraces(f Format, body []byte) ([]store.Span, error) {
	var req coltrace.ExportTraceServiceRequest
	if err := unmarshal(f, body, &req, "traces"); err != nil {
		return nil, err
	}
	var spans []store.Span
	for _, rs := range req.GetResourceSpans() {
		resource := keyValues(rs.GetResource().GetAttributes())
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				decoded, err := span(s, resource)
				if err != nil {
					return nil, err
				}
				spans = append(spans, decoded)
			}
		}
	}
	return spans, nil
}

func span(s *tracepb.Span, resource map[string]any) (store.Span, error) {
	traceID, err := identifier("trace id", s.GetTraceId(), traceIDHexLen, false)
	if err != nil {
		return store.Span{}, err
	}
	spanID, err := identifier("span id", s.GetSpanId(), spanIDHexLen, false)
	if err != nil {
		return store.Span{}, err
	}
	parentID, err := identifier("parent span id", s.GetParentSpanId(), spanIDHexLen, true)
	if err != nil {
		return store.Span{}, err
	}
	return store.Span{
		TraceID:       traceID,
		SpanID:        spanID,
		ParentSpanID:  parentID,
		Name:          s.GetName(),
		StartNS:       int64(s.GetStartTimeUnixNano()),
		EndNS:         int64(s.GetEndTimeUnixNano()),
		Status:        statusCode(s.GetStatus().GetCode()),
		StatusMessage: s.GetStatus().GetMessage(),
		Attributes:    attributes(resource, s.GetAttributes()),
	}, nil
}

// identifier renders a trace or span id as hex of exactly the width the query
// surface publishes. Anything else is refused rather than padded: a producer
// that sends half an identifier is not sending the one it means.
func identifier(what string, raw []byte, hexLen int, optional bool) (string, error) {
	if len(raw) == 0 {
		if optional {
			return "", nil
		}
		return "", fmt.Errorf("otlp: a record carries no %s; a %s is %d hex characters", what, what, hexLen)
	}
	encoded := hex.EncodeToString(raw)
	if len(encoded) != hexLen {
		return "", fmt.Errorf("otlp: a record carries a %s of %d hex characters; a %s is %d",
			what, len(encoded), what, hexLen)
	}
	return encoded, nil
}

// statusCode maps the OTLP status to the three values the query surface
// publishes, see the spanRecord schema in spec/mcp-surface.yaml.
func statusCode(code tracepb.Status_StatusCode) string {
	switch code {
	case tracepb.Status_STATUS_CODE_OK:
		return store.StatusOK
	case tracepb.Status_STATUS_CODE_ERROR:
		return store.StatusError
	default:
		return store.StatusUnset
	}
}

// DecodeLogs turns an ExportLogsServiceRequest into log records.
func DecodeLogs(f Format, body []byte) ([]store.Log, error) {
	var req collogs.ExportLogsServiceRequest
	if err := unmarshal(f, body, &req, "logs"); err != nil {
		return nil, err
	}
	var logs []store.Log
	for _, rl := range req.GetResourceLogs() {
		resource := keyValues(rl.GetResource().GetAttributes())
		for _, sl := range rl.GetScopeLogs() {
			for _, l := range sl.GetLogRecords() {
				decoded, err := logRecord(l, resource)
				if err != nil {
					return nil, err
				}
				logs = append(logs, decoded)
			}
		}
	}
	return logs, nil
}

// logRecord decodes one log record. Its identifiers are optional, because a
// log record that belongs to no span is a normal thing to send, but one that
// is present is held to the same width as a span's.
func logRecord(l *logspb.LogRecord, resource map[string]any) (store.Log, error) {
	// A producer that only stamps the observed time still gets a time.
	ts := l.GetTimeUnixNano()
	if ts == 0 {
		ts = l.GetObservedTimeUnixNano()
	}
	severity := l.GetSeverityText()
	if severity == "" && l.GetSeverityNumber() != logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED {
		severity = l.GetSeverityNumber().String()
	}
	traceID, err := identifier("trace id", l.GetTraceId(), traceIDHexLen, true)
	if err != nil {
		return store.Log{}, err
	}
	spanID, err := identifier("span id", l.GetSpanId(), spanIDHexLen, true)
	if err != nil {
		return store.Log{}, err
	}
	return store.Log{
		TimeNS:     int64(ts),
		Severity:   severity,
		Body:       text(l.GetBody()),
		TraceID:    traceID,
		SpanID:     spanID,
		Attributes: attributes(resource, l.GetAttributes()),
	}, nil
}

// DecodeMetrics turns an ExportMetricsServiceRequest into metric points.
//
// Gauges and sums flatten one data point to one record. Histograms,
// exponential histograms and summaries have no single value, so kitbash stores
// their sum when the point carries one and their count otherwise, and drops
// the buckets. That is a deliberate simplification: the store answers "what
// happened and how much", not a full metrics backend, and no producer emits
// metrics before M4, see PLAN.md section 5.5. An observability kit that needs
// buckets subscribes to the fan out instead.
//
// A data point whose value is not finite is skipped rather than stored. JSON
// has no NaN and no infinity, so such a point would be unreadable the moment
// anyone queried the window it landed in, and one producer sending it would
// take the answer away from every other member. The rest of the request is
// stored: a broken point is the producer's bug, not a reason to lose the
// batch it travelled with.
func DecodeMetrics(f Format, body []byte) ([]store.Metric, error) {
	var req colmetrics.ExportMetricsServiceRequest
	if err := unmarshal(f, body, &req, "metrics"); err != nil {
		return nil, err
	}
	var metrics []store.Metric
	for _, rm := range req.GetResourceMetrics() {
		resource := keyValues(rm.GetResource().GetAttributes())
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				metrics = append(metrics, points(m, resource)...)
			}
		}
	}
	return metrics, nil
}

func points(m *metricspb.Metric, resource map[string]any) []store.Metric {
	var out []store.Metric
	point := func(ts uint64, value float64, attrs []*commonpb.KeyValue) {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return
		}
		out = append(out, store.Metric{
			TimeNS:     int64(ts),
			Name:       m.GetName(),
			Value:      value,
			Unit:       m.GetUnit(),
			Attributes: attributes(resource, attrs),
		})
	}
	switch data := m.GetData().(type) {
	case *metricspb.Metric_Gauge:
		for _, p := range data.Gauge.GetDataPoints() {
			point(p.GetTimeUnixNano(), number(p), p.GetAttributes())
		}
	case *metricspb.Metric_Sum:
		for _, p := range data.Sum.GetDataPoints() {
			point(p.GetTimeUnixNano(), number(p), p.GetAttributes())
		}
	case *metricspb.Metric_Histogram:
		for _, p := range data.Histogram.GetDataPoints() {
			value := float64(p.GetCount())
			if p.Sum != nil {
				value = p.GetSum()
			}
			point(p.GetTimeUnixNano(), value, p.GetAttributes())
		}
	case *metricspb.Metric_ExponentialHistogram:
		for _, p := range data.ExponentialHistogram.GetDataPoints() {
			value := float64(p.GetCount())
			if p.Sum != nil {
				value = p.GetSum()
			}
			point(p.GetTimeUnixNano(), value, p.GetAttributes())
		}
	case *metricspb.Metric_Summary:
		for _, p := range data.Summary.GetDataPoints() {
			point(p.GetTimeUnixNano(), p.GetSum(), p.GetAttributes())
		}
	}
	return out
}

// number reads the value of a data point whichever way the producer typed it.
func number(p *metricspb.NumberDataPoint) float64 {
	switch v := p.GetValue().(type) {
	case *metricspb.NumberDataPoint_AsDouble:
		return v.AsDouble
	case *metricspb.NumberDataPoint_AsInt:
		return float64(v.AsInt)
	default:
		return 0
	}
}

// TracesResponse, LogsResponse and MetricsResponse are the empty success
// bodies. Partial success is not used: a request kitbashd answers 200 is
// stored whole, see spec/kitbashd-api.yaml.
func TracesResponse(f Format) ([]byte, error) {
	return marshal(f, &coltrace.ExportTraceServiceResponse{})
}

// LogsResponse is the empty success body of the logs path.
func LogsResponse(f Format) ([]byte, error) {
	return marshal(f, &collogs.ExportLogsServiceResponse{})
}

// MetricsResponse is the empty success body of the metrics path.
func MetricsResponse(f Format) ([]byte, error) {
	return marshal(f, &colmetrics.ExportMetricsServiceResponse{})
}
