package otlp

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	collogs "github.com/zyx1121/kitbash/internal/otlpproto/collector/logs/v1"
	colmetrics "github.com/zyx1121/kitbash/internal/otlpproto/collector/metrics/v1"
	coltrace "github.com/zyx1121/kitbash/internal/otlpproto/collector/trace/v1"
	commonpb "github.com/zyx1121/kitbash/internal/otlpproto/common/v1"
	logspb "github.com/zyx1121/kitbash/internal/otlpproto/logs/v1"
	metricspb "github.com/zyx1121/kitbash/internal/otlpproto/metrics/v1"
	resourcepb "github.com/zyx1121/kitbash/internal/otlpproto/resource/v1"
	tracepb "github.com/zyx1121/kitbash/internal/otlpproto/trace/v1"

	"github.com/zyx1121/kitbash/internal/store"
)

func str(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

func boolean(key string, value bool) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_BoolValue{BoolValue: value}}}
}

// traceRequest is one span carrying resource attributes the record overrides.
func traceRequest() *coltrace.ExportTraceServiceRequest {
	return &coltrace.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				str(AttrUser, "resource-user"),
				str(AttrPackage, "/org/ffmpeg"),
				str("service.name", "kitbash-mcp"),
			}},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: []*tracepb.Span{{
					TraceId:           []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
					SpanId:            []byte{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18},
					ParentSpanId:      []byte{0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28},
					Name:              "fs_list",
					StartTimeUnixNano: 1_700_000_000_000_000_000,
					EndTimeUnixNano:   1_700_000_000_500_000_000,
					Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "no such folder"},
					Attributes: []*commonpb.KeyValue{
						str(AttrUser, "record-user"),
						str(AttrPath, "/org/handbook"),
						str(AttrTool, "fs_list"),
						boolean(AttrEval, true),
						{Key: "http.status", Value: &commonpb.AnyValue{
							Value: &commonpb.AnyValue_IntValue{IntValue: 404}}},
					},
				}},
			}},
		}},
	}
}

func TestDecodeTraces(t *testing.T) {
	req := traceRequest()
	pb, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal protobuf: %v", err)
	}
	js, err := protojson.Marshal(req)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}

	cases := []struct {
		name   string
		format Format
		body   []byte
	}{
		{"protobuf", Protobuf, pb},
		{"json", JSON, js},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spans, err := DecodeTraces(tc.format, tc.body)
			if err != nil {
				t.Fatalf("DecodeTraces: %v", err)
			}
			if len(spans) != 1 {
				t.Fatalf("spans = %d, want 1", len(spans))
			}
			got := spans[0]
			if got.TraceID != "0102030405060708090a0b0c0d0e0f10" {
				t.Errorf("TraceID = %q", got.TraceID)
			}
			if got.SpanID != "1112131415161718" || got.ParentSpanID != "2122232425262728" {
				t.Errorf("span ids = %q %q", got.SpanID, got.ParentSpanID)
			}
			if got.Name != "fs_list" {
				t.Errorf("Name = %q", got.Name)
			}
			if got.StartNS != 1_700_000_000_000_000_000 || got.EndNS != 1_700_000_000_500_000_000 {
				t.Errorf("times = %d %d", got.StartNS, got.EndNS)
			}
			if got.Status != store.StatusError || got.StatusMessage != "no such folder" {
				t.Errorf("status = %q %q", got.Status, got.StatusMessage)
			}
			// The record attribute wins over the resource attribute.
			if got.User != "record-user" {
				t.Errorf("User = %q, want the record attribute to win", got.User)
			}
			// A resource attribute the record does not carry is merged in.
			if got.Package != "/org/ffmpeg" {
				t.Errorf("Package = %q, want the resource attribute", got.Package)
			}
			if got.Path != "/org/handbook" || got.Tool != "fs_list" {
				t.Errorf("lifted attributes = %+v", got.Attributes)
			}
			if got.Eval == nil || !*got.Eval {
				t.Errorf("Eval = %v, want true", got.Eval)
			}
			// Everything else stays in other, by its wire name.
			if got.Other["service.name"] != "kitbash-mcp" {
				t.Errorf("other = %v, want service.name", got.Other)
			}
			if _, lifted := got.Other[AttrUser]; lifted {
				t.Errorf("other still carries %s", AttrUser)
			}
		})
	}
}

func TestDecodeTracesStatus(t *testing.T) {
	cases := []struct {
		code tracepb.Status_StatusCode
		want string
	}{
		{tracepb.Status_STATUS_CODE_OK, store.StatusOK},
		{tracepb.Status_STATUS_CODE_ERROR, store.StatusError},
		{tracepb.Status_STATUS_CODE_UNSET, store.StatusUnset},
	}
	for _, tc := range cases {
		req := traceRequest()
		req.ResourceSpans[0].ScopeSpans[0].Spans[0].Status = &tracepb.Status{Code: tc.code}
		body, err := proto.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		spans, err := DecodeTraces(Protobuf, body)
		if err != nil {
			t.Fatalf("DecodeTraces: %v", err)
		}
		if spans[0].Status != tc.want {
			t.Errorf("status of %v = %q, want %q", tc.code, spans[0].Status, tc.want)
		}
	}

	// A span with no status at all is unset, not an error.
	req := traceRequest()
	req.ResourceSpans[0].ScopeSpans[0].Spans[0].Status = nil
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	spans, err := DecodeTraces(Protobuf, body)
	if err != nil {
		t.Fatalf("DecodeTraces: %v", err)
	}
	if spans[0].Status != store.StatusUnset {
		t.Errorf("status of a span without one = %q", spans[0].Status)
	}
}

func TestDecodeLogs(t *testing.T) {
	req := &collogs.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str(AttrPackage, "/home/bob/tool")}},
			ScopeLogs: []*logspb.ScopeLogs{{
				LogRecords: []*logspb.LogRecord{
					{
						TimeUnixNano: 1_700_000_000_000_000_000,
						SeverityText: "ERROR",
						Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "build failed"}},
						TraceId:      []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
						SpanId:       []byte{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18},
						Attributes:   []*commonpb.KeyValue{str(AttrTool, "pkg_build")},
					},
					{
						ObservedTimeUnixNano: 1_700_000_001_000_000_000,
						SeverityNumber:       logspb.SeverityNumber_SEVERITY_NUMBER_INFO,
						Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 7}},
					},
				},
			}},
		}},
	}
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	logs, err := DecodeLogs(Protobuf, body)
	if err != nil {
		t.Fatalf("DecodeLogs: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("logs = %d, want 2", len(logs))
	}
	if logs[0].Severity != "ERROR" || logs[0].Body != "build failed" {
		t.Errorf("first record = %+v", logs[0])
	}
	if logs[0].Package != "/home/bob/tool" || logs[0].Tool != "pkg_build" {
		t.Errorf("first record attributes = %+v", logs[0].Attributes)
	}
	if logs[0].TraceID != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("TraceID = %q", logs[0].TraceID)
	}
	// A record with only an observed time and a severity number still lands.
	if logs[1].TimeNS != 1_700_000_001_000_000_000 {
		t.Errorf("second time = %d", logs[1].TimeNS)
	}
	if logs[1].Severity != "SEVERITY_NUMBER_INFO" {
		t.Errorf("second severity = %q", logs[1].Severity)
	}
	if logs[1].Body != "7" {
		t.Errorf("second body = %q, want the non string body as JSON", logs[1].Body)
	}
}

func TestDecodeMetrics(t *testing.T) {
	req := &colmetrics.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str(AttrUser, "alice")}},
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{
					{
						Name: "calls",
						Unit: "1",
						Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: []*metricspb.NumberDataPoint{{
							TimeUnixNano: 1_700_000_000_000_000_000,
							Value:        &metricspb.NumberDataPoint_AsInt{AsInt: 3},
							Attributes:   []*commonpb.KeyValue{str(AttrTool, "fs_read")},
						}}}},
					},
					{
						Name: "latency",
						Unit: "ms",
						Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
							TimeUnixNano: 1_700_000_001_000_000_000,
							Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: 12.5},
						}}}},
					},
					{
						Name: "sizes",
						Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{DataPoints: []*metricspb.HistogramDataPoint{{
							TimeUnixNano: 1_700_000_002_000_000_000,
							Count:        4,
							Sum:          proto.Float64(40),
						}}}},
					},
				},
			}},
		}},
	}
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	metrics, err := DecodeMetrics(Protobuf, body)
	if err != nil {
		t.Fatalf("DecodeMetrics: %v", err)
	}
	if len(metrics) != 3 {
		t.Fatalf("metrics = %d, want 3", len(metrics))
	}
	if metrics[0].Name != "calls" || metrics[0].Value != 3 || metrics[0].Unit != "1" {
		t.Errorf("sum point = %+v", metrics[0])
	}
	if metrics[0].User != "alice" || metrics[0].Tool != "fs_read" {
		t.Errorf("sum point attributes = %+v", metrics[0].Attributes)
	}
	if metrics[1].Value != 12.5 {
		t.Errorf("gauge point = %+v", metrics[1])
	}
	// A histogram is stored as its sum, see the DecodeMetrics comment.
	if metrics[2].Value != 40 {
		t.Errorf("histogram point = %+v, want the sum", metrics[2])
	}
}

func TestDecodeRefusesGarbage(t *testing.T) {
	if _, err := DecodeTraces(JSON, []byte("not json at all")); err == nil {
		t.Error("DecodeTraces accepted a body that is not JSON")
	}
	if _, err := DecodeTraces(JSON, []byte(`{"nonsense":1}`)); err == nil {
		t.Error("DecodeTraces accepted JSON with an unknown field")
	}
	if _, err := DecodeTraces(Protobuf, []byte{0xff, 0xff, 0xff, 0xff}); err == nil {
		t.Error("DecodeTraces accepted a body that is not protobuf")
	}
	if _, err := DecodeLogs(JSON, []byte("{")); err == nil {
		t.Error("DecodeLogs accepted a truncated body")
	}
	if _, err := DecodeMetrics(JSON, []byte("[]")); err == nil {
		t.Error("DecodeMetrics accepted an array")
	}
}

func TestParseFormat(t *testing.T) {
	cases := []struct {
		header string
		want   Format
		ok     bool
	}{
		{ContentTypeProtobuf, Protobuf, true},
		{ContentTypeJSON, JSON, true},
		{"application/json; charset=utf-8", JSON, true},
		{" application/x-protobuf ", Protobuf, true},
		{"text/plain", Protobuf, false},
		{"", Protobuf, false},
	}
	for _, tc := range cases {
		got, err := ParseFormat(tc.header)
		if tc.ok != (err == nil) {
			t.Errorf("ParseFormat(%q) error = %v, want ok %v", tc.header, err, tc.ok)
			continue
		}
		if tc.ok && got != tc.want {
			t.Errorf("ParseFormat(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

func TestResponses(t *testing.T) {
	for _, f := range []Format{Protobuf, JSON} {
		traces, err := TracesResponse(f)
		if err != nil {
			t.Fatalf("TracesResponse: %v", err)
		}
		logs, err := LogsResponse(f)
		if err != nil {
			t.Fatalf("LogsResponse: %v", err)
		}
		metrics, err := MetricsResponse(f)
		if err != nil {
			t.Fatalf("MetricsResponse: %v", err)
		}
		for _, body := range [][]byte{traces, logs, metrics} {
			switch f {
			case JSON:
				if !bytes.Equal(body, []byte("{}")) {
					t.Errorf("JSON response = %q, want an empty object", body)
				}
			case Protobuf:
				if len(body) != 0 {
					t.Errorf("protobuf response = %q, want an empty message", body)
				}
			}
		}
	}
}
