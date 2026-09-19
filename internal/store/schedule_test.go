package store_test

import (
	"context"
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
	if err := s.RegisterProcess(context.Background(), p, "a-hash", 0); err != nil {
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

// ScheduledCount is what bounds the ticker per member: it counts that member's
// jobs, not their Processes, and never the registration about to replace
// itself.
func TestScheduledCountCountsOneMembersJobs(t *testing.T) {
	s := freshStore(t)
	write := func(id, owner, cron string) {
		t.Helper()
		p := store.Process{ID: id, Owner: owner, Package: "/home/" + owner + "/weather",
			Container: "kitbash-weather-" + id, RegisteredAt: fixtureBase}
		if cron != "" {
			p.Schedule = store.Schedule{Cron: cron}
		}
		// The token hash is unique per row, the way a minted token is.
		if err := s.RegisterProcess(context.Background(), p, "a-hash-of-"+id, 0); err != nil {
			t.Fatalf("RegisterProcess %s: %v", id, err)
		}
	}
	write("01930000-0000-7000-8000-0000000000d1", "loki", "0 8 * * *")
	write("01930000-0000-7000-8000-0000000000d2", "loki", "0 9 * * *")
	write("01930000-0000-7000-8000-0000000000d3", "loki", "")
	write("01930000-0000-7000-8000-0000000000d4", "kilo", "0 8 * * *")

	held, err := s.ScheduledCount(context.Background(), "loki", "")
	if err != nil {
		t.Fatalf("ScheduledCount: %v", err)
	}
	if held != 2 {
		t.Errorf("loki holds %d jobs, want 2", held)
	}
	held, err = s.ScheduledCount(context.Background(), "loki", "01930000-0000-7000-8000-0000000000d1")
	if err != nil {
		t.Fatalf("ScheduledCount excluding one: %v", err)
	}
	if held != 1 {
		t.Errorf("loki holds %d other jobs, want 1: a replacement is not a new job", held)
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
