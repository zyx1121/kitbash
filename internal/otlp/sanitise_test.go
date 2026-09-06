package otlp

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func double(key string, value float64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_DoubleValue{DoubleValue: value}}}
}

func gauge(name string, value float64) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name,
		Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
			TimeUnixNano: 1_700_000_000_000_000_000,
			Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: value},
		}}}},
	}
}

func TestDecodeMetricsSkipsNonFinitePoints(t *testing.T) {
	req := &colmetrics.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{
					gauge("positive-infinity", math.Inf(1)),
					gauge("negative-infinity", math.Inf(-1)),
					gauge("not-a-number", math.NaN()),
					gauge("finite", 12.5),
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
	if len(metrics) != 1 {
		t.Fatalf("metrics = %d, want only the finite point", len(metrics))
	}
	if metrics[0].Name != "finite" || metrics[0].Value != 12.5 {
		t.Errorf("point = %+v", metrics[0])
	}
	// What survives must be renderable, which is the whole point of the skip.
	if _, err := json.Marshal(metrics[0].Value); err != nil {
		t.Errorf("the stored value is not JSON: %v", err)
	}
}

func TestDecodeDropsNonFiniteAttributes(t *testing.T) {
	req := &colmetrics.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				double("resource.ratio", math.Inf(1)),
				str(AttrUser, "alice"),
			}},
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{{
					Name: "calls",
					Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
						TimeUnixNano: 1_700_000_000_000_000_000,
						Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: 1},
						Attributes: []*commonpb.KeyValue{
							double("point.ratio", math.NaN()),
							double("point.finite", 0.5),
						},
					}}}},
				}},
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
	if len(metrics) != 1 {
		t.Fatalf("metrics = %d, want 1", len(metrics))
	}
	got := metrics[0]
	// The record is kept, without the attributes JSON cannot spell.
	if got.User != "alice" {
		t.Errorf("User = %q, want the record kept", got.User)
	}
	if _, present := got.Other["resource.ratio"]; present {
		t.Errorf("other still carries an infinite attribute: %v", got.Other)
	}
	if _, present := got.Other["point.ratio"]; present {
		t.Errorf("other still carries a NaN attribute: %v", got.Other)
	}
	if got.Other["point.finite"] != 0.5 {
		t.Errorf("other = %v, want the finite attribute kept", got.Other)
	}
	if _, err := json.Marshal(got.Other); err != nil {
		t.Errorf("the stored attributes are not JSON: %v", err)
	}
}

func TestDecodeTracesDropsNonFiniteAttributes(t *testing.T) {
	req := traceRequest()
	req.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes = append(
		req.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes,
		double("span.ratio", math.Inf(-1)))
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	spans, err := DecodeTraces(Protobuf, body)
	if err != nil {
		t.Fatalf("DecodeTraces: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want the record kept", len(spans))
	}
	if _, present := spans[0].Other["span.ratio"]; present {
		t.Errorf("other still carries an infinite attribute: %v", spans[0].Other)
	}
	if _, err := json.Marshal(spans[0].Other); err != nil {
		t.Errorf("the stored attributes are not JSON: %v", err)
	}
}

func TestDecodeTracesRefusesShortIdentifiers(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*tracepb.Span)
	}{
		{"short trace id", func(s *tracepb.Span) { s.TraceId = []byte{0x01, 0x02} }},
		{"long trace id", func(s *tracepb.Span) { s.TraceId = make([]byte, 24) }},
		{"no trace id", func(s *tracepb.Span) { s.TraceId = nil }},
		{"short span id", func(s *tracepb.Span) { s.SpanId = []byte{0x01} }},
		{"no span id", func(s *tracepb.Span) { s.SpanId = nil }},
		{"short parent span id", func(s *tracepb.Span) { s.ParentSpanId = []byte{0x01} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := traceRequest()
			tc.mutate(req.ResourceSpans[0].ScopeSpans[0].Spans[0])
			body, err := proto.Marshal(req)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if _, err := DecodeTraces(Protobuf, body); err == nil {
				t.Error("DecodeTraces accepted an identifier of the wrong width")
			}
		})
	}

	// An absent parent is how a root span says it has none.
	req := traceRequest()
	req.ResourceSpans[0].ScopeSpans[0].Spans[0].ParentSpanId = nil
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	spans, err := DecodeTraces(Protobuf, body)
	if err != nil {
		t.Fatalf("DecodeTraces refused a root span: %v", err)
	}
	if spans[0].ParentSpanID != "" {
		t.Errorf("ParentSpanID = %q, want empty", spans[0].ParentSpanID)
	}
}

func TestDecodeErrorsNameNoGoTypes(t *testing.T) {
	// A member reads these strings. Go type names tell an agent nothing it
	// can act on, so they must not appear.
	_, err := DecodeTraces(JSON, []byte(`{"nonsense":1}`))
	if err == nil {
		t.Fatal("DecodeTraces accepted an unknown field")
	}
	for _, leak := range []string{"v1.", "*v1", "ExportTraceServiceRequest"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q leaks the Go type name %q", err, leak)
		}
	}
}
