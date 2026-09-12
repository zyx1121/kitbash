package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // this test opens the store file directly

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/store"
)

// TestProcessKeepsThePermitsItWasRegisteredWith is the store's half of the
// narrowing: the block travels with the registration, so an MCP session is
// given what the Process was run with and not what its Package says today.
func TestProcessKeepsThePermitsItWasRegisteredWith(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()
	token, hash, err := store.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	want := manifest.Permits{Tools: []string{"fs_read", "*_*"}, Paths: []string{"/org", "/home/*"}}
	if err := st.RegisterProcess(ctx, store.Process{
		ID:           "01930000-0000-7000-8000-000000000001",
		Owner:        "tester",
		Package:      "/org/workflow",
		Name:         "workflow",
		Expose:       "mcp",
		Permits:      want,
		RegisteredAt: time.Now().UTC(),
	}, hash, 0); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}

	got, found, err := st.ProcessByToken(ctx, token)
	if err != nil || !found {
		t.Fatalf("ProcessByToken: %v, found %v", err, found)
	}
	if strings.Join(got.Permits.Tools, ",") != strings.Join(want.Tools, ",") ||
		strings.Join(got.Permits.Paths, ",") != strings.Join(want.Paths, ",") {
		t.Errorf("permits are %+v, want %+v", got.Permits, want)
	}

	listed, err := st.Processes(ctx, "tester")
	if err != nil {
		t.Fatalf("Processes: %v", err)
	}
	if len(listed) != 1 || !listed[0].Permits.Match("fs_read", nil) || !listed[0].Permits.Allows("/org/flows") {
		t.Errorf("the listing carries %+v, want the registered block", listed)
	}

	// Registering the same id again replaces the block, so a Package that
	// asks for less on its next run gets less.
	_, hash, err = store.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if err := st.RegisterProcess(ctx, store.Process{
		ID:           "01930000-0000-7000-8000-000000000001",
		Owner:        "tester",
		Package:      "/org/workflow",
		Name:         "workflow",
		Expose:       "mcp",
		RegisteredAt: time.Now().UTC(),
	}, hash, 0); err != nil {
		t.Fatalf("RegisterProcess again: %v", err)
	}
	after, found, err := st.Process(ctx, "01930000-0000-7000-8000-000000000001")
	if err != nil || !found {
		t.Fatalf("Process: %v, found %v", err, found)
	}
	if after.Permits.Match("fs_read", nil) || after.Permits.AnyPath() {
		t.Errorf("the replaced registration kept %+v", after.Permits)
	}
}

// TestARegistrationWrittenBeforePermitsExistedPermitsNothing is the migration
// promise: the column is added with an empty default, and a record that has
// never carried one reads as the empty block rather than failing to read.
func TestARegistrationWrittenBeforePermitsExistedPermitsNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kitbashd.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	_, hash, err := store.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	id := "01930000-0000-7000-8000-000000000002"
	if err := st.RegisterProcess(ctx, store.Process{
		ID:           id,
		Owner:        "tester",
		Package:      "/org/workflow",
		Name:         "workflow",
		Expose:       "mcp",
		Permits:      manifest.Permits{Tools: []string{"fs_read"}},
		RegisteredAt: time.Now().UTC(),
	}, hash, 0); err != nil {
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
	if _, err := db.Exec("ALTER TABLE processes DROP COLUMN permits"); err != nil {
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
	if got.Permits.Match("fs_read", nil) || got.Permits.AnyPath() {
		t.Errorf("a legacy registration permitted %+v", got.Permits)
	}
}
