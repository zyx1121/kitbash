package otlp

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"

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

// EncodeTraces, EncodeLogs and EncodeMetrics render stored records back as an
// OTLP/HTTP JSON export request, which is what the fan out POSTs to a
// subscriber, see spec/kitbashd-api.yaml.
//
// The records travel as kitbashd stored them, stamped attributes included, so
// a subscriber reads the same values a query would return. Identifiers are hex
// and timestamps are strings of unix nanoseconds, as the OTLP JSON encoding
// requires. Everything a subscriber receives went through the store first: the
// fan out is a wake up, not a second protocol.
func EncodeTraces(spans []store.Span) ([]byte, error) {
	out := make([]*tracepb.Span, 0, len(spans))
	for _, sp := range spans {
		traceID, err := rawID("trace id", sp.TraceID)
		if err != nil {
			return nil, err
		}
		spanID, err := rawID("span id", sp.SpanID)
		if err != nil {
			return nil, err
		}
		parentID, err := rawID("parent span id", sp.ParentSpanID)
		if err != nil {
			return nil, err
		}
		out = append(out, &tracepb.Span{
			TraceId:           traceID,
			SpanId:            spanID,
			ParentSpanId:      parentID,
			Name:              sp.Name,
			StartTimeUnixNano: uint64(sp.StartNS),
			EndTimeUnixNano:   uint64(sp.EndNS),
			Status: &tracepb.Status{
				Code:    statusOf(sp.Status),
				Message: sp.StatusMessage,
			},
			Attributes: KeyValues(sp.Attributes),
		})
	}
	return encode(&coltrace.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: out}},
		}},
	})
}

// EncodeLogs renders stored log records as an OTLP/HTTP JSON export request.
func EncodeLogs(logs []store.Log) ([]byte, error) {
	out := make([]*logspb.LogRecord, 0, len(logs))
	for _, l := range logs {
		traceID, err := rawID("trace id", l.TraceID)
		if err != nil {
			return nil, err
		}
		spanID, err := rawID("span id", l.SpanID)
		if err != nil {
			return nil, err
		}
		out = append(out, &logspb.LogRecord{
			TimeUnixNano:         uint64(l.TimeNS),
			ObservedTimeUnixNano: uint64(l.TimeNS),
			SeverityText:         l.Severity,
			Body: &commonpb.AnyValue{
				Value: &commonpb.AnyValue_StringValue{StringValue: l.Body},
			},
			TraceId:    traceID,
			SpanId:     spanID,
			Attributes: KeyValues(l.Attributes),
		})
	}
	return encode(&collogs.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: out}},
		}},
	})
}

// EncodeMetrics renders stored metric points as an OTLP/HTTP JSON export
// request. The store holds one flattened value per point, so every point goes
// back out as a gauge: the buckets a histogram arrived with were dropped at
// the door, see DecodeMetrics.
func EncodeMetrics(metrics []store.Metric) ([]byte, error) {
	out := make([]*metricspb.Metric, 0, len(metrics))
	for _, m := range metrics {
		out = append(out, &metricspb.Metric{
			Name: m.Name,
			Unit: m.Unit,
			Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
				DataPoints: []*metricspb.NumberDataPoint{{
					TimeUnixNano: uint64(m.TimeNS),
					Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: m.Value},
					Attributes:   KeyValues(m.Attributes),
				}},
			}},
		})
	}
	return encode(&colmetrics.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: out}},
		}},
	})
}

// encode renders one export request as the JSON the OTLP specification
// defines, with the identifiers in hex.
func encode(msg proto.Message) ([]byte, error) {
	body, err := protojson.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("otlp: encode export request: %w", err)
	}
	return hexOut(body)
}

// KeyValues turns stored attributes back into the OTLP attribute list, the
// stamped ones included. It is the inverse of attributes, minus the merge:
// what came in as a resource attribute goes out on the record, because that is
// where the store kept it.
func KeyValues(a store.Attributes) []*commonpb.KeyValue {
	out := make([]*commonpb.KeyValue, 0, len(a.Other)+7)
	for _, kv := range []struct {
		key   string
		value string
	}{
		{AttrUser, a.User},
		{AttrPackage, a.Package},
		{AttrProcess, a.Process},
		{AttrPath, a.Path},
		{AttrTool, a.Tool},
		{AttrProducer, a.Producer},
	} {
		if kv.value == "" {
			continue
		}
		out = append(out, stringValue(kv.key, kv.value))
	}
	if a.Eval != nil {
		out = append(out, &commonpb.KeyValue{
			Key:   AttrEval,
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: *a.Eval}},
		})
	}
	// A record that is the cause of an internal problem says so on the way
	// out too, so a subscriber that receives one, which only an admin's
	// subscriber does, can tell it from an ordinary record.
	if a.Internal != nil {
		out = append(out, &commonpb.KeyValue{
			Key:   AttrInternal,
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: *a.Internal}},
		})
	}
	for key, value := range a.Other {
		converted := anyValue(value)
		if converted == nil {
			continue
		}
		out = append(out, &commonpb.KeyValue{Key: key, Value: converted})
	}
	return out
}

func stringValue(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   key,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}},
	}
}

// anyValue converts a stored attribute back into an OTLP value. The store
// holds what encoding/json decodes to, so a number is a float64 or a
// json.Number and nothing here has to guess at a wider type.
func anyValue(v any) *commonpb.AnyValue {
	switch value := v.(type) {
	case string:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}
	case bool:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: value}}
	case float64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: value}}
	case int64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: value}}
	case json.Number:
		if n, err := value.Int64(); err == nil {
			return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: n}}
		}
		f, err := value.Float64()
		if err != nil {
			return nil
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: f}}
	case []any:
		list := make([]*commonpb.AnyValue, 0, len(value))
		for _, item := range value {
			converted := anyValue(item)
			if converted == nil {
				continue
			}
			list = append(list, converted)
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{
			ArrayValue: &commonpb.ArrayValue{Values: list},
		}}
	case map[string]any:
		kvs := make([]*commonpb.KeyValue, 0, len(value))
		for key, item := range value {
			converted := anyValue(item)
			if converted == nil {
				continue
			}
			kvs = append(kvs, &commonpb.KeyValue{Key: key, Value: converted})
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{
			KvlistValue: &commonpb.KeyValueList{Values: kvs},
		}}
	default:
		// A nil, or anything else the store could not have written.
		return nil
	}
}

// statusOf maps the short status the store holds back to the OTLP code.
func statusOf(s string) tracepb.Status_StatusCode {
	switch s {
	case store.StatusOK:
		return tracepb.Status_STATUS_CODE_OK
	case store.StatusError:
		return tracepb.Status_STATUS_CODE_ERROR
	default:
		return tracepb.Status_STATUS_CODE_UNSET
	}
}

// rawID turns a stored hex identifier back into bytes. An empty one stays
// empty: a log record without a span is a normal thing to store.
func rawID(what, id string) ([]byte, error) {
	if id == "" {
		return nil, nil
	}
	raw, err := hex.DecodeString(id)
	if err != nil {
		return nil, fmt.Errorf("otlp: a stored record carries a %s that is not hex", what)
	}
	return raw, nil
}

// hexOut rewrites the identifiers protojson wrote as base64 back to the hex
// the OTLP JSON encoding requires. It is the outbound half of hexIdentifiers,
// and it walks the document by the same field names.
func hexOut(body []byte) ([]byte, error) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("otlp: encode export request: %w", err)
	}
	if !toHex(doc) {
		return body, nil
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("otlp: encode export request: %w", err)
	}
	return out, nil
}

// toHex converts every base64 identifier in place and reports whether it
// changed anything.
func toHex(node any) bool {
	changed := false
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			if want, isID := idFields[key]; isID {
				s, ok := child.(string)
				if !ok || len(s) == want {
					continue
				}
				raw, err := base64.StdEncoding.DecodeString(s)
				if err != nil || len(raw)*2 != want {
					continue
				}
				value[key] = hex.EncodeToString(raw)
				changed = true
				continue
			}
			if toHex(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range value {
			if toHex(child) {
				changed = true
			}
		}
	}
	return changed
}
