package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite" // this test opens the store file directly

	"github.com/zyx1121/kitbash/internal/store"
)

// TestProcessKeepsTheLimitsItWasStartedWith is the store's half of the
// enforcement: the cgroup filesystem is empty after a reboot, so the ceiling a
// Process runs under has to be somewhere that survives one. It is here.
func TestProcessKeepsTheLimitsItWasStartedWith(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()
	token, hash, err := store.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	want := store.Limits{Memory: "512Mi", CPU: "0.5", Pids: 512}
	if err := st.RegisterProcess(ctx, store.Process{
		ID:           "01930000-0000-7000-8000-000000000011",
		Owner:        "tester",
		Package:      "/org/workflow",
		Name:         "workflow",
		Expose:       "none",
		Limits:       want,
		RegisteredAt: time.Now().UTC(),
	}, hash, store.Quota{}); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}

	got, found, err := st.ProcessByToken(ctx, token)
	if err != nil || !found {
		t.Fatalf("ProcessByToken: %v, found %v", err, found)
	}
	if got.Limits != want {
		t.Errorf("the limits are %+v, want %+v as the start received them", got.Limits, want)
	}
	list, err := st.Processes(ctx, "tester")
	if err != nil {
		t.Fatalf("Processes: %v", err)
	}
	if len(list) != 1 || list[0].Limits != want {
		t.Errorf("the listed limits are %+v, want %+v: what a Process may spend is not a secret from its owner",
			list, want)
	}
}

// A registration written before the ceiling was recorded carries none, and
// reads back as a Process with no limits rather than as a failure. Restore
// brings it back without a ceiling and says so.
func TestAProcessRegisteredBeforeLimitsReadsBackWithNone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kitbashd.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	ctx := context.Background()
	_, hash, err := store.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	id := "01930000-0000-7000-8000-000000000012"
	if err := st.RegisterProcess(ctx, store.Process{
		ID:           id,
		Owner:        "tester",
		Package:      "/org/workflow",
		Name:         "workflow",
		Expose:       "none",
		Limits:       store.Limits{Memory: "512Mi"},
		RegisteredAt: time.Now().UTC(),
	}, hash, store.Quota{}); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Put the file back the way a release without the column left it: the
	// registration is there and the column is not.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open the store file directly: %v", err)
	}
	if _, err := db.Exec("ALTER TABLE processes DROP COLUMN limits"); err != nil {
		t.Fatalf("drop the column: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st, err = store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()
	got, found, err := st.Process(ctx, id)
	if err != nil || !found {
		t.Fatalf("Process: %v, found %v", err, found)
	}
	if !got.Limits.Empty() {
		t.Errorf("the limits are %+v, want none for a registration written before them", got.Limits)
	}
}
