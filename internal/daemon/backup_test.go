package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is a wall clock a test moves by hand. The daemon reads it from its own
// goroutines, so every read and every move goes through the lock.
type clock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration
}

func newClock(at time.Time) *clock { return &clock{now: at} }

// newTickingClock moves on by step every time it is read. A copy is named for
// the second it was taken, so a run of them in one real second needs a clock
// that does not stand still.
func newTickingClock(at time.Time, step time.Duration) *clock {
	return &clock{now: at, step: step}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	at := c.now
	c.now = c.now.Add(c.step)
	return at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// backupDir is where the copies of a harness's store go.
func (h *harness) backupDir() string {
	return filepath.Join(filepath.Dir(h.store.Path()), BackupDirName)
}

// copies are the backups on disk, oldest first.
func copies(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// Nine days of backups leave seven files: the newest week, and the two oldest
// deleted rather than kept forever, see PLAN.md section 4.7.
func TestBackupKeepsTheNewestSeven(t *testing.T) {
	c := newClock(time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC))
	h := serveWith(t, Options{
		Admin: func(*user.User) (bool, error) { return false, nil },
		Now:   c.Now,
	})
	dir := h.backupDir()
	for range 9 {
		if _, err := h.server.Backup(context.Background(), dir); err != nil {
			t.Fatalf("Backup: %v", err)
		}
		c.advance(24 * time.Hour)
	}

	names := copies(t, dir)
	if len(names) != BackupKeep {
		t.Fatalf("the backup directory holds %d copies (%v), want %d", len(names), names, BackupKeep)
	}
	// The names carry the date, so the oldest two days are the ones that went.
	if names[0] != "kitbashd-20260908-030000.db" {
		t.Errorf("the oldest copy is %s, want the eighth day back", names[0])
	}
	if names[len(names)-1] != "kitbashd-20260914-030000.db" {
		t.Errorf("the newest copy is %s, want the last run", names[len(names)-1])
	}
	for _, name := range names {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s is mode %o, want 600", name, mode)
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat the backup directory: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("the backup directory is mode %o, want 700", mode)
	}
}

// health is where an operator reads whether the backups are happening, so it
// carries the time of the last one and how many are kept.
func TestHealthReportsTheBackups(t *testing.T) {
	c := newClock(time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC))
	h := serveWith(t, Options{
		Admin: func(*user.User) (bool, error) { return false, nil },
		Now:   c.Now,
	})

	before := h.health(t)
	if before.LastBackup != "" || before.Backups != 0 || before.LastBackupError != "" {
		t.Errorf("a daemon that has taken no backup reports %+v", before)
	}

	if _, err := h.server.Backup(context.Background(), h.backupDir()); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	after := h.health(t)
	if after.Backups != 1 {
		t.Errorf("backups = %d, want 1", after.Backups)
	}
	when, err := time.Parse(time.RFC3339, after.LastBackup)
	if err != nil {
		t.Fatalf("lastBackup %q is not RFC 3339: %v", after.LastBackup, err)
	}
	if !when.Equal(c.Now()) {
		t.Errorf("lastBackup = %s, want %s", when, c.Now())
	}
	if after.LastBackupError != "" {
		t.Errorf("lastBackupError = %q, want none", after.LastBackupError)
	}
}

// A backup that cannot be written is reported rather than swallowed: health
// says why, and the last good backup's time stays where it was.
func TestAFailedBackupIsReportedByHealth(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a directory with no write bit")
	}
	h := serve(t, false)
	if _, err := h.server.Backup(context.Background(), h.backupDir()); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	good := h.health(t)

	unwritable := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(unwritable, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := h.server.Backup(context.Background(), unwritable); err == nil {
		t.Fatal("a backup into an unwritable directory returned no error")
	}
	failed := h.health(t)
	if failed.LastBackupError == "" {
		t.Error("health reports no lastBackupError after a backup failed")
	}
	if failed.LastBackup != good.LastBackup {
		t.Errorf("lastBackup moved to %q on a failed run, want the last good one %q",
			failed.LastBackup, good.LastBackup)
	}
	if failed.Backups != good.Backups {
		t.Errorf("backups = %d after a failed run, want the %d still on disk", failed.Backups, good.Backups)
	}
}

// The loop is what actually takes the backups, so it is run on an interval a
// test can wait for rather than only being read.
func TestBackupLoopTakesOneCopyPerInterval(t *testing.T) {
	c := newTickingClock(time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC), time.Second)
	h := serveWith(t, Options{
		Admin: func(*user.User) (bool, error) { return false, nil },
		Now:   c.Now,
	})
	dir := h.backupDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.server.BackupLoop(ctx, dir, 20*time.Millisecond)

	// health is what says a run finished; a file in the directory may still be
	// the copy in flight.
	deadline := time.Now().Add(10 * time.Second)
	var got healthResponse
	for {
		got = h.health(t)
		if got.Backups >= 2 && got.LastBackup != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the backup loop finished %d runs in ten seconds, want two", got.Backups)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if got.LastBackupError != "" {
		t.Errorf("lastBackupError = %q, want none", got.LastBackupError)
	}
	if names := copies(t, dir); len(names) < 2 {
		t.Errorf("the backup directory holds %v, want the copies the loop took", names)
	}
}

// The interval override is what a test and an operator outside a kitbash host
// set; a value that is not a duration leaves the daily backup alone.
func TestBackupEveryFromEnv(t *testing.T) {
	for _, tc := range []struct {
		set  string
		want time.Duration
	}{
		{"", BackupEvery},
		{"90m", 90 * time.Minute},
		{"nightly", BackupEvery},
		{"-1h", BackupEvery},
	} {
		t.Setenv(BackupEveryEnv, tc.set)
		if got := backupEveryFromEnv(); got != tc.want {
			t.Errorf("%s=%q gives %s, want %s", BackupEveryEnv, tc.set, got, tc.want)
		}
	}
}

// health reads the health of this daemon as the response shape declares it.
func (h *harness) health(t *testing.T) healthResponse {
	t.Helper()
	res, body := h.do(http.MethodGet, healthPath, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, body %s", res.StatusCode, body)
	}
	var got healthResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("health body %q: %v", body, err)
	}
	return got
}

// A copy is named for the moment it was taken, so an operator reading the
// directory knows what each file is without opening it.
func TestBackupNamesCarryTheTime(t *testing.T) {
	c := newClock(time.Date(2026, 9, 6, 3, 4, 5, 0, time.UTC))
	h := serveWith(t, Options{
		Admin: func(*user.User) (bool, error) { return false, nil },
		Now:   c.Now,
	})
	path, err := h.server.Backup(context.Background(), h.backupDir())
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if name := filepath.Base(path); name != "kitbashd-20260906-030405.db" {
		t.Errorf("the copy is called %s, want the time it was taken", name)
	}
	if !strings.HasPrefix(path, h.backupDir()) {
		t.Errorf("the copy is at %s, want it under %s", path, h.backupDir())
	}
}
