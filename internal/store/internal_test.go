package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// internalSeed writes one log record that is the cause of an internal problem
// and one that is an ordinary record of the same member.
func internalSeed(t *testing.T, s *Store) {
	t.Helper()
	err := s.Insert(context.Background(), Export{Logs: []Log{
		{TimeNS: base.Add(-time.Hour).UnixNano(), Severity: "ERROR", Body: "git: exit status 128",
			Attributes: Attributes{User: "alice", Producer: "kitbashd", Path: "/org/handbook",
				Internal: boolPtr(true)}},
		{TimeNS: base.Add(-time.Hour).UnixNano(), Severity: "INFO", Body: "built /home/alice/tool",
			Attributes: Attributes{User: "alice", Producer: "alice"}},
	}})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
}

// The whole point of the flag is that it survives the round trip and can be
// filtered on, in both directions.
func TestInternalIsATypedColumn(t *testing.T) {
	s := open(t)
	internalSeed(t, s)
	window := Filter{Since: base.Add(-24 * time.Hour), Until: base}

	all, err := s.Query(context.Background(), SignalLogs, window)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(all.Logs) != 2 {
		t.Fatalf("an unfiltered query returned %d records, want both", len(all.Logs))
	}

	only := window
	only.Internal = boolPtr(true)
	causes, err := s.Query(context.Background(), SignalLogs, only)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(causes.Logs) != 1 {
		t.Fatalf("internal true returned %d records, want the one cause", len(causes.Logs))
	}
	if causes.Logs[0].Internal == nil || !*causes.Logs[0].Internal {
		t.Errorf("the stored record does not carry internal true")
	}
	if causes.Logs[0].Body != "git: exit status 128" {
		t.Errorf("body = %q, want the cause", causes.Logs[0].Body)
	}

	without := window
	without.Internal = boolPtr(false)
	rest, err := s.Query(context.Background(), SignalLogs, without)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// A record that is not a cause carries no internal at all, so this is the
	// case a plain "internal = 0" would answer with nothing.
	if len(rest.Logs) != 1 {
		t.Fatalf("internal false returned %d records, want the one ordinary record", len(rest.Logs))
	}
	if rest.Logs[0].Internal != nil {
		t.Errorf("the ordinary record carries internal %v, want none", *rest.Logs[0].Internal)
	}
}

// A store written before the column existed is opened, migrated and queried
// rather than refused.
func TestInternalIsAddedToAStoreThatPredatesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kitbashd.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, table := range []string{"spans", "logs", "metrics"} {
		if _, err := s.db.Exec("DROP INDEX " + table + "_internal"); err != nil {
			t.Fatalf("dropping the index of %s: %v", table, err)
		}
		if _, err := s.db.Exec("ALTER TABLE " + table + " DROP COLUMN internal"); err != nil {
			t.Fatalf("dropping the column from %s: %v", table, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopening the store: %v", err)
	}
	defer again.Close()
	internalSeed(t, again)
	page, err := again.Query(context.Background(), SignalLogs, Filter{
		Since: base.Add(-24 * time.Hour), Until: base, Internal: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Logs) != 1 {
		t.Fatalf("the migrated store returned %d causes, want 1", len(page.Logs))
	}
}
