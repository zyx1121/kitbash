package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite" // this test writes a store file directly

	"github.com/zyx1121/kitbash/internal/store"
)

// v0120Processes is the processes table as the release before the scheduler
// wrote it: every column of 0.12.0 and no schedule. A fixture of that release
// is a file somebody has to build from a tag, so the shape is written here
// instead, which is the thing the migration has to survive.
const v0120Processes = `CREATE TABLE processes (
  id            TEXT    PRIMARY KEY,
  owner         TEXT    NOT NULL,
  admin         INTEGER NOT NULL DEFAULT 0,
  package       TEXT    NOT NULL DEFAULT '',
  name          TEXT    NOT NULL DEFAULT '',
  container     TEXT    NOT NULL DEFAULT '',
  digest        TEXT    NOT NULL DEFAULT '',
  expose        TEXT    NOT NULL DEFAULT '',
  endpoint      TEXT    NOT NULL DEFAULT '',
  subscriptions TEXT    NOT NULL DEFAULT '',
  runner        TEXT    NOT NULL DEFAULT '',
  token_hash    TEXT    NOT NULL,
  fanout_secret TEXT    NOT NULL DEFAULT '',
  registered_at INTEGER NOT NULL,
  permits       TEXT    NOT NULL DEFAULT '',
  limits        TEXT    NOT NULL DEFAULT '',
  health        TEXT    NOT NULL DEFAULT '',
  mounts        TEXT    NOT NULL DEFAULT '',
  secrets       TEXT    NOT NULL DEFAULT '',
  hostname      TEXT    NOT NULL DEFAULT ''
)`

// TestTheScheduleColumnIsAddedToAnOlderStore opens a database written before
// the scheduler existed. The registration in it comes back as a Process that
// is not a job, which is what every Process was, and opening it again adds
// nothing: the migration is additive and idempotent, like the host name column
// before it.
func TestTheScheduleColumnIsAddedToAnOlderStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kitbashd.db")
	db := openRaw(t, path)
	if _, err := db.Exec(v0120Processes); err != nil {
		t.Fatalf("write the 0.12.0 processes table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO processes (id, owner, package, name, container, expose, token_hash, registered_at)
		VALUES ('01930000-0000-7000-8000-0000000000a1', 'loki', '/org/handbook', 'handbook',
		        'kitbash-handbook', 'http', 'a-hash', ?)`, fixtureBase.UnixNano()); err != nil {
		t.Fatalf("write the 0.12.0 registration: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the 0.12.0 store: %v", err)
	}

	for pass := range 2 {
		s, err := store.Open(path)
		if err != nil {
			t.Fatalf("open pass %d: %v", pass, err)
		}
		p, found, err := s.Process(context.Background(), "01930000-0000-7000-8000-0000000000a1")
		if err != nil || !found {
			t.Fatalf("pass %d: read the migrated Process: %v, found %v", pass, err, found)
		}
		if p.Schedule.Declared() {
			t.Errorf("pass %d: the migrated Process reads as a job; a row written before schedules is not one", pass)
		}
		if p.Package != "/org/handbook" || p.Container != "kitbash-handbook" {
			t.Errorf("pass %d: the migration changed the row: %+v", pass, p)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close pass %d: %v", pass, err)
		}
	}
}

// A schedule is written and read back as it was declared, because the ticker of
// the next daemon is built from these rows and nothing else remembers that a
// Process is a job.
func TestAScheduleSurvivesTheStore(t *testing.T) {
	s := freshStore(t)
	p := store.Process{
		ID:        "01930000-0000-7000-8000-0000000000c1",
		Owner:     "loki",
		Package:   "/home/loki/weather",
		Name:      "weather",
		Container: "kitbash-weather-weather",
		Expose:    "none",
		Schedule: store.Schedule{
			Cron:   "0 8 * * *",
			Env:    map[string]string{"CITY": "taipei"},
			Memory: "512Mi",
			CPU:    "1",
		},
		RegisteredAt: fixtureBase,
	}
	if err := s.RegisterProcess(context.Background(), p, "a-hash", store.Quota{}); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}

	held, found, err := s.Process(context.Background(), p.ID)
	if err != nil || !found {
		t.Fatalf("read the job: %v, found %v", err, found)
	}
	if held.Schedule.Cron != "0 8 * * *" || held.Schedule.Memory != "512Mi" || held.Schedule.CPU != "1" {
		t.Errorf("schedule = %+v, want the one that was registered", held.Schedule)
	}
	if held.Schedule.Env["CITY"] != "taipei" {
		t.Errorf("the schedule's environment came back as %+v", held.Schedule.Env)
	}
}

// The job quota is held inside the transaction that writes the registration,
// so two sessions registering at the same moment cannot both read a count
// under the limit and both write. A registration that is not a job is never
// refused by it, and one that replaces a job of the same id is not a new one.
func TestTheJobQuotaIsHeldInsideTheTransaction(t *testing.T) {
	s := freshStore(t)
	quota := store.Quota{Processes: 10, Scheduled: 2}
	write := func(id, owner, cron string, q store.Quota) error {
		p := store.Process{ID: id, Owner: owner, Package: "/home/" + owner + "/weather",
			Container: "kitbash-weather-" + id, RegisteredAt: fixtureBase}
		if cron != "" {
			p.Schedule = store.Schedule{Cron: cron}
		}
		return s.RegisterProcess(context.Background(), p, "a-hash-of-"+id, q)
	}
	for _, id := range []string{"01930000-0000-7000-8000-0000000000d1", "01930000-0000-7000-8000-0000000000d2"} {
		if err := write(id, "loki", "0 8 * * *", quota); err != nil {
			t.Fatalf("RegisterProcess %s: %v", id, err)
		}
	}
	if err := write("01930000-0000-7000-8000-0000000000d3", "loki", "0 9 * * *", quota); !errors.Is(err, store.ErrTooManyScheduled) {
		t.Errorf("the third job answered %v, want %v", err, store.ErrTooManyScheduled)
	}
	// A Process that is not a job is not refused by the job limit.
	if err := write("01930000-0000-7000-8000-0000000000d4", "loki", "", quota); err != nil {
		t.Errorf("a Process that is not a job was refused by the job limit: %v", err)
	}
	// And a replacement of a job that is already there is not a new job.
	if err := write("01930000-0000-7000-8000-0000000000d1", "loki", "0 10 * * *", quota); err != nil {
		t.Errorf("replacing a job was refused: %v", err)
	}
	// Another member has their own count.
	if err := write("01930000-0000-7000-8000-0000000000d5", "kilo", "0 8 * * *", quota); err != nil {
		t.Errorf("another member's first job was refused: %v", err)
	}
}

// A run writes the ceiling and the token of a registration that is there, and
// writes nothing at all for one that is gone: a tick that raced a proc_stop
// must not put the job back, see internal/daemon/schedule.go.
func TestUpdateProcessRunWritesNoRow(t *testing.T) {
	s := freshStore(t)
	id := "01930000-0000-7000-8000-0000000000e1"
	p := store.Process{ID: id, Owner: "loki", Package: "/home/loki/weather",
		Container: "kitbash-weather-weather", Schedule: store.Schedule{Cron: "0 8 * * *"},
		RegisteredAt: fixtureBase}
	if err := s.RegisterProcess(context.Background(), p, "the-first-hash", store.Quota{}); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}

	written, err := s.UpdateProcessRun(context.Background(), id, store.Limits{Memory: "512Mi", Ceiling: true}, "the-run-hash")
	if err != nil || !written {
		t.Fatalf("UpdateProcessRun on a registered Process: %v, written %v", err, written)
	}
	held, found, err := s.Process(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("read it back: %v, found %v", err, found)
	}
	if held.Limits.Memory != "512Mi" || held.Schedule.Cron != "0 8 * * *" {
		t.Errorf("the run wrote %+v, want the ceiling written and the job left alone", held)
	}

	if _, err := s.DeleteProcess(context.Background(), id); err != nil {
		t.Fatalf("DeleteProcess: %v", err)
	}
	written, err = s.UpdateProcessRun(context.Background(), id, store.Limits{}, "a-later-hash")
	if err != nil {
		t.Fatalf("UpdateProcessRun on an unregistered Process: %v", err)
	}
	if written {
		t.Error("the run wrote a row for a Process that was unregistered")
	}
	if _, found, _ := s.Process(context.Background(), id); found {
		t.Error("the run put the unregistered registration back")
	}
}

// The token of a run is revoked when that run ends, and only that token: a
// Process registered again while the run was going keeps the one the new
// registration minted.
func TestRevokeProcessTokenNamesTheHash(t *testing.T) {
	s := freshStore(t)
	id := "01930000-0000-7000-8000-0000000000f1"
	p := store.Process{ID: id, Owner: "loki", Package: "/home/loki/weather", RegisteredAt: fixtureBase}
	token, hash, err := store.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if err := s.RegisterProcess(context.Background(), p, hash, store.Quota{}); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}
	if _, live, _ := s.ProcessByToken(context.Background(), token); !live {
		t.Fatal("the token does not answer before the run ended")
	}

	// A hash that is not this run's revokes nothing.
	if revoked, err := s.RevokeProcessToken(context.Background(), id, "another-run-hash"); err != nil || revoked {
		t.Errorf("revoking with another run's hash answered %v, %v", revoked, err)
	}
	if _, live, _ := s.ProcessByToken(context.Background(), token); !live {
		t.Error("another run's revoke took this run's token")
	}
	if revoked, err := s.RevokeProcessToken(context.Background(), id, hash); err != nil || !revoked {
		t.Fatalf("revoking this run's token answered %v, %v", revoked, err)
	}
	if _, live, _ := s.ProcessByToken(context.Background(), token); live {
		t.Error("the token of a run that ended still answers")
	}
	// The registration itself is untouched.
	if _, found, _ := s.Process(context.Background(), id); !found {
		t.Error("revoking the token unregistered the Process")
	}
}

// A schedule column this release cannot read is a Process that is not a job,
// and never a listing that fails: one row nothing can parse would otherwise
// take every Process of the host off proc_list.
func TestAnUnreadableScheduleIsNotAFailedRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kitbashd.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	id := "01930000-0000-7000-8000-000000000101"
	p := store.Process{ID: id, Owner: "loki", Package: "/home/loki/weather",
		Container: "kitbash-weather-weather", RegisteredAt: fixtureBase}
	if err := s.RegisterProcess(context.Background(), p, "a-hash", store.Quota{}); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	for _, column := range []string{
		"not json at all",
		`{"cron":"every morning"}`,
		`{"cron":"0 8 * * *","memory":"lots"}`,
		`{"cron":"0 8 * * *","cpu":"all of them"}`,
	} {
		db := openRaw(t, path)
		if _, err := db.Exec("UPDATE processes SET schedule = ? WHERE id = ?", column, id); err != nil {
			t.Fatalf("write the column %q: %v", column, err)
		}
		db.Close()

		s, err := store.Open(path)
		if err != nil {
			t.Fatalf("open with the column %q: %v", column, err)
		}
		list, err := s.Processes(context.Background(), "loki")
		if err != nil {
			t.Fatalf("the column %q failed the listing: %v", column, err)
		}
		if len(list) != 1 {
			t.Fatalf("the column %q answered %d Processes, want the one", column, len(list))
		}
		if list[0].Schedule.Declared() {
			t.Errorf("the column %q reads as a job, want a Process that is not one", column)
		}
		s.Close()
	}
}

// freshStore is an empty store of the current shape.
func freshStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "kitbashd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
