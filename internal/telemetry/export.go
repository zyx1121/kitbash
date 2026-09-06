package telemetry

import (
	"context"
	"log"
	"sync"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
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
