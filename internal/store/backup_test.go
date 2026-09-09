package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A backup is a whole database an operator can open, not a file copy, so the
// test reads the records back out of it rather than comparing sizes.
func TestBackupIsAStoreWithTheSameRecords(t *testing.T) {
	s := open(t)
	seed(t, s)

	path := filepath.Join(t.TempDir(), "backup", "kitbashd-20260906-120000.db")
	if err := s.Backup(context.Background(), path); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the backup: %v", err)
	}
	if mode := info.Mode().Perm(); mode != FileMode {
		t.Errorf("backup mode is %o, want %o: it holds every member's Telemetry", mode, FileMode)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat the backup directory: %v", err)
	}
	if mode := dir.Mode().Perm(); mode != DirMode {
		t.Errorf("backup directory mode is %o, want %o", mode, DirMode)
	}

	copied, err := Open(path)
	if err != nil {
		t.Fatalf("opening the backup: %v", err)
	}
	defer copied.Close()
	page, err := copied.Query(context.Background(), SignalTraces, Filter{
		Since: base.Add(-24 * time.Hour), Until: base,
	})
	if err != nil {
		t.Fatalf("Query on the backup: %v", err)
	}
	if len(page.Spans) != 3 {
		t.Errorf("the backup holds %d spans, want the 3 the store holds", len(page.Spans))
	}
}

// A path SQLite cannot write is an error the caller can log, not a panic and
// not a half written file.
func TestBackupToAnUnwritableDirectoryFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a directory with no write bit")
	}
	s := open(t)
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	err := s.Backup(context.Background(), filepath.Join(dir, "kitbashd-20260906-120000.db"))
	if err == nil {
		t.Fatal("Backup into an unwritable directory returned no error")
	}
}

// Backing up over an existing file would destroy the backup it names, so
// SQLite's refusal is passed on rather than worked around.
func TestBackupRefusesAPathThatExists(t *testing.T) {
	s := open(t)
	path := filepath.Join(t.TempDir(), "kitbashd-20260906-120000.db")
	if err := os.WriteFile(path, []byte("not a database"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Backup(context.Background(), path); err == nil {
		t.Fatal("Backup over an existing file returned no error")
	}
}
