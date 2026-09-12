package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// BackupEvery is how often the store is copied, and BackupDelay how long after
// start the first copy is taken. A daemon that restarts often would otherwise
// spend its first second on a backup instead of on the sessions waiting for
// it, and one that runs for a day takes exactly one copy.
const (
	BackupEvery = 24 * time.Hour
	BackupDelay = 5 * time.Minute
)

// BackupKeep is how many copies are kept. Older ones are deleted after every
// successful run, so a week of daily copies is what the disk carries, see
// PLAN.md section 4.7.
const BackupKeep = 7

// BackupEveryEnv shortens the interval, which is what a test sets. It is read
// like every other override in kitbash: for tests and for running outside a
// kitbash host.
const BackupEveryEnv = "KITBASH_BACKUP_EVERY"

// BackupDirName is the directory the copies go in, beside the store file.
const BackupDirName = "backup"

// backupPrefix and backupSuffix bracket the timestamp of one copy. Nothing
// else in the directory is ever deleted: kitbashd removes only files it wrote.
const (
	backupPrefix = "kitbashd-"
	backupSuffix = ".db"
	backupStamp  = "20060102-150405"
)

// backupStatus is what health reports about the backups.
type backupStatus struct {
	LastBackup      string
	Backups         int
	LastBackupError string
}

// backups is one server's view of its own copies, read by every health request
// and written by the backup loop.
type backups struct {
	mu     sync.Mutex
	status backupStatus
}

func (b *backups) get() backupStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

// succeeded records a run that produced a copy, and how many are kept now.
func (b *backups) succeeded(at time.Time, kept int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status.LastBackup = at.UTC().Format(time.RFC3339)
	b.status.Backups = kept
	b.status.LastBackupError = ""
}

// failed records why the last run produced none. The last successful time is
// kept, so health says both that backups are failing and how old the newest
// copy is.
func (b *backups) failed(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status.LastBackupError = err.Error()
}

// backupState is what the health handler reads.
func (s *Server) backupState() backupStatus { return s.backups.get() }

// BackupDir is where the copies go: a directory beside the store file, which
// only root may enter because the copies are the store, see PLAN.md section
// 4.7.
func (s *Server) BackupDir() string {
	return filepath.Join(filepath.Dir(s.store.Path()), BackupDirName)
}

// Backup writes one copy of the store, deletes the ones beyond BackupKeep and
// reports the file it wrote. It logs one line per run, whether it worked or
// not, so an operator reading the daemon log sees every backup this host took.
func (s *Server) Backup(ctx context.Context, dir string) (string, error) {
	if dir == "" {
		dir = s.BackupDir()
	}
	now := s.now()
	path := filepath.Join(dir, backupPrefix+now.UTC().Format(backupStamp)+backupSuffix)
	if err := s.store.Backup(ctx, path); err != nil {
		if ctx.Err() != nil {
			// The daemon is stopping. A copy that was interrupted is not a
			// backup that failed, and health is about to be nobody's to read.
			return "", err
		}
		s.backups.failed(err)
		logger.Printf("store backup failed: %v", err)
		return "", err
	}
	kept, err := pruneBackups(dir, BackupKeep)
	if err != nil {
		// The copy is on disk, so the backup worked; what failed is the
		// housekeeping, and the next run tries again.
		s.backups.succeeded(now, kept)
		logger.Printf("backed up the store to %s, but could not remove the older copies: %v", path, err)
		return path, nil
	}
	s.backups.succeeded(now, kept)
	logger.Printf("backed up the store to %s, keeping %d", path, kept)
	return path, nil
}

// BackupLoop takes the first copy BackupDelay after start and one every
// interval after that, until ctx is done. A zero interval is the one
// KITBASH_BACKUP_EVERY names, and then BackupEvery.
//
// An interval shorter than the delay, which is what a test sets, is the delay
// too: nothing is served by making a test wait five minutes for the first
// copy of a store it just wrote.
func (s *Server) BackupLoop(ctx context.Context, dir string, every time.Duration) {
	if every <= 0 {
		every = backupEveryFromEnv()
	}
	delay := min(BackupDelay, every)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	for {
		if _, err := s.Backup(ctx, dir); err != nil && ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// backupEveryFromEnv reads the override. A value that is not a duration, or is
// not positive, leaves the daily interval in place and says so: a typo must
// not turn the backups off.
func backupEveryFromEnv() time.Duration {
	raw := os.Getenv(BackupEveryEnv)
	if raw == "" {
		return BackupEvery
	}
	every, err := time.ParseDuration(raw)
	if err != nil || every <= 0 {
		logger.Printf("%s=%q is not a positive duration, so the store is backed up every %s",
			BackupEveryEnv, raw, BackupEvery)
		return BackupEvery
	}
	return every
}

// pruneBackups deletes every copy but the newest keep and returns how many are
// left. The names carry a sortable timestamp, so the newest are the last ones
// in order, and a file this daemon did not write is not touched.
func pruneBackups(dir string, keep int) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("daemon: read the backup directory %s: %w", dir, err)
	}
	var copies []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, backupPrefix) || !strings.HasSuffix(name, backupSuffix) {
			continue
		}
		copies = append(copies, name)
	}
	sort.Strings(copies)
	if len(copies) <= keep {
		return len(copies), nil
	}
	var failed error
	kept := len(copies)
	for _, name := range copies[:len(copies)-keep] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			if failed == nil {
				failed = fmt.Errorf("daemon: remove the old backup %s: %w", name, err)
			}
			continue
		}
		kept--
	}
	return kept, failed
}
