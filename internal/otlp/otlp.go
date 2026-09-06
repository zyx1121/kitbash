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

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

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
func unmarshal(f Format, body []byte, msg proto.Message) error {
	if f == JSON {
		body, err := hexIdentifiers(body)
		if err != nil {
			return err
		}
		if err := protojson.Unmarshal(body, msg); err != nil {
			return fmt.Errorf("otlp: body is not a JSON %T: %w", msg, err)
		}
		return nil
	}
	if err := proto.Unmarshal(body, msg); err != nil {
		return fmt.Errorf("otlp: body is not a protobuf %T: %w", msg, err)
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

// DecodeTraces turns an ExportTraceServiceRequest into spans.
func DecodeTraces(f Format, body []byte) ([]store.Span, error) {
	var req coltrace.ExportTraceServiceRequest
	if err := unmarshal(f, body, &req); err != nil {
		return nil, err
	}
	var spans []store.Span
	for _, rs := range req.GetResourceSpans() {
		resource := keyValues(rs.GetResource().GetAttributes())
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				spans = append(spans, span(s, resource))
			}
		}
	}
	return spans, nil
}

func span(s *tracepb.Span, resource map[string]any) store.Span {
	return store.Span{
		TraceID:       hex.EncodeToString(s.GetTraceId()),
		SpanID:        hex.EncodeToString(s.GetSpanId()),
		ParentSpanID:  hex.EncodeToString(s.GetParentSpanId()),
		Name:          s.GetName(),
		StartNS:       int64(s.GetStartTimeUnixNano()),
		EndNS:         int64(s.GetEndTimeUnixNano()),
		Status:        statusCode(s.GetStatus().GetCode()),
		StatusMessage: s.GetStatus().GetMessage(),
		Attributes:    attributes(resource, s.GetAttributes()),
	}
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
	if err := unmarshal(f, body, &req); err != nil {
		return nil, err
	}
	var logs []store.Log
	for _, rl := range req.GetResourceLogs() {
		resource := keyValues(rl.GetResource().GetAttributes())
		for _, sl := range rl.GetScopeLogs() {
			for _, l := range sl.GetLogRecords() {
				logs = append(logs, logRecord(l, resource))
			}
		}
	}
	return logs, nil
}

func logRecord(l *logspb.LogRecord, resource map[string]any) store.Log {
	// A producer that only stamps the observed time still gets a time.
	ts := l.GetTimeUnixNano()
	if ts == 0 {
		ts = l.GetObservedTimeUnixNano()
	}
	severity := l.GetSeverityText()
	if severity == "" && l.GetSeverityNumber() != logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED {
		severity = l.GetSeverityNumber().String()
	}
	return store.Log{
		TimeNS:     int64(ts),
		Severity:   severity,
		Body:       text(l.GetBody()),
		TraceID:    hex.EncodeToString(l.GetTraceId()),
		SpanID:     hex.EncodeToString(l.GetSpanId()),
		Attributes: attributes(resource, l.GetAttributes()),
	}
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
func DecodeMetrics(f Format, body []byte) ([]store.Metric, error) {
	var req colmetrics.ExportMetricsServiceRequest
	if err := unmarshal(f, body, &req); err != nil {
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
