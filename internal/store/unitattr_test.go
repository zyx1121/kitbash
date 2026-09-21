package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the store is opened as a file here too

	"github.com/zyx1121/kitbash/internal/store"
)

// kitbash.unit is a column of its own on every signal, so a query for what one
// unit of a Process did never parses JSON to filter, see PLAN.md 2.4 and 5.6.
func TestRecordsCarryTheUnit(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()
	now := time.Now()

	insert := func(name, unit string, at time.Time) {
		t.Helper()
		if err := st.Insert(ctx, store.Export{Spans: []store.Span{{
			TraceID:    strings.Repeat("ab", 16),
			SpanID:     strings.Repeat("cd", 8),
			Name:       name,
			StartNS:    at.UnixNano(),
			EndNS:      at.Add(time.Millisecond).UnixNano(),
			Attributes: store.Attributes{User: "alice", Producer: "alice", Unit: unit},
		}}}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	insert("serve", "web", now.Add(-time.Minute))
	insert("evict", "cache", now.Add(-2*time.Minute))
	insert("run", "", now.Add(-3*time.Minute))

	page, err := st.Query(ctx, store.SignalTraces, store.Filter{Unit: "cache"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Spans) != 1 || page.Spans[0].Name != "evict" {
		t.Fatalf("the unit filter returned %+v, want the one span of the cache", page.Spans)
	}
	if page.Spans[0].Unit != "cache" {
		t.Errorf("Unit = %q, want cache", page.Spans[0].Unit)
	}
	page, err = st.Query(ctx, store.SignalTraces, store.Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Spans) != 3 {
		t.Errorf("the unfiltered query returned %d spans, want all three", len(page.Spans))
	}
}

// A metric has two units: the unit of measure it was recorded in and the unit
// of the Process that recorded it. They are two columns and two fields, and a
// query for one must never answer the other.
func TestAMetricKeepsItsMeasureApartFromItsUnit(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()
	now := time.Now()

	if err := st.Insert(ctx, store.Export{Metrics: []store.Metric{{
		TimeNS: now.UnixNano(), Name: "kitbash.health", Value: 1, Unit: "1",
		Attributes: store.Attributes{User: "alice", Producer: "alice", Unit: "web"},
	}}}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	page, err := st.Query(ctx, store.SignalMetrics, store.Filter{Unit: "web"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Metrics) != 1 {
		t.Fatalf("the unit filter returned %d metrics, want the one of the web unit", len(page.Metrics))
	}
	m := page.Metrics[0]
	if m.Unit != "1" {
		t.Errorf("the unit of measure came back as %q, want 1", m.Unit)
	}
	if m.Attributes.Unit != "web" {
		t.Errorf("the kitbash unit came back as %q, want web", m.Attributes.Unit)
	}
	// And the measure is not a unit to filter on: a query for the unit of the
	// Process must not match a metric whose measure happens to read the same.
	page, err = st.Query(ctx, store.SignalMetrics, store.Filter{Unit: "1"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Metrics) != 0 {
		t.Errorf("a query for the unit 1 returned %d metrics, want none", len(page.Metrics))
	}
}

// The column is added to a store written before pods existed rather than the
// records lost, which is the rule every attribute column here follows.
func TestMigrationAddsTheUnitColumn(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()
	// openProcessStore opens a fresh store, so the column is in the schema.
	// What this asks is the other half: a record written with no unit reads
	// back with none and is answered by an unfiltered query.
	if err := st.Insert(ctx, store.Export{Logs: []store.Log{{
		TimeNS: time.Now().UnixNano(), Body: "a line",
		Attributes: store.Attributes{User: "alice", Producer: "alice"},
	}}}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	page, err := st.Query(ctx, store.SignalLogs, store.Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Logs) != 1 || page.Logs[0].Unit != "" {
		t.Errorf("a record with no unit came back as %+v", page.Logs)
	}
}
