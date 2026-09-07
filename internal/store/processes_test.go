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

// open is a store in a temporary directory, closed when the test ends.
func openProcessStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "kitbashd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func process(id, owner string, admin bool) store.Process {
	return store.Process{
		ID:            id,
		Owner:         owner,
		Admin:         admin,
		Package:       "/home/" + owner + "/echo",
		Name:          "echo",
		Expose:        "http",
		Endpoint:      "http://127.0.0.1:40275",
		Subscriptions: []string{store.SubscriptionTelemetry},
		RegisteredAt:  time.Now().UTC().Truncate(time.Millisecond),
	}
}

const (
	idOne = "0192f2c0-1234-7abc-8def-0123456789ab"
	idTwo = "0192f2c0-1234-7abc-8def-0123456789ac"
)

// TestProcessTokensAreStoredAsHashes is the rule the whole receiver rests on:
// the store never holds a value that could be replayed as a token.
func TestProcessTokensAreStoredAsHashes(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()

	token, hash, err := store.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if len(token) < 40 || strings.ContainsAny(token, "+/=") {
		t.Errorf("token = %q, want at least 32 bytes of entropy in base64url", token)
	}
	if hash == token || store.HashToken(token) != hash {
		t.Errorf("hash = %q, want the SHA-256 of the token", hash)
	}
	if err := st.RegisterProcess(ctx, process(idOne, "alice", false), hash, 0); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}

	got, found, err := st.ProcessByToken(ctx, token)
	if err != nil || !found {
		t.Fatalf("ProcessByToken: %v, found %v", err, found)
	}
	if got.ID != idOne || got.Owner != "alice" || !got.Subscribes() {
		t.Errorf("process = %+v", got)
	}
	if _, found, err := st.ProcessByToken(ctx, "not-a-token"); err != nil || found {
		t.Errorf("an unknown token found %v, %v", found, err)
	}
}

// TestRegisterReplacesForTheSameOwner is the proc_run again case: the record is
// replaced, the old token stops working, and no second row appears.
func TestRegisterReplacesForTheSameOwner(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()

	first, firstHash, _ := store.NewToken()
	if err := st.RegisterProcess(ctx, process(idOne, "alice", false), firstHash, 0); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}
	second, secondHash, _ := store.NewToken()
	if err := st.RegisterProcess(ctx, process(idOne, "alice", true), secondHash, 0); err != nil {
		t.Fatalf("RegisterProcess again: %v", err)
	}

	if _, found, _ := st.ProcessByToken(ctx, first); found {
		t.Error("the first token still names a Process; a replacement must revoke it")
	}
	got, found, _ := st.ProcessByToken(ctx, second)
	if !found || !got.Admin {
		t.Errorf("the second token names %+v, found %v", got, found)
	}
	list, err := st.Processes(ctx, "")
	if err != nil {
		t.Fatalf("Processes: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("processes = %d, want the one replaced record", len(list))
	}
}

// TestRegisterRefusesAnotherOwnersID keeps two members' Processes apart even
// when one of them names an id the other holds.
func TestRegisterRefusesAnotherOwnersID(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()

	_, hash, _ := store.NewToken()
	if err := st.RegisterProcess(ctx, process(idOne, "alice", false), hash, 0); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}
	_, otherHash, _ := store.NewToken()
	err := st.RegisterProcess(ctx, process(idOne, "bob", false), otherHash, 0)
	if err == nil || !strings.Contains(err.Error(), "another member") {
		t.Fatalf("registering another owner's id returned %v, want a conflict", err)
	}
	got, _, _ := st.Process(ctx, idOne)
	if got.Owner != "alice" {
		t.Errorf("owner = %q, want the refused registration to have changed nothing", got.Owner)
	}
}

// TestProcessesListAndDelete covers what the API answers: one member's
// Processes, everyone's, and a delete that revokes.
func TestProcessesListAndDelete(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()

	aliceToken, aliceHash, _ := store.NewToken()
	if err := st.RegisterProcess(ctx, process(idOne, "alice", false), aliceHash, 0); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}
	_, bobHash, _ := store.NewToken()
	if err := st.RegisterProcess(ctx, process(idTwo, "bob", false), bobHash, 0); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}

	mine, err := st.Processes(ctx, "alice")
	if err != nil {
		t.Fatalf("Processes: %v", err)
	}
	if len(mine) != 1 || mine[0].ID != idOne {
		t.Errorf("alice sees %+v, want her own Process only", mine)
	}
	all, err := st.Processes(ctx, "")
	if err != nil {
		t.Fatalf("Processes: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("processes = %d, want both", len(all))
	}

	removed, err := st.DeleteProcess(ctx, idOne)
	if err != nil || !removed {
		t.Fatalf("DeleteProcess: %v, removed %v", err, removed)
	}
	if _, found, _ := st.ProcessByToken(ctx, aliceToken); found {
		t.Error("the token still names a Process after it was unregistered")
	}
	removed, err = st.DeleteProcess(ctx, idOne)
	if err != nil || removed {
		t.Errorf("deleting again returned removed %v, %v", removed, err)
	}
}

// TestRecordsCarryTheProducer is the column the fan out and the query surface
// both read: it round trips and it filters.
func TestRecordsCarryTheProducer(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()
	now := time.Now()

	insert := func(producer string, at time.Time) {
		t.Helper()
		if err := st.Insert(ctx, store.Export{Spans: []store.Span{{
			TraceID:    strings.Repeat("ab", 16),
			SpanID:     strings.Repeat("cd", 8),
			Name:       "tools/call",
			StartNS:    at.UnixNano(),
			EndNS:      at.Add(time.Millisecond).UnixNano(),
			Attributes: store.Attributes{User: "alice", Producer: producer},
		}}}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	insert("alice", now.Add(-time.Minute))
	insert(idOne, now.Add(-2*time.Minute))

	page, err := st.Query(ctx, store.SignalTraces, store.Filter{Producer: idOne})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Spans) != 1 || page.Spans[0].Producer != idOne {
		t.Fatalf("producer filter returned %+v", page.Spans)
	}
}

// TestMigrationAddsTheProducerColumn is the upgrade path from a store written
// before M4: the column is added rather than the records lost.
func TestMigrationAddsTheProducerColumn(t *testing.T) {
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
		if _, err := db.Exec("DROP INDEX IF EXISTS " + table + "_producer"); err != nil {
			t.Fatalf("drop the index: %v", err)
		}
		if _, err := db.Exec("ALTER TABLE " + table + " DROP COLUMN producer"); err != nil {
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
	if err := st.Insert(ctx, store.Export{Logs: []store.Log{{
		TimeNS:     at.UnixNano(),
		Body:       "built",
		Attributes: store.Attributes{User: "alice", Producer: "alice"},
	}}}); err != nil {
		t.Fatalf("Insert after the migration: %v", err)
	}
	page, err := st.Query(ctx, store.SignalLogs, store.Filter{Producer: "alice"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Logs) != 1 {
		t.Fatalf("logs = %d, want the record written after the migration", len(page.Logs))
	}
}
