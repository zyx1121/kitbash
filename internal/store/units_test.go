package store_test

import (
	"context"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite" // this test writes a store file directly

	"github.com/zyx1121/kitbash/internal/mounts"
	"github.com/zyx1121/kitbash/internal/store"
)

// v0140Processes is the processes table as the release before pods wrote it:
// every column of 0.14.0 and no units. A fixture of that release is a file
// somebody has to build from a tag, so the shape is written here instead,
// which is the thing the migration has to survive, the same way the schedule
// column is covered.
const v0140Processes = `CREATE TABLE processes (
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
  hostname      TEXT    NOT NULL DEFAULT '',
  schedule      TEXT    NOT NULL DEFAULT ''
)`

// TestTheUnitsColumnIsAddedToAnOlderStore opens a database written before pods
// existed. The registration in it comes back as the Process of one unit it is,
// and opening it again adds nothing: the migration is additive and idempotent,
// like the schedule column before it.
func TestTheUnitsColumnIsAddedToAnOlderStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kitbashd.db")
	db := openRaw(t, path)
	if _, err := db.Exec(v0140Processes); err != nil {
		t.Fatalf("write the 0.14.0 processes table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO processes (id, owner, package, name, container, expose, token_hash, registered_at)
		VALUES ('01930000-0000-7000-8000-0000000000b1', 'loki', '/org/handbook', 'handbook',
		        'kitbash-handbook', 'http', 'a-hash', ?)`, fixtureBase.UnixNano()); err != nil {
		t.Fatalf("write the 0.14.0 registration: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the 0.14.0 store: %v", err)
	}

	for pass := range 2 {
		s, err := store.Open(path)
		if err != nil {
			t.Fatalf("open pass %d: %v", pass, err)
		}
		p, found, err := s.Process(context.Background(), "01930000-0000-7000-8000-0000000000b1")
		if err != nil || !found {
			t.Fatalf("pass %d: read the migrated Process: %v, found %v", pass, err, found)
		}
		if p.Composition.Declared() {
			t.Errorf("pass %d: the migrated Process reads as a pod; a row written before units is one unit", pass)
		}
		if len(p.Composition.Units) != 0 || p.Composition.Pod != "" {
			t.Errorf("pass %d: the migrated Process carries units %+v", pass, p.Composition)
		}
		if p.Package != "/org/handbook" || p.Container != "kitbash-handbook" {
			t.Errorf("pass %d: the migration changed the row: %+v", pass, p)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close pass %d: %v", pass, err)
		}
	}
}

// A pod is written and read back as it was registered, because the boot
// restore of the next daemon is built from these rows and nothing else
// remembers which containers one Process has.
func TestAPodSurvivesTheStore(t *testing.T) {
	s := freshStore(t)
	p := store.Process{
		ID:        "01930000-0000-7000-8000-0000000000b2",
		Owner:     "loki",
		Package:   "/home/loki/board",
		Name:      "board",
		Container: "kitbash-board-board-web",
		Digest:    "sha256:" + "aa" + "00000000000000000000000000000000000000000000000000000000000000",
		Expose:    "http",
		Composition: store.Composition{
			Pod: "kitbash-board-board",
			Units: []store.Unit{
				{
					Name: "cache", Container: "kitbash-board-board-cache",
					Digest:  "sha256:" + "bb" + "00000000000000000000000000000000000000000000000000000000000000",
					Secrets: []string{"CACHE_PASSWORD"},
					Limits:  store.Limits{Memory: "128Mi"},
				},
				{
					Name: "web", Container: "kitbash-board-board-web",
					Digest: "sha256:" + "aa" + "00000000000000000000000000000000000000000000000000000000000000",
					Face:   true,
					Mounts: []mounts.Resolved{{
						Source: "/home/loki/notes", Target: "/files/notes", Mode: "ro",
					}},
				},
			},
		},
		RegisteredAt: fixtureBase,
	}
	if err := s.RegisterProcess(context.Background(), p, "a-hash", store.Quota{}); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}

	held, found, err := s.Process(context.Background(), p.ID)
	if err != nil || !found {
		t.Fatalf("read the pod: %v, found %v", err, found)
	}
	if !held.Composition.Declared() {
		t.Fatalf("the Process reads as one unit: %+v", held.Composition)
	}
	if held.Composition.Pod != "kitbash-board-board" || len(held.Composition.Units) != 2 {
		t.Fatalf("composition = %+v, want the pod that was registered", held.Composition)
	}
	face, ok := held.Composition.Face()
	if !ok || face.Name != "web" {
		t.Errorf("the face is %+v, want the web unit", face)
	}
	if len(face.Mounts) != 1 || face.Mounts[0].Source != "/home/loki/notes" {
		t.Errorf("the face's mounts came back as %+v", face.Mounts)
	}
	cache := held.Composition.Units[0]
	if cache.Name != "cache" || cache.Limits.Memory != "128Mi" ||
		len(cache.Secrets) != 1 || cache.Secrets[0] != "CACHE_PASSWORD" {
		t.Errorf("the sidecar came back as %+v", cache)
	}
	// The order the manifest declared is the order a start and a restore make
	// the containers in, so it is part of the record.
	if held.Composition.Units[1].Name != "web" {
		t.Errorf("the units came back in the order %s, %s",
			held.Composition.Units[0].Name, held.Composition.Units[1].Name)
	}
	if names := held.Composition.Names(); len(names) != 2 || names[0] != "cache" || names[1] != "web" {
		t.Errorf("the unit names are %v, want cache and web", names)
	}
}

// A Process of one unit is written with no composition at all, so a row this
// release writes for a single unit Package reads the same as one an earlier
// release wrote.
func TestASingleUnitProcessWritesNoUnits(t *testing.T) {
	s := freshStore(t)
	p := store.Process{
		ID: "01930000-0000-7000-8000-0000000000b3", Owner: "loki",
		Package: "/home/loki/echo", Name: "echo", Container: "kitbash-echo-echo",
		RegisteredAt: fixtureBase,
	}
	if err := s.RegisterProcess(context.Background(), p, "a-hash", store.Quota{}); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}
	held, _, err := s.Process(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if held.Composition.Declared() || held.Composition.Pod != "" {
		t.Errorf("a single unit Process was stored as %+v", held.Composition)
	}
	if (store.Composition{}).JSON() != "" {
		t.Errorf("the empty composition renders as %q, want the empty column",
			(store.Composition{}).JSON())
	}
	// A composition of one unit is not a pod either: a Package of one unit
	// runs as a bare container whatever it was registered with.
	one := store.Composition{Pod: "kitbash-echo-echo", Units: []store.Unit{{Name: "only"}}}
	if one.Declared() {
		t.Error("a composition of one unit reads as a pod")
	}
}
