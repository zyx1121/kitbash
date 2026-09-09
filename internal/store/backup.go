package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Backup writes a consistent copy of the store to path with SQLite's own
// VACUUM INTO, which reads the database through the same connection every
// other statement uses. Nothing is locked out for the length of the copy and
// the copy is a whole database rather than a file plus whatever the write
// ahead log happened to hold, which is what copying kitbashd.db by hand gives.
//
// The copy carries every member's Telemetry, so it is written with the mode of
// the store itself and into a directory only root may enter, see
// PLAN.md section 4.7. SQLite refuses a path that already exists, so a caller
// that names the same file twice is told rather than silently overwriting a
// backup.
func (s *Store) Backup(ctx context.Context, path string) error {
	if path == "" {
		return fmt.Errorf("store: backup: empty path")
	}
	if err := os.MkdirAll(filepath.Dir(path), DirMode); err != nil {
		return fmt.Errorf("store: backup directory %s: %w", filepath.Dir(path), err)
	}
	// The check comes before the copy so the failure below may delete what it
	// left behind: SQLite refuses a path that exists, and a backup already
	// there is one this must not touch.
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("store: backup to %s: the file already exists", path)
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		// A copy that was interrupted, by a cancelled context or by a full
		// disk, leaves a file that is not a database. Nothing may mistake it
		// for a backup, so it goes.
		os.Remove(path)
		return fmt.Errorf("store: backup to %s: %w", path, err)
	}
	if err := os.Chmod(path, FileMode); err != nil {
		return fmt.Errorf("store: backup to %s: %w", path, err)
	}
	return nil
}
