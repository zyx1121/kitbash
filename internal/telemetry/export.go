package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/protobuf/proto"

	collogs "github.com/zyx1121/kitbash/internal/otlpproto/collector/logs/v1"
	coltrace "github.com/zyx1121/kitbash/internal/otlpproto/collector/trace/v1"
)

// notice keeps the promise in spec/kitbashd-api.yaml that a missing kitbashd
// costs one line per session and nothing else. It is a state, not a switch:
// the first failed export writes the line and the ones after it are silent,
// but every batch is still offered to the socket. A daemon restarted under a
// live session is exported to again, and says so once.
type notice struct {
	socket string
	logger *log.Logger

	mu      sync.Mutex
	failing bool
}

// failed records one export that did not land, logging the reason on the
// first failure of a run.
func (n *notice) failed(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.failing {
		return
	}
	n.failing = true
	n.logger.Printf("telemetry: dropping records: %s: %v", n.socket, err)
}

// succeeded records one export that landed, logging once when a run of
// failures ends.
func (n *notice) succeeded() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.failing {
		return
	}
	n.failing = false
	n.logger.Printf("telemetry: export resumed: %s", n.socket)
}

// spanGuard wraps the OTLP span exporter so an export failure never reaches
// the SDK's error handler, which would be one line per batch, and never
// reaches the caller, whose call succeeded either way.
type spanGuard struct {
	next   sdktrace.SpanExporter
	notice *notice
}

func (g *spanGuard) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if err := g.next.ExportSpans(ctx, spans); err != nil {
		g.notice.failed(err)
		return nil
	}
	g.notice.succeeded()
	return nil
}

func (g *spanGuard) Shutdown(ctx context.Context) error { return g.next.Shutdown(ctx) }

// logGuard is spanGuard for log records, sharing the same one line per run of
// failures.
type logGuard struct {
	next   sdklog.Exporter
	notice *notice
}

func (g *logGuard) Export(ctx context.Context, records []sdklog.Record) error {
	if err := g.next.Export(ctx, records); err != nil {
		g.notice.failed(err)
		return nil
	}
	g.notice.succeeded()
	return nil
}

func (g *logGuard) Shutdown(ctx context.Context) error { return g.next.Shutdown(ctx) }

func (g *logGuard) ForceFlush(ctx context.Context) error { return g.next.ForceFlush(ctx) }

// The standard OTLP/HTTP paths of spec/kitbashd-api.yaml. The host in the URL
// is never resolved: every connection dials the unix socket, see socketClient.
const (
	tracesURL = "http://localhost/v1/traces"
	logsURL   = "http://localhost/v1/logs"
)

// contentTypeProtobuf is the encoding this process exports in. It is spelled
// here rather than taken from internal/otlp, which is the receiver's side and
// would drag the store into kitbash-mcp.
const contentTypeProtobuf = "application/x-protobuf"

// maxDiscardBytes bounds what is read off a response whose body nobody wants.
// Reading it is what lets the connection be used again; reading an unbounded
// one would let a broken daemon spend this process's memory.
const maxDiscardBytes = 1 << 20

// poster POSTs one OTLP export request. The two exporters below are this plus
// the transformation of what the SDK hands them, which is the whole of what
// the SDK's own OTLP exporters do that kitbash needs: no gRPC, no retry queue,
// no compression, see PLAN.md section 2.7.
//
// Retrying is deliberately absent. The peer is a local daemon on a unix
// socket: it is either there or it is not, and a retry only delays the drop
// the session is told about once.
type poster struct {
	url    string
	client *http.Client

	mu      sync.RWMutex
	stopped bool
}

// post sends one request message. A shutdown exporter drops the batch and
// reports success. The SDK's own exporters answer with an error instead, which
// the guard above would turn into the line a dropping session is told about,
// over a batch that arrived after the last flush: the session is over by then
// and nothing an agent could read is still listening.
func (p *poster) post(ctx context.Context, message proto.Message) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.stopped {
		return nil
	}
	body, err := proto.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode export request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentTypeProtobuf)
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// kitbashd answers a stored request with an empty success body, so the
	// body is read only to hand the connection back to the pool.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDiscardBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s answered %s", p.url, resp.Status)
	}
	return nil
}

// shutdown stops sending. It is safe to call twice, which the SDK does when a
// provider is shut down after a flush failed.
func (p *poster) shutdown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
}

// spanExporter is the sdktrace.SpanExporter of one session.
type spanExporter struct{ poster }

func newSpanExporter(client *http.Client) *spanExporter {
	return &spanExporter{poster{url: tracesURL, client: client}}
}

func (e *spanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	resources := resourceSpans(spans)
	if len(resources) == 0 {
		return nil
	}
	return e.post(ctx, &coltrace.ExportTraceServiceRequest{ResourceSpans: resources})
}

func (e *spanExporter) Shutdown(context.Context) error {
	e.shutdown()
	return nil
}

// logExporter is the sdklog.Exporter of the same session.
type logExporter struct{ poster }

func newLogExporter(client *http.Client) *logExporter {
	return &logExporter{poster{url: logsURL, client: client}}
}

func (e *logExporter) Export(ctx context.Context, records []sdklog.Record) error {
	resources := resourceLogs(records)
	if len(resources) == 0 {
		return nil
	}
	return e.post(ctx, &collogs.ExportLogsServiceRequest{ResourceLogs: resources})
}

func (e *logExporter) Shutdown(context.Context) error {
	e.shutdown()
	return nil
}

// ForceFlush is nothing to do: this exporter holds no records of its own, the
// batch processor above it does.
func (e *logExporter) ForceFlush(context.Context) error { return nil }
