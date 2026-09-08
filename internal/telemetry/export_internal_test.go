package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/zyx1121/kitbash/internal/otlp"
	commonpb "github.com/zyx1121/kitbash/internal/otlpproto/common/v1"
	logspb "github.com/zyx1121/kitbash/internal/otlpproto/logs/v1"
	tracepb "github.com/zyx1121/kitbash/internal/otlpproto/trace/v1"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/telemetry/teltest"
)

// These tests exercise the exporters kitbash writes in place of the SDK's own
// OTLP exporters, see export.go and transform.go. Every assertion is on what
// the daemon decoded off the socket, not on what the exporter was handed: the
// point of the pair is that the bytes on the wire are the same OTLP as before.

const (
	scopeVersion   = "test"
	scopeSchemaURL = "https://opentelemetry.io/schemas/1.43.0"
)

func fakeDaemon(t *testing.T) *teltest.Daemon {
	t.Helper()
	daemon, err := teltest.Start()
	if err != nil {
		t.Fatalf("teltest.Start: %v", err)
	}
	t.Cleanup(daemon.Close)
	return daemon
}

// everyKind is one attribute of every kind an attribute.Value can hold.
func everyKind() []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.Bool("bool", true),
		attribute.Int64("int", 7),
		attribute.Float64("float", 1.5),
		attribute.String("string", "/org/ffmpeg"),
		attribute.BoolSlice("bools", []bool{true, false}),
		attribute.Int64Slice("ints", []int64{1, 2}),
		attribute.Float64Slice("floats", []float64{1.5, 2.5}),
		attribute.StringSlice("strings", []string{"a", "b"}),
		attribute.ByteSlice("bytes", []byte{0xde, 0xad}),
		attribute.Slice("slice", attribute.StringValue("a"), attribute.Int64Value(2)),
		attribute.Map("map", attribute.String("inner", "v")),
		{Key: "empty"},
	}
}

// wantEveryKind is everyKind as the protocol carries it.
func wantEveryKind() map[string]*commonpb.AnyValue {
	str := func(s string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
	}
	i64 := func(n int64) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: n}}
	}
	f64 := func(f float64) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: f}}
	}
	b := func(v bool) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v}}
	}
	list := func(values ...*commonpb.AnyValue) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{
			ArrayValue: &commonpb.ArrayValue{Values: values},
		}}
	}
	return map[string]*commonpb.AnyValue{
		"bool":    b(true),
		"int":     i64(7),
		"float":   f64(1.5),
		"string":  str("/org/ffmpeg"),
		"bools":   list(b(true), b(false)),
		"ints":    list(i64(1), i64(2)),
		"floats":  list(f64(1.5), f64(2.5)),
		"strings": list(str("a"), str("b")),
		"bytes":   {Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{0xde, 0xad}}},
		"slice":   list(str("a"), i64(2)),
		"map": {Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{
			Values: []*commonpb.KeyValue{{Key: "inner", Value: str("v")}},
		}}},
		"empty": {},
	}
}

// byKey indexes an attribute list, which is how a test asks about one of them.
func byKey(kvs []*commonpb.KeyValue) map[string]*commonpb.AnyValue {
	out := make(map[string]*commonpb.AnyValue, len(kvs))
	for _, kv := range kvs {
		out[kv.GetKey()] = kv.GetValue()
	}
	return out
}

func checkKinds(t *testing.T, what string, kvs []*commonpb.KeyValue, want map[string]*commonpb.AnyValue) {
	t.Helper()
	got := byKey(kvs)
	for key, value := range want {
		if !proto.Equal(got[key], value) {
			t.Errorf("%s carries %s = %v, want %v", what, key, got[key], value)
		}
	}
}

// The span the exporter is asked to send, with every field the SDK can put on
// one: identifiers, a remote parent, a kind, a trace state, both timestamps,
// attributes of every value kind, an event, a link, a status and three dropped
// counts.
func fullSpan(t *testing.T) tracetest.SpanStub {
	t.Helper()
	traceID := trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	spanID := trace.SpanID{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18}
	parentID := trace.SpanID{0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28}
	linkTraceID := trace.TraceID{0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38,
		0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e, 0x3f, 0x40}
	linkSpanID := trace.SpanID{0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48}
	state, err := trace.ParseTraceState("kitbash=1")
	if err != nil {
		t.Fatalf("trace state: %v", err)
	}
	return tracetest.SpanStub{
		Name: "pkg_build",
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: traceID, SpanID: spanID,
			TraceFlags: trace.FlagsSampled, TraceState: state,
		}),
		Parent: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: traceID, SpanID: parentID,
			TraceFlags: trace.FlagsSampled, Remote: true,
		}),
		SpanKind:   trace.SpanKindServer,
		StartTime:  time.Unix(1700000000, 123),
		EndTime:    time.Unix(1700000001, 456),
		Attributes: everyKind(),
		Events: []sdktrace.Event{{
			Name:                  "commit",
			Time:                  time.Unix(1700000000, 999),
			Attributes:            []attribute.KeyValue{attribute.String("digest", "sha256:abc")},
			DroppedAttributeCount: 1,
		}},
		Links: []sdktrace.Link{{
			SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: linkTraceID, SpanID: linkSpanID, TraceFlags: trace.FlagsSampled,
			}),
			Attributes:            []attribute.KeyValue{attribute.String("why", "caused by")},
			DroppedAttributeCount: 2,
		}},
		Status:            sdktrace.Status{Code: codes.Error, Description: "not found"},
		DroppedAttributes: 3,
		DroppedEvents:     4,
		DroppedLinks:      5,
		Resource: resource.NewWithAttributes(semconv.SchemaURL,
			semconv.ServiceName(ServiceName), semconv.ServiceVersion(scopeVersion)),
		InstrumentationScope: instrumentation.Scope{
			Name: scopeName, Version: scopeVersion, SchemaURL: scopeSchemaURL,
		},
	}
}

func TestTheSpanExporterSendsEveryFieldOfASpan(t *testing.T) {
	daemon := fakeDaemon(t)
	exporter := newSpanExporter(socketClient(daemon.Socket))
	stub := fullSpan(t)

	if err := exporter.ExportSpans(context.Background(),
		[]sdktrace.ReadOnlySpan{stub.Snapshot()}); err != nil {
		t.Fatalf("exporting: %v", err)
	}

	requests := daemon.TraceRequests()
	if len(requests) != 1 {
		t.Fatalf("the daemon received %d requests, want one", len(requests))
	}
	resources := requests[0].GetResourceSpans()
	if len(resources) != 1 || len(resources[0].GetScopeSpans()) != 1 {
		t.Fatalf("the request carries %+v, want one resource with one scope", resources)
	}
	if got := resources[0].GetSchemaUrl(); got != semconv.SchemaURL {
		t.Errorf("the resource schema is %q, want %q", got, semconv.SchemaURL)
	}
	res := byKey(resources[0].GetResource().GetAttributes())
	if got := res[string(semconv.ServiceNameKey)].GetStringValue(); got != ServiceName {
		t.Errorf("the resource says service.name is %q, want %s", got, ServiceName)
	}
	scoped := resources[0].GetScopeSpans()[0]
	if scoped.GetScope().GetName() != scopeName || scoped.GetScope().GetVersion() != scopeVersion {
		t.Errorf("the scope is %+v, want %s %s", scoped.GetScope(), scopeName, scopeVersion)
	}
	if scoped.GetSchemaUrl() != scopeSchemaURL {
		t.Errorf("the scope schema is %q, want %q", scoped.GetSchemaUrl(), scopeSchemaURL)
	}
	if len(scoped.GetSpans()) != 1 {
		t.Fatalf("the scope carries %d spans, want one", len(scoped.GetSpans()))
	}
	got := scoped.GetSpans()[0]

	wantTraceID := stub.SpanContext.TraceID()
	wantSpanID := stub.SpanContext.SpanID()
	wantParentID := stub.Parent.SpanID()
	if string(got.GetTraceId()) != string(wantTraceID[:]) {
		t.Errorf("the trace id is %x, want %x", got.GetTraceId(), wantTraceID[:])
	}
	if string(got.GetSpanId()) != string(wantSpanID[:]) {
		t.Errorf("the span id is %x, want %x", got.GetSpanId(), wantSpanID[:])
	}
	if string(got.GetParentSpanId()) != string(wantParentID[:]) {
		t.Errorf("the parent span id is %x, want %x", got.GetParentSpanId(), wantParentID[:])
	}
	if got.GetName() != "pkg_build" {
		t.Errorf("the name is %q", got.GetName())
	}
	if got.GetKind() != tracepb.Span_SPAN_KIND_SERVER {
		t.Errorf("the kind is %s, want a server span", got.GetKind())
	}
	if got.GetTraceState() != "kitbash=1" {
		t.Errorf("the trace state is %q, want kitbash=1", got.GetTraceState())
	}
	if got.GetStartTimeUnixNano() != uint64(stub.StartTime.UnixNano()) {
		t.Errorf("the start is %d, want %d", got.GetStartTimeUnixNano(), stub.StartTime.UnixNano())
	}
	if got.GetEndTimeUnixNano() != uint64(stub.EndTime.UnixNano()) {
		t.Errorf("the end is %d, want %d", got.GetEndTimeUnixNano(), stub.EndTime.UnixNano())
	}
	if got.GetStatus().GetCode() != tracepb.Status_STATUS_CODE_ERROR ||
		got.GetStatus().GetMessage() != "not found" {
		t.Errorf("the status is %+v, want an error with the description", got.GetStatus())
	}
	if got.GetDroppedAttributesCount() != 3 || got.GetDroppedEventsCount() != 4 ||
		got.GetDroppedLinksCount() != 5 {
		t.Errorf("the dropped counts are %d/%d/%d, want 3/4/5", got.GetDroppedAttributesCount(),
			got.GetDroppedEventsCount(), got.GetDroppedLinksCount())
	}
	// A remote parent is what both flag bits are for: the receiver is told
	// that the answer is known and that the answer is yes.
	hasRemote := uint32(tracepb.SpanFlags_SPAN_FLAGS_CONTEXT_HAS_IS_REMOTE_MASK)
	isRemote := uint32(tracepb.SpanFlags_SPAN_FLAGS_CONTEXT_IS_REMOTE_MASK)
	if got.GetFlags()&hasRemote == 0 || got.GetFlags()&isRemote == 0 {
		t.Errorf("the flags are %08b, want the remote parent bits set", got.GetFlags())
	}
	if uint8(got.GetFlags()) != uint8(trace.FlagsSampled) {
		t.Errorf("the flags are %08b, want the sampled bit in the low byte", got.GetFlags())
	}
	checkKinds(t, "the span", got.GetAttributes(), wantEveryKind())

	if len(got.GetEvents()) != 1 {
		t.Fatalf("the span carries %d events, want one", len(got.GetEvents()))
	}
	event := got.GetEvents()[0]
	if event.GetName() != "commit" || event.GetDroppedAttributesCount() != 1 {
		t.Errorf("the event is %+v, want commit with one dropped attribute", event)
	}
	if event.GetTimeUnixNano() != uint64(stub.Events[0].Time.UnixNano()) {
		t.Errorf("the event time is %d, want %d", event.GetTimeUnixNano(),
			stub.Events[0].Time.UnixNano())
	}
	if byKey(event.GetAttributes())["digest"].GetStringValue() != "sha256:abc" {
		t.Errorf("the event carries %+v, want the digest", event.GetAttributes())
	}

	if len(got.GetLinks()) != 1 {
		t.Fatalf("the span carries %d links, want one", len(got.GetLinks()))
	}
	link := got.GetLinks()[0]
	linkTraceID := stub.Links[0].SpanContext.TraceID()
	linkSpanID := stub.Links[0].SpanContext.SpanID()
	if string(link.GetTraceId()) != string(linkTraceID[:]) ||
		string(link.GetSpanId()) != string(linkSpanID[:]) {
		t.Errorf("the link points at %x/%x, want %x/%x", link.GetTraceId(), link.GetSpanId(),
			linkTraceID[:], linkSpanID[:])
	}
	if link.GetDroppedAttributesCount() != 2 {
		t.Errorf("the link dropped %d attributes, want 2", link.GetDroppedAttributesCount())
	}
	if byKey(link.GetAttributes())["why"].GetStringValue() != "caused by" {
		t.Errorf("the link carries %+v, want the reason", link.GetAttributes())
	}
	if link.GetFlags()&hasRemote == 0 {
		t.Errorf("the link flags are %08b, want the has-is-remote bit set", link.GetFlags())
	}
}

// A span the exporter sent has to arrive as the record the store would keep,
// which is the whole reason the producer and the receiver share one set of
// message definitions. The request the daemon decoded is marshalled again and
// handed to the receiver, so the bytes under test are the bytes that crossed
// the socket.
func TestAnExportedSpanDecodesToTheStoreRecord(t *testing.T) {
	daemon := fakeDaemon(t)
	exporter := newSpanExporter(socketClient(daemon.Socket))
	stub := fullSpan(t)
	stub.Attributes = []attribute.KeyValue{
		attribute.String(AttrUser, "tester"),
		attribute.String(AttrTool, "pkg_build"),
		attribute.String(AttrPackage, "/org/ffmpeg"),
		attribute.String(AttrDigest, "sha256:abc"),
	}
	if err := exporter.ExportSpans(context.Background(),
		[]sdktrace.ReadOnlySpan{stub.Snapshot()}); err != nil {
		t.Fatalf("exporting: %v", err)
	}
	requests := daemon.TraceRequests()
	if len(requests) != 1 {
		t.Fatalf("the daemon received %d requests, want one", len(requests))
	}
	body, err := proto.Marshal(requests[0])
	if err != nil {
		t.Fatalf("re-encoding what arrived: %v", err)
	}

	spans, err := otlp.DecodeTraces(otlp.Protobuf, body)
	if err != nil {
		t.Fatalf("the receiver refused the export: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("the receiver decoded %d spans, want one", len(spans))
	}
	got := spans[0]
	traceID := stub.SpanContext.TraceID()
	spanID := stub.SpanContext.SpanID()
	parentID := stub.Parent.SpanID()
	for _, field := range []struct {
		what string
		got  any
		want any
	}{
		{"trace id", got.TraceID, traceID.String()},
		{"span id", got.SpanID, spanID.String()},
		{"parent span id", got.ParentSpanID, parentID.String()},
		{"name", got.Name, "pkg_build"},
		{"start", got.StartNS, stub.StartTime.UnixNano()},
		{"end", got.EndNS, stub.EndTime.UnixNano()},
		{"status", got.Status, store.StatusError},
		{"status message", got.StatusMessage, "not found"},
	} {
		if field.got != field.want {
			t.Errorf("the store record's %s is %v, want %v", field.what, field.got, field.want)
		}
	}
	attrs := spans[0].Attributes
	if attrs.User != "tester" || attrs.Tool != "pkg_build" || attrs.Package != "/org/ffmpeg" {
		t.Errorf("the record's columns are %+v, want the kitbash attributes lifted", attrs)
	}
	// The resource attributes are merged into the record, which is what a
	// query for one service's spans reads.
	if attrs.Other[string(semconv.ServiceNameKey)] != ServiceName {
		t.Errorf("the record carries %v, want the resource merged in", attrs.Other)
	}
	if attrs.Other[AttrDigest] != "sha256:abc" {
		t.Errorf("the record carries %v, want the digest", attrs.Other)
	}
}

// loggerOn builds the logger of one session over the exporter under test. It
// goes through the SDK rather than building an sdklog.Record by hand: a record
// the SDK did not make carries none of the limits, and the resource and the
// scope are the provider's to put on it.
func loggerOn(t *testing.T, daemon *teltest.Daemon) (*sdklog.LoggerProvider, otellog.Logger) {
	t.Helper()
	provider := sdklog.NewLoggerProvider(
		sdklog.WithResource(resource.NewWithAttributes(semconv.SchemaURL,
			semconv.ServiceName(ServiceName), semconv.ServiceVersion(scopeVersion))),
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(
			newLogExporter(socketClient(daemon.Socket)))),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return provider, provider.Logger(scopeName,
		otellog.WithInstrumentationVersion(scopeVersion),
		otellog.WithSchemaURL(scopeSchemaURL))
}

func TestTheLogExporterSendsEveryFieldOfARecord(t *testing.T) {
	daemon := fakeDaemon(t)
	_, logger := loggerOn(t, daemon)

	traceID := trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	spanID := trace.SpanID{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18}
	stamped := time.Unix(1700000000, 123)
	observed := time.Unix(1700000000, 456)
	var record otellog.Record
	record.SetTimestamp(stamped)
	record.SetObservedTimestamp(observed)
	record.SetEventName("build")
	record.SetSeverity(otellog.SeverityError)
	record.SetSeverityText("ERROR")
	record.SetBody(attribute.StringValue("COMMIT localhost/kitbash/ffmpeg:test"))
	record.AddAttributes(everyKind()...)
	// The span a record belongs to is the one in its context, which is how
	// Span.Info lands a build's output on the build's trace.
	ctx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
		}))
	logger.Emit(ctx, record)

	requests := daemon.LogRequests()
	if len(requests) != 1 {
		t.Fatalf("the daemon received %d requests, want one", len(requests))
	}
	resources := requests[0].GetResourceLogs()
	if len(resources) != 1 || len(resources[0].GetScopeLogs()) != 1 {
		t.Fatalf("the request carries %+v, want one resource with one scope", resources)
	}
	if byKey(resources[0].GetResource().GetAttributes())[string(semconv.ServiceNameKey)].
		GetStringValue() != ServiceName {
		t.Errorf("the resource is %+v, want the service name", resources[0].GetResource())
	}
	scoped := resources[0].GetScopeLogs()[0]
	if scoped.GetScope().GetName() != scopeName || scoped.GetSchemaUrl() != scopeSchemaURL {
		t.Errorf("the scope is %+v at %q, want %s at %s", scoped.GetScope(), scoped.GetSchemaUrl(),
			scopeName, scopeSchemaURL)
	}
	records := scoped.GetLogRecords()
	if len(records) != 1 {
		t.Fatalf("the scope carries %d records, want one", len(records))
	}
	got := records[0]
	if got.GetTimeUnixNano() != uint64(stamped.UnixNano()) {
		t.Errorf("the time is %d, want %d", got.GetTimeUnixNano(), stamped.UnixNano())
	}
	if got.GetObservedTimeUnixNano() != uint64(observed.UnixNano()) {
		t.Errorf("the observed time is %d, want %d", got.GetObservedTimeUnixNano(), observed.UnixNano())
	}
	if got.GetEventName() != "build" {
		t.Errorf("the event name is %q, want build", got.GetEventName())
	}
	if got.GetSeverityNumber() != logspb.SeverityNumber_SEVERITY_NUMBER_ERROR {
		t.Errorf("the severity number is %s, want ERROR", got.GetSeverityNumber())
	}
	if got.GetSeverityText() != "ERROR" {
		t.Errorf("the severity text is %q", got.GetSeverityText())
	}
	if got.GetBody().GetStringValue() != "COMMIT localhost/kitbash/ffmpeg:test" {
		t.Errorf("the body is %q", got.GetBody().GetStringValue())
	}
	if string(got.GetTraceId()) != string(traceID[:]) || string(got.GetSpanId()) != string(spanID[:]) {
		t.Errorf("the record belongs to %x/%x, want %x/%x", got.GetTraceId(), got.GetSpanId(),
			traceID[:], spanID[:])
	}
	if uint8(got.GetFlags()) != uint8(trace.FlagsSampled) {
		t.Errorf("the flags are %08b, want the sampled bit", got.GetFlags())
	}
	// The SDK drops an attribute with no value before an exporter sees it,
	// so the empty one everyKind carries is not among these.
	want := wantEveryKind()
	delete(want, "empty")
	checkKinds(t, "the record", got.GetAttributes(), want)
}

// A record that belongs to no span carries no identifiers at all, which is
// what tells the receiver to store it without one rather than to refuse an
// identifier of the wrong width.
func TestALogRecordWithoutASpanCarriesNoIdentifiers(t *testing.T) {
	daemon := fakeDaemon(t)
	_, logger := loggerOn(t, daemon)

	var record otellog.Record
	record.SetTimestamp(time.Unix(1700000000, 0))
	record.SetSeverity(otellog.SeverityInfo)
	record.SetBody(attribute.StringValue("nobody's span"))
	logger.Emit(context.Background(), record)

	requests := daemon.LogRequests()
	if len(requests) != 1 {
		t.Fatalf("the daemon received %d requests, want one", len(requests))
	}
	got := requests[0].GetResourceLogs()[0].GetScopeLogs()[0].GetLogRecords()[0]
	if len(got.GetTraceId()) != 0 || len(got.GetSpanId()) != 0 {
		t.Errorf("the record carries %x/%x, want no identifiers", got.GetTraceId(), got.GetSpanId())
	}
	if got.GetSeverityNumber() != logspb.SeverityNumber_SEVERITY_NUMBER_INFO {
		t.Errorf("the severity number is %s, want INFO", got.GetSeverityNumber())
	}
}

// An exporter that has been shut down sends nothing and says nothing went
// wrong: the session is over and the batch is gone either way.
func TestAShutdownExporterSendsNothing(t *testing.T) {
	daemon := fakeDaemon(t)
	spans := newSpanExporter(socketClient(daemon.Socket))
	records := newLogExporter(socketClient(daemon.Socket))
	ctx := context.Background()

	for range 2 {
		if err := spans.Shutdown(ctx); err != nil {
			t.Fatalf("shutting the span exporter down: %v", err)
		}
		if err := records.Shutdown(ctx); err != nil {
			t.Fatalf("shutting the log exporter down: %v", err)
		}
	}
	if err := spans.ExportSpans(ctx, []sdktrace.ReadOnlySpan{fullSpan(t).Snapshot()}); err != nil {
		t.Errorf("exporting after shutdown: %v", err)
	}
	var record sdklog.Record
	record.SetBody(attribute.StringValue("after the session"))
	if err := records.Export(ctx, []sdklog.Record{record}); err != nil {
		t.Errorf("exporting after shutdown: %v", err)
	}
	if got := daemon.TraceRequests(); len(got) != 0 {
		t.Errorf("the daemon received %d trace requests after shutdown", len(got))
	}
	if got := daemon.LogRequests(); len(got) != 0 {
		t.Errorf("the daemon received %d log requests after shutdown", len(got))
	}
}

// An empty batch is not a request: the SDK offers one when a flush finds
// nothing, and a daemon that logged it would be logging the flush.
func TestAnEmptyBatchIsNotSent(t *testing.T) {
	daemon := fakeDaemon(t)
	ctx := context.Background()
	if err := newSpanExporter(socketClient(daemon.Socket)).ExportSpans(ctx, nil); err != nil {
		t.Fatalf("exporting nothing: %v", err)
	}
	if err := newLogExporter(socketClient(daemon.Socket)).Export(ctx, nil); err != nil {
		t.Fatalf("exporting nothing: %v", err)
	}
	if got := daemon.ContentTypes(); len(got) != 0 {
		t.Errorf("the daemon received %d requests, want none", len(got))
	}
}

// The exports are protobuf, which is the encoding spec/kitbashd-api.yaml has
// the producer speak.
func TestExportsAreProtobuf(t *testing.T) {
	daemon := fakeDaemon(t)
	ctx := context.Background()
	if err := newSpanExporter(socketClient(daemon.Socket)).
		ExportSpans(ctx, []sdktrace.ReadOnlySpan{fullSpan(t).Snapshot()}); err != nil {
		t.Fatalf("exporting: %v", err)
	}
	types := daemon.ContentTypes()
	if len(types) != 1 || types[0] != contentTypeProtobuf {
		t.Errorf("the daemon saw %v, want one %s", types, contentTypeProtobuf)
	}
}
