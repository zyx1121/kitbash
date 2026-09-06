package telemetry

import (
	"context"
	"log"
	"sync"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// notice keeps the promise in spec/kitbashd-api.yaml that a missing kitbashd
// costs one line per session and nothing else. The first export that fails
// writes that line; every later record is dropped in silence, because a socket
// that is absent at the start of a session is absent for all of it and an
// agent's session is not the place to learn that.
type notice struct {
	socket string
	logger *log.Logger

	mu      sync.Mutex
	dropped bool
}

// drop reports whether records are being dropped, logging the reason once.
func (n *notice) drop(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.dropped {
		return
	}
	n.dropped = true
	n.logger.Printf("telemetry: dropping records for this session: %s: %v", n.socket, err)
}

func (n *notice) dropping() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dropped
}

// spanGuard wraps the OTLP span exporter so an export failure never reaches
// the SDK's error handler, which would be one line per batch.
type spanGuard struct {
	next   sdktrace.SpanExporter
	notice *notice
}

func (g *spanGuard) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if g.notice.dropping() {
		return nil
	}
	if err := g.next.ExportSpans(ctx, spans); err != nil {
		g.notice.drop(err)
	}
	return nil
}

func (g *spanGuard) Shutdown(ctx context.Context) error { return g.next.Shutdown(ctx) }

// logGuard is spanGuard for log records, sharing the same one line per session.
type logGuard struct {
	next   sdklog.Exporter
	notice *notice
}

func (g *logGuard) Export(ctx context.Context, records []sdklog.Record) error {
	if g.notice.dropping() {
		return nil
	}
	if err := g.next.Export(ctx, records); err != nil {
		g.notice.drop(err)
	}
	return nil
}

func (g *logGuard) Shutdown(ctx context.Context) error { return g.next.Shutdown(ctx) }

func (g *logGuard) ForceFlush(ctx context.Context) error { return g.next.ForceFlush(ctx) }
