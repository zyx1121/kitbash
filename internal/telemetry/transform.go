package telemetry

import (
	"math"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	commonpb "github.com/zyx1121/kitbash/internal/otlpproto/common/v1"
	logspb "github.com/zyx1121/kitbash/internal/otlpproto/logs/v1"
	resourcepb "github.com/zyx1121/kitbash/internal/otlpproto/resource/v1"
	tracepb "github.com/zyx1121/kitbash/internal/otlpproto/trace/v1"
)

// This file turns what the OpenTelemetry SDK hands an exporter into the OTLP
// messages of internal/otlpproto. It is the transformation the SDK's own OTLP
// exporters do, written out here because theirs arrives with the gRPC stack
// attached, see PLAN.md section 2.7 and internal/otlpproto/README.md. Nothing
// is dropped on the way: a record that would have gone out under the SDK's
// exporter goes out with at least the same fields under this one.
//
// There is one deliberate addition. A link carries the trace state of the span
// it points at, which the protocol has a field for and the SDK's exporter
// leaves empty. Sending it costs nothing and loses nothing.

// resourceSpans groups a batch of finished spans by resource and then by
// instrumentation scope, which is the shape an OTLP export request has.
//
// One session has one resource and one scope, so the grouping almost always
// produces a single pair. It is done properly anyway: the batcher hands over
// whatever the tracer provider recorded, and a second scope would otherwise
// have its spans filed under the first one's name.
func resourceSpans(spans []sdktrace.ReadOnlySpan) []*tracepb.ResourceSpans {
	if len(spans) == 0 {
		return nil
	}
	type scopeKey struct {
		res   attribute.Distinct
		scope instrumentation.Scope
	}
	byResource := make(map[attribute.Distinct]*tracepb.ResourceSpans)
	byScope := make(map[scopeKey]*tracepb.ScopeSpans)
	var order []*tracepb.ResourceSpans

	for _, s := range spans {
		if s == nil {
			continue
		}
		res := s.Resource()
		resKey := res.Equivalent()
		key := scopeKey{res: resKey, scope: s.InstrumentationScope()}
		scoped, known := byScope[key]
		if !known {
			scoped = &tracepb.ScopeSpans{
				Scope:     scope(s.InstrumentationScope()),
				SchemaUrl: s.InstrumentationScope().SchemaURL,
			}
			byScope[key] = scoped
		}
		scoped.Spans = append(scoped.Spans, protoSpan(s))

		grouped, seen := byResource[resKey]
		if !seen {
			grouped = &tracepb.ResourceSpans{
				Resource:  resourceOf(res),
				SchemaUrl: res.SchemaURL(),
			}
			byResource[resKey] = grouped
			order = append(order, grouped)
		}
		if !known {
			grouped.ScopeSpans = append(grouped.ScopeSpans, scoped)
		}
	}
	return order
}

// protoSpan transforms one finished span.
func protoSpan(s sdktrace.ReadOnlySpan) *tracepb.Span {
	context := s.SpanContext()
	traceID := context.TraceID()
	spanID := context.SpanID()
	status := s.Status()
	out := &tracepb.Span{
		TraceId:                traceID[:],
		SpanId:                 spanID[:],
		TraceState:             context.TraceState().String(),
		Name:                   s.Name(),
		Kind:                   spanKind(s.SpanKind()),
		StartTimeUnixNano:      unixNano(s.StartTime()),
		EndTimeUnixNano:        unixNano(s.EndTime()),
		Attributes:             keyValues(s.Attributes()),
		DroppedAttributesCount: count(s.DroppedAttributes()),
		Events:                 events(s.Events()),
		DroppedEventsCount:     count(s.DroppedEvents()),
		Links:                  links(s.Links()),
		DroppedLinksCount:      count(s.DroppedLinks()),
		Status:                 spanStatus(status.Code, status.Description),
		Flags:                  spanFlags(context.TraceFlags(), s.Parent()),
	}
	if parent := s.Parent().SpanID(); parent.IsValid() {
		out.ParentSpanId = parent[:]
	}
	return out
}

// spanKind maps the SDK's kind onto the protocol's.
func spanKind(kind trace.SpanKind) tracepb.Span_SpanKind {
	switch kind {
	case trace.SpanKindInternal:
		return tracepb.Span_SPAN_KIND_INTERNAL
	case trace.SpanKindServer:
		return tracepb.Span_SPAN_KIND_SERVER
	case trace.SpanKindClient:
		return tracepb.Span_SPAN_KIND_CLIENT
	case trace.SpanKindProducer:
		return tracepb.Span_SPAN_KIND_PRODUCER
	case trace.SpanKindConsumer:
		return tracepb.Span_SPAN_KIND_CONSUMER
	default:
		return tracepb.Span_SPAN_KIND_UNSPECIFIED
	}
}

// spanStatus maps the SDK's status onto the three codes the protocol has.
func spanStatus(code codes.Code, message string) *tracepb.Status {
	out := &tracepb.Status{Message: message}
	switch code {
	case codes.Ok:
		out.Code = tracepb.Status_STATUS_CODE_OK
	case codes.Error:
		out.Code = tracepb.Status_STATUS_CODE_ERROR
	default:
		out.Code = tracepb.Status_STATUS_CODE_UNSET
	}
	return out
}

// spanFlags carries the W3C trace flags plus the two bits that say whether the
// parent was remote. The has-is-remote bit is always set: the SDK knows the
// answer, so a receiver never has to guess at it.
func spanFlags(flags trace.TraceFlags, parent trace.SpanContext) uint32 {
	out := uint32(flags) | uint32(tracepb.SpanFlags_SPAN_FLAGS_CONTEXT_HAS_IS_REMOTE_MASK)
	if parent.IsRemote() {
		out |= uint32(tracepb.SpanFlags_SPAN_FLAGS_CONTEXT_IS_REMOTE_MASK)
	}
	return out
}

func events(recorded []sdktrace.Event) []*tracepb.Span_Event {
	if len(recorded) == 0 {
		return nil
	}
	out := make([]*tracepb.Span_Event, 0, len(recorded))
	for _, e := range recorded {
		out = append(out, &tracepb.Span_Event{
			Name:                   e.Name,
			TimeUnixNano:           unixNano(e.Time),
			Attributes:             keyValues(e.Attributes),
			DroppedAttributesCount: count(e.DroppedAttributeCount),
		})
	}
	return out
}

func links(recorded []sdktrace.Link) []*tracepb.Span_Link {
	if len(recorded) == 0 {
		return nil
	}
	out := make([]*tracepb.Span_Link, 0, len(recorded))
	for _, l := range recorded {
		traceID := l.SpanContext.TraceID()
		spanID := l.SpanContext.SpanID()
		out = append(out, &tracepb.Span_Link{
			TraceId:                traceID[:],
			SpanId:                 spanID[:],
			TraceState:             l.SpanContext.TraceState().String(),
			Attributes:             keyValues(l.Attributes),
			DroppedAttributesCount: count(l.DroppedAttributeCount),
			Flags:                  spanFlags(l.SpanContext.TraceFlags(), l.SpanContext),
		})
	}
	return out
}

// resourceLogs groups a batch of log records the way resourceSpans groups
// spans.
func resourceLogs(records []sdklog.Record) []*logspb.ResourceLogs {
	if len(records) == 0 {
		return nil
	}
	type scopeKey struct {
		res   attribute.Distinct
		scope instrumentation.Scope
	}
	byResource := make(map[attribute.Distinct]*logspb.ResourceLogs)
	byScope := make(map[scopeKey]*logspb.ScopeLogs)
	var order []*logspb.ResourceLogs

	for _, r := range records {
		res := r.Resource()
		resKey := res.Equivalent()
		key := scopeKey{res: resKey, scope: r.InstrumentationScope()}
		scoped, known := byScope[key]
		if !known {
			scoped = &logspb.ScopeLogs{
				Scope:     scope(r.InstrumentationScope()),
				SchemaUrl: r.InstrumentationScope().SchemaURL,
			}
			byScope[key] = scoped
		}
		scoped.LogRecords = append(scoped.LogRecords, protoLogRecord(r))

		grouped, seen := byResource[resKey]
		if !seen {
			grouped = &logspb.ResourceLogs{
				Resource:  resourceOf(res),
				SchemaUrl: res.SchemaURL(),
			}
			byResource[resKey] = grouped
			order = append(order, grouped)
		}
		if !known {
			grouped.ScopeLogs = append(grouped.ScopeLogs, scoped)
		}
	}
	return order
}

// protoLogRecord transforms one log record. WalkAttributes is the only way to read
// a record's attributes back, so the list is built by walking it.
func protoLogRecord(r sdklog.Record) *logspb.LogRecord {
	out := &logspb.LogRecord{
		TimeUnixNano:           unixNano(r.Timestamp()),
		ObservedTimeUnixNano:   unixNano(r.ObservedTimestamp()),
		EventName:              r.EventName(),
		SeverityNumber:         severity(r.Severity()),
		SeverityText:           r.SeverityText(),
		Body:                   value(r.Body()),
		Attributes:             make([]*commonpb.KeyValue, 0, r.AttributesLen()),
		DroppedAttributesCount: count(r.DroppedAttributes()),
		Flags:                  uint32(r.TraceFlags()),
	}
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		out.Attributes = append(out.Attributes, keyValue(kv))
		return true
	})
	// An unset identifier is left off the record rather than sent as zeroes,
	// which is what tells a receiver the record belongs to no span.
	if traceID := r.TraceID(); traceID.IsValid() {
		out.TraceId = traceID[:]
	}
	if spanID := r.SpanID(); spanID.IsValid() {
		out.SpanId = spanID[:]
	}
	return out
}

// severity maps a log severity onto the protocol's number. The two scales are
// the same one: the OpenTelemetry log API defines its values as the severity
// numbers of the protocol. A value from outside the range is sent unspecified
// rather than as a number the protocol does not define.
func severity(s otellog.Severity) logspb.SeverityNumber {
	if s < otellog.SeverityTrace1 || s > otellog.SeverityFatal4 {
		return logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED
	}
	return logspb.SeverityNumber(s)
}

// resourceOf transforms the resource every record of a batch shares.
func resourceOf(res *resource.Resource) *resourcepb.Resource {
	if res == nil || res.Len() == 0 {
		return nil
	}
	return &resourcepb.Resource{Attributes: iterator(res.Iter())}
}

// scope names the instrumentation that produced a record.
func scope(s instrumentation.Scope) *commonpb.InstrumentationScope {
	if s == (instrumentation.Scope{}) {
		return nil
	}
	return &commonpb.InstrumentationScope{
		Name:       s.Name,
		Version:    s.Version,
		Attributes: iterator(s.Attributes.Iter()),
	}
}

func keyValues(attrs []attribute.KeyValue) []*commonpb.KeyValue {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]*commonpb.KeyValue, 0, len(attrs))
	for _, kv := range attrs {
		out = append(out, keyValue(kv))
	}
	return out
}

func iterator(iter attribute.Iterator) []*commonpb.KeyValue {
	if iter.Len() == 0 {
		return nil
	}
	out := make([]*commonpb.KeyValue, 0, iter.Len())
	for iter.Next() {
		out = append(out, keyValue(iter.Attribute()))
	}
	return out
}

func keyValue(kv attribute.KeyValue) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: string(kv.Key), Value: value(kv.Value)}
}

// value transforms one attribute value of any kind the SDK can hold: the four
// scalars, the four typed slices, bytes, a slice of values and a map, the last
// two nesting as deep as the caller built them.
func value(v attribute.Value) *commonpb.AnyValue {
	switch v.Type() {
	case attribute.BOOL:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v.AsBool()}}
	case attribute.INT64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v.AsInt64()}}
	case attribute.FLOAT64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v.AsFloat64()}}
	case attribute.STRING:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v.AsString()}}
	case attribute.BOOLSLICE:
		items := v.AsBoolSlice()
		out := make([]*commonpb.AnyValue, 0, len(items))
		for _, item := range items {
			out = append(out, &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: item}})
		}
		return array(out)
	case attribute.INT64SLICE:
		items := v.AsInt64Slice()
		out := make([]*commonpb.AnyValue, 0, len(items))
		for _, item := range items {
			out = append(out, &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: item}})
		}
		return array(out)
	case attribute.FLOAT64SLICE:
		items := v.AsFloat64Slice()
		out := make([]*commonpb.AnyValue, 0, len(items))
		for _, item := range items {
			out = append(out, &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: item}})
		}
		return array(out)
	case attribute.STRINGSLICE:
		items := v.AsStringSlice()
		out := make([]*commonpb.AnyValue, 0, len(items))
		for _, item := range items {
			out = append(out, &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: item}})
		}
		return array(out)
	case attribute.BYTESLICE:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: v.AsByteSlice()}}
	case attribute.SLICE:
		items := v.AsSlice()
		out := make([]*commonpb.AnyValue, 0, len(items))
		for _, item := range items {
			out = append(out, value(item))
		}
		return array(out)
	case attribute.MAP:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{
			KvlistValue: &commonpb.KeyValueList{Values: keyValues(v.AsMap())},
		}}
	case attribute.EMPTY:
		// A value that was never set, which a log record with no body is.
		return &commonpb.AnyValue{}
	default:
		// A kind this build does not know. The SDK's own exporters send the
		// word rather than nothing, so a receiver sees the producer's bug
		// instead of a missing attribute.
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "INVALID"}}
	}
}

func array(values []*commonpb.AnyValue) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{
		ArrayValue: &commonpb.ArrayValue{Values: values},
	}}
}

// unixNano renders a timestamp the way the protocol carries one. A zero time,
// and any time before the epoch, is no timestamp rather than a negative one.
func unixNano(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	nano := t.UnixNano()
	if nano < 0 {
		return 0
	}
	return uint64(nano)
}

// count renders a dropped count, which the protocol carries unsigned.
func count(n int) uint32 {
	if n < 0 {
		return 0
	}
	if int64(n) > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(n)
}
