package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the migration test opens the store file directly

	"github.com/zyx1121/kitbash/internal/store"
)

// caller is the Process whose MCP session recorded a span, see PLAN.md 2.3.
const caller = "01a07f95-df01-7073-add4-29e769a336f4"

// TestRecordsCarryTheCaller is the column that answers what a Process did on
// its owner's behalf: it round trips and it filters.
func TestRecordsCarryTheCaller(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()
	now := time.Now()

	insert := func(name, by string, at time.Time) {
		t.Helper()
		if err := st.Insert(ctx, store.Export{Spans: []store.Span{{
			TraceID:    strings.Repeat("ab", 16),
			SpanID:     strings.Repeat("cd", 8),
			Name:       name,
			StartNS:    at.UnixNano(),
			EndNS:      at.Add(time.Millisecond).UnixNano(),
			Attributes: store.Attributes{User: "alice", Producer: "alice", Caller: by},
		}}}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	insert("fs_list", "", now.Add(-time.Minute))
	insert("fs_write", caller, now.Add(-2*time.Minute))

	page, err := st.Query(ctx, store.SignalTraces, store.Filter{Caller: caller})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Spans) != 1 || page.Spans[0].Name != "fs_write" {
		t.Fatalf("caller filter returned %+v, want the one span a Process recorded", page.Spans)
	}
	if page.Spans[0].Caller != caller {
		t.Errorf("Caller = %q, want %q", page.Spans[0].Caller, caller)
	}

	page, err = st.Query(ctx, store.SignalTraces, store.Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Spans) != 2 {
		t.Fatalf("the unfiltered query returned %d spans, want both", len(page.Spans))
	}
}

// TestMigrationAddsTheCallerColumn is the upgrade path from a store written
// before M6: the column is added rather than the records lost.
func TestMigrationAddsTheCallerColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kitbashd.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Put the file back the way a version without the column left it.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open the store file directly: %v", err)
	}
	for _, table := range []string{"spans", "logs", "metrics"} {
		if _, err := db.Exec("DROP INDEX IF EXISTS " + table + "_caller"); err != nil {
			t.Fatalf("drop the index: %v", err)
		}
		if _, err := db.Exec("ALTER TABLE " + table + " DROP COLUMN caller"); err != nil {
			t.Fatalf("drop the column: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st, err = store.Open(path)
	if err != nil {
		t.Fatalf("reopen a store written without the column: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	at := time.Now().Add(-time.Minute)
	if err := st.Insert(ctx, store.Export{Spans: []store.Span{{
		TraceID:    strings.Repeat("ab", 16),
		SpanID:     strings.Repeat("cd", 8),
		Name:       "fs_write",
		StartNS:    at.UnixNano(),
		EndNS:      at.Add(time.Millisecond).UnixNano(),
		Attributes: store.Attributes{User: "alice", Caller: caller},
	}}}); err != nil {
		t.Fatalf("Insert after the migration: %v", err)
	}
	page, err := st.Query(ctx, store.SignalTraces, store.Filter{Caller: caller})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Spans) != 1 {
		t.Fatalf("the migrated store returned %+v", page.Spans)
	}
}
