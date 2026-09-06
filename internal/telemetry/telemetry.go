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

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
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
// SSH session is over by then and nobody is waiting for the records.
const ShutdownTimeout = 3 * time.Second

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

// Options configure a Provider.
type Options struct {
	// Version is the kitbash-mcp version, exported as service.version.
	Version string
	// Socket is the kitbashd socket. Empty means SocketPath.
	Socket string
	// Logger receives the one line a dropping session is worth. Empty means
	// the same stderr logger the rest of kitbash writes to.
	Logger *log.Logger
}

// Provider owns the exporters of one session.
type Provider struct {
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

	// The endpoint host is never resolved, see socketClient. It is here
	// because the exporter needs a URL to build the standard OTLP paths on.
	spans, err := otlptracehttp.New(context.Background(),
		otlptracehttp.WithHTTPClient(client),
		otlptracehttp.WithEndpoint("localhost"),
		otlptracehttp.WithInsecure(),
		// Retrying a socket that is not there only delays the drop.
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}),
		otlptracehttp.WithTimeout(requestTimeout),
	)
	if err != nil {
		return nil, err
	}
	records, err := otlploghttp.New(context.Background(),
		otlploghttp.WithHTTPClient(client),
		otlploghttp.WithEndpoint("localhost"),
		otlploghttp.WithInsecure(),
		otlploghttp.WithRetry(otlploghttp.RetryConfig{Enabled: false}),
		otlploghttp.WithTimeout(requestTimeout),
	)
	if err != nil {
		_ = spans.Shutdown(context.Background())
		return nil, err
	}

	version := opts.Version
	if version == "" {
		version = "dev"
	}
	res := resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceName(ServiceName),
		semconv.ServiceVersion(version),
	)
	traces := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(&spanGuard{next: spans, notice: drops}),
		sdktrace.WithResource(res),
	)
	logs := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(&logGuard{next: records, notice: drops})),
		sdklog.WithResource(res),
	)
	return &Provider{
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

// Shutdown flushes what is batched and closes the exporters. It is bounded by
// ShutdownTimeout however much time the caller offers.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, ShutdownTimeout)
	defer cancel()
	err := p.traces.Shutdown(ctx)
	if logErr := p.logs.Shutdown(ctx); err == nil {
		err = logErr
	}
	return err
}

// StartTool opens the span of one tools/call. The returned context carries
// both the span and this provider, so a service can add attributes, open a
// child span and emit a log record without holding either.
func (p *Provider) StartTool(ctx context.Context, tool, user string) (context.Context, *Span) {
	if p == nil {
		return ctx, nil
	}
	ctx = context.WithValue(ctx, providerKey{}, p)
	ctx, span := p.tracer.Start(ctx, tool)
	s := &Span{ctx: ctx, span: span, provider: p}
	s.Set(AttrUser, user)
	s.Set(AttrTool, tool)
	return ctx, s
}

// providerKey carries the Provider through a call's context.
type providerKey struct{}

// FromContext is the provider serving this call, nil outside one.
func FromContext(ctx context.Context) *Provider {
	p, _ := ctx.Value(providerKey{}).(*Provider)
	return p
}

// Start opens a child span of the call in flight, for the two operations that
// are worth their own span: a build and a Process start. Without a call in
// flight it returns a Span that does nothing, so a service never has to ask.
func Start(ctx context.Context, name string) (context.Context, *Span) {
	p := FromContext(ctx)
	if p == nil {
		return ctx, nil
	}
	ctx, span := p.tracer.Start(ctx, name)
	return ctx, &Span{ctx: ctx, span: span, provider: p}
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

func set(ctx context.Context, key, value string) {
	if value == "" {
		return
	}
	// SpanFromContext returns a span that records nothing when there is none,
	// which is exactly the no op this promises.
	trace.SpanFromContext(ctx).SetAttributes(attribute.String(key, value))
}
