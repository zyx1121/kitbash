package otlp

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/zyx1121/kitbash/internal/store"
)

func TestDecodeJSONHexIdentifiers(t *testing.T) {
	// The OTLP specification spells identifiers in hex in JSON, and a
	// timestamp is unix nanoseconds, which no float64 could hold.
	body := []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{
		"traceId":"0102030405060708090a0b0c0d0e0f10",
		"spanId":"1112131415161718",
		"parentSpanId":"2122232425262728",
		"name":"fs_list",
		"startTimeUnixNano":"1700000000123456789",
		"endTimeUnixNano":1700000000223456789,
		"status":{"code":2,"message":"no such folder"}}]}]}]}`)
	spans, err := DecodeTraces(JSON, body)
	if err != nil {
		t.Fatalf("DecodeTraces: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	got := spans[0]
	if got.TraceID != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("TraceID = %q, want the hex identifier as sent", got.TraceID)
	}
	if got.SpanID != "1112131415161718" || got.ParentSpanID != "2122232425262728" {
		t.Errorf("span ids = %q %q", got.SpanID, got.ParentSpanID)
	}
	if got.StartNS != 1_700_000_000_123_456_789 {
		t.Errorf("StartNS = %d, want the nanoseconds as sent", got.StartNS)
	}
	if got.EndNS != 1_700_000_000_223_456_789 {
		t.Errorf("EndNS = %d, want a numeric timestamp kept whole", got.EndNS)
	}
	if got.Status != store.StatusError {
		t.Errorf("Status = %q", got.Status)
	}
}

func TestDecodeJSONBase64Identifiers(t *testing.T) {
	// protojson based exporters send base64 instead. Both are accepted,
	// because the two encodings have different lengths and never collide.
	body, err := protojson.Marshal(traceRequest())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(body, []byte(`"traceId":"AQIDBAUGBwgJCgsMDQ4PEA=="`)) {
		t.Fatalf("protojson no longer encodes identifiers as base64: %s", body)
	}
	spans, err := DecodeTraces(JSON, body)
	if err != nil {
		t.Fatalf("DecodeTraces: %v", err)
	}
	if spans[0].TraceID != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("TraceID = %q", spans[0].TraceID)
	}
}

func TestHexIdentifiersLeavesOtherBodiesAlone(t *testing.T) {
	body := []byte(`{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"hello"}}]}]}]}`)
	out, err := hexIdentifiers(body)
	if err != nil {
		t.Fatalf("hexIdentifiers: %v", err)
	}
	if !bytes.Equal(out, body) {
		t.Errorf("body was rewritten to %s", out)
	}
	if _, err := hexIdentifiers([]byte("not json")); err == nil {
		t.Error("hexIdentifiers accepted a body that is not JSON")
	}
}
