// Package telemetry is the producer side of the Telemetry object, PLAN.md
// section 2.4. kitbash-mcp opens one span per tools/call, a child span per
// build and per Process start, and one log record per build, and exports them
// as OTLP over HTTP on the kitbashd unix socket.
//
// Export is asynchronous and never blocks a tool call. When the socket is
// absent or refuses the connection the records are dropped and one line goes
// to the server log for the whole session, which is the client behaviour
// spec/kitbashd-api.yaml describes.
//
// Nothing here imports MCP types, and no service imports the OpenTelemetry SDK:
// the helpers below are the whole interface the fs, pkg and proc families use.
package telemetry

import (
	"context"
	"log"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"go.opentelemetry.io/otel/codes"
)

// ServiceName is the OpenTelemetry service every record from this process
// carries as a resource attribute.
const ServiceName = "kitbash-mcp"

// scopeName is the instrumentation scope of the spans and log records this
// package produces.
const scopeName = "github.com/zyx1121/kitbash/internal/telemetry"

// ShutdownTimeout is how long the last flush of a session may take. An agent's
// SSH session is over by then and nobody is waiting for the records. Traces
// and logs flush in parallel, so each gets the whole budget rather than half
// of it.
const ShutdownTimeout = 3 * time.Second

// AttributeValueLimit and AttributeCountLimit bound what one span may carry.
// An argument reaches a span before anything has validated it, and a record
// larger than kitbashd's request cap is a record the whole batch dies with,
// so the producer truncates rather than letting a caller decide the size.
const (
	AttributeValueLimit = 1 << 10
	AttributeCountLimit = 32
)

// The attribute keys of PLAN.md section 2.4, namespaced as the OpenTelemetry
// conventions require. kitbash.user is stamped again by kitbashd from the
// socket's peer credentials, so what this process sends for it is a
// convenience, never an identity claim.
const (
	AttrUser    = "kitbash.user"
	AttrPackage = "kitbash.package"
	AttrProcess = "kitbash.process"
	AttrPath    = "kitbash.path"
	AttrTool    = "kitbash.tool"
	AttrEval    = "kitbash.eval"
)

// AttrDigest and AttrError are the two attributes the producer adds beyond the
// six the query surface speaks: the OCI digest a build produced or a Process
// runs, and the slug of the problem a failed call returned.
const (
	AttrDigest = "kitbash.digest"
	AttrError  = "kitbash.error"
)

// AttrCaller carries the caller credential of this session, which kitbashd
// rewrites to the Process id of the session it minted the credential for. A
// query then answers what a Process did on its owner's behalf, see PLAN.md
// section 2.3. This process never learns the Process id and never sends one.
const AttrCaller = "kitbash.caller"

// EnvCaller carries that credential into this process. It is set by kitbashd
// on the kitbash-mcp it starts for a Process and by nothing else; a session a
// member opens over SSH carries none. The writer spells it in
// internal/sysusers, which cannot import this package without pulling the
// OpenTelemetry SDK into kitbashd.
const EnvCaller = "KITBASH_CALLER"

// Caller is the credential of the session this process serves, empty for a
// member's own session. Unlike the socket and roots overrides it is honoured
// in an SSH session too, and it costs nothing to allow: a credential kitbashd
// did not mint resolves to no Process and is dropped when the record arrives.
func Caller() string { return os.Getenv(EnvCaller) }

// AttrApproval and AttrRequester name the queued call an approvals_approve
// span executed and the member it was executed for. The admin's session
// records the span, so without them a query for what a member asked for would
// end at the queued problem.
const (
	AttrApproval  = "kitbash.approval"
	AttrRequester = "kitbash.requester"
)

// Options configure a Provider.
type Options struct {
	// Version is the kitbash-mcp version, exported as service.version.
	Version string
	// Socket is the kitbashd socket. Empty means SocketPath.
	Socket string
	// Logger receives the one line a dropping session is worth. Empty means
	// the same stderr logger the rest of kitbash writes to.
	Logger *log.Logger
	// Caller is the credential of the session this process serves, recorded
	// on every span as kitbash.caller. Empty means the one KITBASH_CALLER
	// carries, which is nothing for a member's own session.
	Caller string
}

// Provider owns the exporters of one session.
type Provider struct {
	caller string
	tracer trace.Tracer
	logger otellog.Logger
	traces *sdktrace.TracerProvider
	logs   *sdklog.LoggerProvider
	client *Client
}

// New builds the provider for one session. It opens no connection: the socket
// is dialled by the first export, in the background, so a missing kitbashd
// costs the session nothing at start up.
func New(opts Options) (*Provider, error) {
	socket := opts.Socket
	if socket == "" {
		socket = SocketPath()
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.New(os.Stderr, "kitbash: ", log.LstdFlags)
	}
	client := socketClient(socket)
	drops := &notice{socket: socket, logger: logger}

	// The two exporters share the one client, which dials the socket whatever
	// host the URL names, see socketClient and export.go.
	spans := newSpanExporter(client)
	records := newLogExporter(client)

	version := opts.Version
	if version == "" {
		version = "dev"
	}
	res := resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceName(ServiceName),
		semconv.ServiceVersion(version),
	)
	// The limits are pinned rather than taken from NewSpanLimits alone, whose
	// two size limits the OTEL_ environment can widen.
	limits := sdktrace.NewSpanLimits()
	limits.AttributeValueLengthLimit = AttributeValueLimit
	limits.AttributeCountLimit = AttributeCountLimit
	traces := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(&spanGuard{next: spans, notice: drops}),
		sdktrace.WithResource(res),
		sdktrace.WithSpanLimits(limits),
		// PLAN.md section 2.6 says there is no untraced path, so the sampler
		// is not the environment's to choose: OTEL_TRACES_SAMPLER would
		// otherwise silence the surface without anyone noticing.
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	logs := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(&logGuard{next: records, notice: drops})),
		sdklog.WithResource(res),
		sdklog.WithAttributeValueLengthLimit(AttributeValueLimit),
		sdklog.WithAttributeCountLimit(AttributeCountLimit),
	)
	caller := opts.Caller
	if caller == "" {
		caller = Caller()
	}
	return &Provider{
		caller: caller,
		tracer: traces.Tracer(scopeName),
		logger: logs.Logger(scopeName),
		traces: traces,
		logs:   logs,
		client: NewClient(socket),
	}, nil
}

// NewFromEnv is New with the socket the environment names. A provider that
// cannot be built is not a reason to refuse the session: the surface still
// works, untraced, and the reason is in the server log.
func NewFromEnv(version string) *Provider {
	p, err := New(Options{Version: version})
	if err != nil {
		log.New(os.Stderr, "kitbash: ", log.LstdFlags).
			Printf("telemetry: this session is untraced: %v", err)
		return nil
	}
	return p
}

// Client is the JSON API of kitbashd, which tel_query and tel_retention
// forward to.
func (p *Provider) Client() *Client {
	if p == nil {
		return nil
	}
	return p.client
}

// ForceFlush exports what is batched without ending the session. Nothing on
// the surface needs it: it is how a caller that has to see a record land, such
// as a test, gets one across without waiting for the batch timeout.
func (p *Provider) ForceFlush(ctx context.Context) error {
	if p == nil {
		return nil
	}
	err := p.traces.ForceFlush(ctx)
	if logErr := p.logs.ForceFlush(ctx); err == nil {
		err = logErr
	}
	return err
}

// Shutdown flushes what is batched and closes the exporters. It is bounded by
// ShutdownTimeout however much time the caller offers, and the two signals
// flush in parallel so a slow daemon cannot spend the traces' budget on the
// logs.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, ShutdownTimeout)
	defer cancel()
	var logErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		logErr = p.logs.Shutdown(ctx)
	}()
	err := p.traces.Shutdown(ctx)
	<-done
	if err == nil {
		err = logErr
	}
	return err
}

// StartTool opens the span of one tools/call. The returned context carries the
// call, so a service can add attributes, open a child span and emit a log
// record without holding a provider or a span.
func (p *Provider) StartTool(ctx context.Context, tool, user string) (context.Context, *Span) {
	if p == nil {
		return ctx, nil
	}
	ctx, span := p.tracer.Start(ctx, tool)
	s := &Span{span: span, provider: p}
	ctx = context.WithValue(ctx, callKey{}, &call{provider: p, tool: tool, user: user, span: s})
	s.ctx = ctx
	s.Set(AttrUser, user)
	s.Set(AttrTool, tool)
	// A session kitbashd opened for a Process records the credential of that
	// session, which the daemon resolves. Set is a no-op for the empty
	// string, so a member's session carries none.
	s.Set(AttrCaller, p.caller)
	return ctx, s
}

// call is the tools/call in flight: what every span of it has in common, and
// the span that describes the call as a whole.
type call struct {
	provider *Provider
	tool     string
	user     string
	span     *Span
}

// callKey carries the call in flight through its context, and currentKey the
// innermost span opened for it.
type (
	callKey    struct{}
	currentKey struct{}
)

func inFlight(ctx context.Context) *call {
	c, _ := ctx.Value(callKey{}).(*call)
	return c
}

// current is the innermost span of the call, which is the call's own span
// until a service opens a child.
func current(ctx context.Context) *Span {
	if s, ok := ctx.Value(currentKey{}).(*Span); ok {
		return s
	}
	if c := inFlight(ctx); c != nil {
		return c.span
	}
	return nil
}

// FromContext is the provider serving this call, nil outside one.
func FromContext(ctx context.Context) *Provider {
	if c := inFlight(ctx); c != nil {
		return c.provider
	}
	return nil
}

// Start opens a child span of the call in flight, for the two operations that
// are worth their own span: a build and a Process start. The child inherits
// the call's user and tool, so a query for what pkg_build did finds the build
// span and the build log record as well as the tool span. Without a call in
// flight it returns a Span that does nothing, so a service never has to ask.
func Start(ctx context.Context, name string) (context.Context, *Span) {
	c := inFlight(ctx)
	if c == nil {
		return ctx, nil
	}
	ctx, span := c.provider.tracer.Start(ctx, name)
	s := &Span{span: span, provider: c.provider}
	ctx = context.WithValue(ctx, currentKey{}, s)
	s.ctx = ctx
	s.Set(AttrUser, c.user)
	s.Set(AttrTool, c.tool)
	s.Set(AttrCaller, c.provider.caller)
	return ctx, s
}

// Span is one span and the attributes put on it. It keeps its own copy of them
// so a log record can carry the same set, which the OpenTelemetry span
// interface does not offer a way to read back.
//
// A nil Span is a working Span that records nothing.
type Span struct {
	ctx      context.Context
	span     trace.Span
	provider *Provider

	mu    sync.Mutex
	attrs []attribute.KeyValue
}

// Set records one attribute. An empty value is not an attribute.
func (s *Span) Set(key, value string) {
	if s == nil || value == "" {
		return
	}
	kv := attribute.String(key, value)
	s.mu.Lock()
	s.attrs = append(s.attrs, kv)
	s.mu.Unlock()
	s.span.SetAttributes(kv)
}

// SetPackage records the Package path this span is about.
func (s *Span) SetPackage(path string) { s.Set(AttrPackage, path) }

// SetProcess records the Process id this span is about.
func (s *Span) SetProcess(id string) { s.Set(AttrProcess, id) }

// SetPath records the Files path this span is about.
func (s *Span) SetPath(path string) { s.Set(AttrPath, path) }

// SetDigest records the OCI digest this span built or ran.
func (s *Span) SetDigest(digest string) { s.Set(AttrDigest, digest) }

// Fail marks the span as an error and records the problem's slug, which is the
// error class the agent saw.
func (s *Span) Fail(slug, title string) {
	if s == nil {
		return
	}
	s.Set(AttrError, slug)
	s.span.SetStatus(codes.Error, title)
}

// OK marks the span as a call that did what it said.
func (s *Span) OK() {
	if s == nil {
		return
	}
	s.span.SetStatus(codes.Ok, "")
}

// End closes the span.
func (s *Span) End() {
	if s == nil {
		return
	}
	s.span.End()
}

// Info emits a log record under this span with the span's own attributes.
func (s *Span) Info(body string) { s.emit(otellog.SeverityInfo, "INFO", body) }

// Error emits a log record under this span with the span's own attributes.
func (s *Span) Error(body string) { s.emit(otellog.SeverityError, "ERROR", body) }

func (s *Span) emit(severity otellog.Severity, text, body string) {
	if s == nil {
		return
	}
	var record otellog.Record
	record.SetTimestamp(time.Now())
	record.SetSeverity(severity)
	record.SetSeverityText(text)
	record.SetBody(attribute.StringValue(body))
	s.mu.Lock()
	record.AddAttributes(s.attrs...)
	s.mu.Unlock()
	// The context carries this span, so the record lands on the same trace.
	s.provider.logger.Emit(s.ctx, record)
}

// SetPackage records the Package path on the call in flight. It is a no op
// outside a call, so a service calls it without asking whether it is traced.
func SetPackage(ctx context.Context, path string) { set(ctx, AttrPackage, path) }

// SetProcess records the Process id on the call in flight.
func SetProcess(ctx context.Context, id string) { set(ctx, AttrProcess, id) }

// SetPath records the Files path on the call in flight.
func SetPath(ctx context.Context, path string) { set(ctx, AttrPath, path) }

// SetApproval records the approval this call executed.
func SetApproval(ctx context.Context, id string) { set(ctx, AttrApproval, id) }

// SetRequester records the member an approved call was executed for.
func SetRequester(ctx context.Context, user string) { set(ctx, AttrRequester, user) }

// set puts one of the four attributes on the span in hand and on the span of
// the whole call. They describe the call, not one step of it: a query for
// everything a Package did has to find the tool span, not only the child span
// that happened to learn the Package's name first.
func set(ctx context.Context, key, value string) {
	if value == "" {
		return
	}
	span := current(ctx)
	if span == nil {
		// SpanFromContext returns a span that records nothing when there is
		// none, which is exactly the no op this promises.
		trace.SpanFromContext(ctx).SetAttributes(attribute.String(key, value))
		return
	}
	span.Set(key, value)
	if c := inFlight(ctx); c != nil && c.span != span {
		c.span.Set(key, value)
	}
}
