package otlp

import (
	"testing"

	"google.golang.org/protobuf/proto"

	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// TestDecodeLiftsTheCaller is the attribute a session kitbashd opened for a
// Process records: it becomes a column of its own rather than one more entry
// among the other attributes, so a query for what a Process did is one filter.
func TestDecodeLiftsTheCaller(t *testing.T) {
	const caller = "01a07f95-df01-7073-add4-29e769a336f4"
	body, err := proto.Marshal(&coltrace.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: []*tracepb.Span{{
					TraceId: []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
						0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
					SpanId:            []byte{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18},
					Name:              "fs_list",
					StartTimeUnixNano: 1_700_000_000_000_000_000,
					EndTimeUnixNano:   1_700_000_000_500_000_000,
					Attributes: []*commonpb.KeyValue{
						str(AttrUser, "alice"),
						str(AttrCaller, caller),
					},
				}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	spans, err := DecodeTraces(Protobuf, body)
	if err != nil {
		t.Fatalf("DecodeTraces: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	if spans[0].Caller != caller {
		t.Errorf("Caller = %q, want %q", spans[0].Caller, caller)
	}
	if _, held := spans[0].Other[AttrCaller]; held {
		t.Errorf("%s stayed among the other attributes as well", AttrCaller)
	}
}

// TestDecodeDropsAWrongTypedCaller keeps a Process id that is not a string out
// of the column, the same rule every other lifted attribute follows.
func TestDecodeDropsAWrongTypedCaller(t *testing.T) {
	body, err := proto.Marshal(&coltrace.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: []*tracepb.Span{{
					TraceId: []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
						0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
					SpanId:            []byte{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18},
					Name:              "fs_list",
					StartTimeUnixNano: 1_700_000_000_000_000_000,
					EndTimeUnixNano:   1_700_000_000_500_000_000,
					Attributes: []*commonpb.KeyValue{{
						Key:   AttrCaller,
						Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 7}},
					}},
				}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	spans, err := DecodeTraces(Protobuf, body)
	if err != nil {
		t.Fatalf("DecodeTraces: %v", err)
	}
	if spans[0].Caller != "" {
		t.Errorf("Caller = %q, want the column left empty", spans[0].Caller)
	}
	if spans[0].Other[AttrCaller] != int64(7) {
		t.Errorf("the value went nowhere: other = %+v", spans[0].Other)
	}
}
