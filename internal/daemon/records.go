package daemon

import (
	"time"

	"github.com/zyx1121/kitbash/internal/store"
)

// timeLayout is RFC 3339 with every nanosecond digit kept. time.RFC3339Nano
// drops trailing zeros, which would make two records of the same second look
// like they carry different precision.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// spanRecord, logRecord and metricRecord are the shapes of the tel_query
// output, defined in the $defs of spec/mcp-surface.yaml.
type spanRecord struct {
	TraceID       string           `json:"traceId"`
	SpanID        string           `json:"spanId"`
	ParentSpanID  string           `json:"parentSpanId,omitempty"`
	Name          string           `json:"name"`
	Start         string           `json:"start"`
	End           string           `json:"end"`
	DurationMs    float64          `json:"durationMs"`
	Status        string           `json:"status"`
	StatusMessage string           `json:"statusMessage,omitempty"`
	Attributes    store.Attributes `json:"attributes"`
	Other         map[string]any   `json:"other,omitempty"`
}

type logRecord struct {
	Time       string           `json:"time"`
	Severity   string           `json:"severity"`
	Body       string           `json:"body"`
	TraceID    string           `json:"traceId,omitempty"`
	SpanID     string           `json:"spanId,omitempty"`
	Attributes store.Attributes `json:"attributes"`
	Other      map[string]any   `json:"other,omitempty"`
}

type metricRecord struct {
	Time       string           `json:"time"`
	Name       string           `json:"name"`
	Value      float64          `json:"value"`
	Unit       string           `json:"unit,omitempty"`
	Attributes store.Attributes `json:"attributes"`
	Other      map[string]any   `json:"other,omitempty"`
}

// records renders one page of the store as the records the surface publishes.
// It always returns a slice, so an empty page is [] and not null.
func records(page store.Page) []any {
	out := make([]any, 0, len(page.Spans)+len(page.Logs)+len(page.Metrics))
	for _, sp := range page.Spans {
		out = append(out, spanRecord{
			TraceID:       sp.TraceID,
			SpanID:        sp.SpanID,
			ParentSpanID:  sp.ParentSpanID,
			Name:          sp.Name,
			Start:         stamp(sp.StartNS),
			End:           stamp(sp.EndNS),
			DurationMs:    float64(sp.EndNS-sp.StartNS) / float64(time.Millisecond),
			Status:        sp.Status,
			StatusMessage: sp.StatusMessage,
			Attributes:    sp.Attributes,
			Other:         sp.Other,
		})
	}
	for _, l := range page.Logs {
		out = append(out, logRecord{
			Time:       stamp(l.TimeNS),
			Severity:   l.Severity,
			Body:       l.Body,
			TraceID:    l.TraceID,
			SpanID:     l.SpanID,
			Attributes: l.Attributes,
			Other:      l.Other,
		})
	}
	for _, m := range page.Metrics {
		out = append(out, metricRecord{
			Time:       stamp(m.TimeNS),
			Name:       m.Name,
			Value:      m.Value,
			Unit:       m.Unit,
			Attributes: m.Attributes,
			Other:      m.Other,
		})
	}
	return out
}

// stamp renders a stored unix nanosecond time as UTC.
func stamp(ns int64) string {
	return time.Unix(0, ns).UTC().Format(timeLayout)
}
