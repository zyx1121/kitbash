package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // these tests read a store file directly

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/mounts"
	"github.com/zyx1121/kitbash/internal/store"
)

// The migration is what these tests exercise: every released store shape is
// kept under testdata as a database an old kitbashd wrote, and the current
// Open is the code under test. See testdata/README.md for how a fixture is
// produced, which is what a future release repeats.

// fixtureBase is the instant every fixture places its records around. The
// generators under testdata/gen use the same one, so a fixture rebuilt next
// year holds the same timestamps and these tests keep their numbers.
var fixtureBase = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

// fixture is one released store and what it holds, as testdata/README.md
// records it. The era fields say which attributes that version stamped, so a
// test can tell a record that never carried a caller from one that lost it.
type fixture struct {
	tag        string
	spans      int
	logs       int
	metrics    int
	processes  int
	approvals  int
	producer   bool // the era stamped kitbash.producer
	caller     bool // the era stamped kitbash.caller
	container  string
	fanoutRead string // the fan out secret the first Process carries
}

var fixtures = []fixture{
	{tag: "v0.3.0", spans: 3, logs: 3, metrics: 3},
	{tag: "v0.4.0", spans: 3, logs: 3, metrics: 3, processes: 2, producer: true},
	{tag: "v0.5.0", spans: 3, logs: 3, metrics: 3, processes: 2, approvals: 3,
		producer: true, container: "kitbash-handbook"},
	{tag: "v0.6.0", spans: 3, logs: 3, metrics: 3, processes: 2, approvals: 3,
		producer: true, caller: true, container: "kitbash-handbook"},
	{tag: "v0.6.2", spans: 3, logs: 3, metrics: 3, processes: 2, approvals: 3,
		producer: true, caller: true, container: "kitbash-handbook",
		fanoutRead: "fixture-fanout-secret-alpha"},
}

// copyFixture puts one fixture in a temporary directory, because opening it
// migrates it and the file under testdata is the record of what an old
// kitbashd wrote.
func copyFixture(t *testing.T, tag string) string {
	t.Helper()
	records, err := os.ReadFile(filepath.Join("testdata", "kitbashd-"+tag+".db"))
	if err != nil {
		t.Fatalf("read the %s fixture: %v", tag, err)
	}
	path := filepath.Join(t.TempDir(), "kitbashd.db")
	if err := os.WriteFile(path, records, 0o600); err != nil {
		t.Fatalf("copy the %s fixture: %v", tag, err)
	}
	return path
}

// openFixture opens a copy of one fixture with the current store, which is the
// migration under test.
func openFixture(t *testing.T, tag string) *store.Store {
	t.Helper()
	s, err := store.Open(copyFixture(t, tag))
	if err != nil {
		t.Fatalf("open the %s fixture: %v", tag, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// TestFixtureRecordsSurviveTheMigration opens every released store with the
// current code and reads back what that version wrote.
func TestFixtureRecordsSurviveTheMigration(t *testing.T) {
	for _, f := range fixtures {
		t.Run(f.tag, func(t *testing.T) {
			s := openFixture(t, f.tag)
			ctx := context.Background()

			// The counts testdata/README.md records, per signal.
			for _, want := range []struct {
				signal string
				rows   int
			}{
				{store.SignalTraces, f.spans},
				{store.SignalLogs, f.logs},
				{store.SignalMetrics, f.metrics},
			} {
				page, err := s.Query(ctx, want.signal, store.Filter{Limit: store.MaxLimit})
				if err != nil {
					t.Fatalf("query %s: %v", want.signal, err)
				}
				if got := len(page.Spans) + len(page.Logs) + len(page.Metrics); got != want.rows {
					t.Errorf("%s holds %d records, want %d", want.signal, got, want.rows)
				}
			}

			// One known record per signal, by user and by time range. The
			// window is the hour around the record the fixture wrote at two
			// hours before its base.
			window := store.Filter{
				Since: fixtureBase.Add(-150 * time.Minute),
				Until: fixtureBase.Add(-90 * time.Minute),
			}

			byUser, err := s.Query(ctx, store.SignalTraces, store.Filter{User: "kilo"})
			if err != nil {
				t.Fatalf("query the spans of kilo: %v", err)
			}
			if len(byUser.Spans) != 1 || byUser.Spans[0].Name != "pkg_build" ||
				byUser.Spans[0].Status != store.StatusError {
				t.Fatalf("the spans of kilo are %+v, want the one pkg_build span", byUser.Spans)
			}
			span := byUser.Spans[0]
			if span.Eval == nil || !*span.Eval {
				t.Errorf("the pkg_build span carries eval %v, want true", span.Eval)
			}
			if span.Internal != nil {
				t.Errorf("the pkg_build span carries internal %v, want none: no release stamped it", *span.Internal)
			}
			if want := producerOf(f, "p-alpha"); span.Producer != want {
				t.Errorf("the pkg_build span carries the producer %q, want %q", span.Producer, want)
			}
			if want := callerOf(f, "p-beta"); span.Caller != want {
				t.Errorf("the pkg_build span carries the caller %q, want %q", span.Caller, want)
			}
			byTime, err := s.Query(ctx, store.SignalTraces, window)
			if err != nil {
				t.Fatalf("query the spans of the window: %v", err)
			}
			if len(byTime.Spans) != 1 || byTime.Spans[0].SpanID != span.SpanID {
				t.Errorf("the window holds %d spans, want the one pkg_build span", len(byTime.Spans))
			}

			logs, err := s.Query(ctx, store.SignalLogs, store.Filter{User: "kilo"})
			if err != nil {
				t.Fatalf("query the logs of kilo: %v", err)
			}
			if len(logs.Logs) != 1 || logs.Logs[0].Body != "the build failed" ||
				logs.Logs[0].Severity != "ERROR" {
				t.Fatalf("the logs of kilo are %+v, want the one build failure", logs.Logs)
			}
			if want := callerOf(f, "p-beta"); logs.Logs[0].Caller != want {
				t.Errorf("the build failure carries the caller %q, want %q", logs.Logs[0].Caller, want)
			}
			logsByTime, err := s.Query(ctx, store.SignalLogs, window)
			if err != nil {
				t.Fatalf("query the logs of the window: %v", err)
			}
			if len(logsByTime.Logs) != 1 || logsByTime.Logs[0].Body != "the build failed" {
				t.Errorf("the window holds %d logs, want the one build failure", len(logsByTime.Logs))
			}

			metrics, err := s.Query(ctx, store.SignalMetrics, store.Filter{User: "kilo"})
			if err != nil {
				t.Fatalf("query the metrics of kilo: %v", err)
			}
			if len(metrics.Metrics) != 1 || metrics.Metrics[0].Name != "kitbash.calls" ||
				metrics.Metrics[0].Value != 2 || metrics.Metrics[0].Unit != "1" {
				t.Fatalf("the metrics of kilo are %+v, want the one call count", metrics.Metrics)
			}
			metricsByTime, err := s.Query(ctx, store.SignalMetrics, window)
			if err != nil {
				t.Fatalf("query the metrics of the window: %v", err)
			}
			if len(metricsByTime.Metrics) != 1 || metricsByTime.Metrics[0].Value != 2 {
				t.Errorf("the window holds %d metrics, want the one call count", len(metricsByTime.Metrics))
			}

			// The attributes an old record carried whole are still whole.
			first, err := s.Query(ctx, store.SignalTraces, store.Filter{Tool: "fs_read"})
			if err != nil {
				t.Fatalf("query the fs_read span: %v", err)
			}
			if len(first.Spans) != 1 || first.Spans[0].Other["http.status"] != float64(200) {
				t.Errorf("the fs_read span carries the attributes %+v, want http.status 200", first.Spans)
			}
		})
	}
}

// TestFixtureRetentionSurvivesTheMigration is the settings table: a window an
// admin set on an old release is still the window after the upgrade, not the
// default the fresh store answers with.
func TestFixtureRetentionSurvivesTheMigration(t *testing.T) {
	want := store.Retention{Traces: "720h", Logs: "168h", Metrics: "90d"}
	for _, f := range fixtures {
		t.Run(f.tag, func(t *testing.T) {
			got, err := openFixture(t, f.tag).Retention(context.Background())
			if err != nil {
				t.Fatalf("Retention: %v", err)
			}
			if got != want {
				t.Errorf("retention is %+v, want %+v", got, want)
			}
		})
	}
}

// TestFixtureProcessesListWithTheNewColumnsDefaulted is the additive half of
// the migration: a registration written before a column existed lists with
// that column empty rather than failing to scan.
func TestFixtureProcessesListWithTheNewColumnsDefaulted(t *testing.T) {
	for _, f := range fixtures {
		if f.processes == 0 {
			continue
		}
		t.Run(f.tag, func(t *testing.T) {
			s := openFixture(t, f.tag)
			ctx := context.Background()

			listed, err := s.Processes(ctx, "")
			if err != nil {
				t.Fatalf("Processes: %v", err)
			}
			if len(listed) != f.processes {
				t.Fatalf("the store lists %d Processes, want %d", len(listed), f.processes)
			}
			for _, p := range listed {
				if len(p.Permits.Tools) != 0 || len(p.Permits.Paths) != 0 {
					t.Errorf("%s carries the permits %+v, want none: no release wrote them", p.ID, p.Permits)
				}
				// The mounts column arrives with M8, so every fixture reads
				// back with none: that Process sees no Files until it is run
				// again, which is how every Process ran before.
				if len(p.Mounts) != 0 {
					t.Errorf("%s carries the mounts %+v, want none: no release wrote them", p.ID, p.Mounts)
				}
			}

			alpha, found, err := s.ProcessByToken(ctx, "fixture-token-alpha")
			if err != nil || !found {
				t.Fatalf("ProcessByToken: %v, found %v", err, found)
			}
			if alpha.Owner != "loki" || !alpha.Admin || alpha.Package != "/org/handbook" ||
				alpha.Endpoint != "http://127.0.0.1:8081" {
				t.Errorf("the first Process is %+v, want the handbook registration", alpha)
			}
			if !alpha.Subscribes() {
				t.Errorf("the first Process lost its Telemetry subscription: %+v", alpha.Subscriptions)
			}
			if alpha.Container != f.container {
				t.Errorf("the first Process carries the container %q, want %q", alpha.Container, f.container)
			}
			if alpha.FanoutSecret != f.fanoutRead {
				t.Errorf("the first Process carries a fan out secret of %d bytes, want %d",
					len(alpha.FanoutSecret), len(f.fanoutRead))
			}
			if want := time.Duration(0); alpha.RegisteredAt.Sub(fixtureBase.Add(-24*time.Hour)) != want {
				t.Errorf("the first Process was registered at %s, want %s",
					alpha.RegisteredAt, fixtureBase.Add(-24*time.Hour))
			}
		})
	}
}

// TestFixtureApprovalsKeepTheirStates reads the queue an old release wrote.
func TestFixtureApprovalsKeepTheirStates(t *testing.T) {
	for _, f := range fixtures {
		if f.approvals == 0 {
			continue
		}
		t.Run(f.tag, func(t *testing.T) {
			s := openFixture(t, f.tag)
			ctx := context.Background()

			all, err := s.Approvals(ctx, "", "")
			if err != nil {
				t.Fatalf("Approvals: %v", err)
			}
			if len(all) != f.approvals {
				t.Fatalf("the queue holds %d approvals, want %d", len(all), f.approvals)
			}
			for _, state := range []string{store.StatePending, store.StateApproved, store.StateRejected} {
				of, err := s.Approvals(ctx, "", state)
				if err != nil {
					t.Fatalf("Approvals in %s: %v", state, err)
				}
				if len(of) != 1 {
					t.Errorf("the queue holds %d approvals in %s, want 1", len(of), state)
				}
			}

			approved, found, err := s.Approval(ctx, "01930000-0000-7000-8000-0000000000b2")
			if err != nil || !found {
				t.Fatalf("Approval: %v, found %v", err, found)
			}
			if approved.State != store.StateApproved || approved.DecidedBy != "loki" ||
				approved.Note != "looks fine" || string(approved.Result) != `{"ok":true}` {
				t.Errorf("the claimed approval is %+v, want the one loki approved", approved)
			}
			if approved.DecidedAt == nil {
				t.Errorf("the claimed approval lost the moment it was decided")
			}
			rejected, found, err := s.Approval(ctx, "01930000-0000-7000-8000-0000000000b3")
			if err != nil || !found {
				t.Fatalf("Approval: %v, found %v", err, found)
			}
			if rejected.State != store.StateRejected || rejected.Reason != "not this one" {
				t.Errorf("the rejected approval is %+v, want the one loki refused", rejected)
			}
		})
	}
}

// TestMigratedStoreTakesNewWork is what an upgraded daemon does next: register
// a Process with the columns the old store never had, queue an approval, run
// every filter of the query surface and sweep.
func TestMigratedStoreTakesNewWork(t *testing.T) {
	for _, f := range fixtures {
		t.Run(f.tag, func(t *testing.T) {
			s := openFixture(t, f.tag)
			ctx := context.Background()

			token, hash, err := store.NewToken()
			if err != nil {
				t.Fatalf("NewToken: %v", err)
			}
			secret, err := store.NewFanoutSecret()
			if err != nil {
				t.Fatalf("NewFanoutSecret: %v", err)
			}
			want := store.Process{
				ID: "01930000-0000-7000-8000-0000000000c1", Owner: "loki",
				Package: "/org/handbook", Name: "handbook", Container: "kitbash-handbook-2",
				Digest: "sha256:cccc", Expose: "mcp", Endpoint: "http://127.0.0.1:8082",
				Subscriptions: []string{store.SubscriptionTelemetry},
				Permits:       manifest.Permits{Tools: []string{"fs_read"}, Paths: []string{"/org"}},
				Mounts: []mounts.Resolved{
					{Source: "/home/loki/notes", Target: "/files/notes", Mode: mounts.ModeRO},
					{Source: "/home/loki/out", Target: "/files/out", Mode: mounts.ModeRW},
				},
				FanoutSecret: secret,
				RegisteredAt: fixtureBase,
			}
			if err := s.RegisterProcess(ctx, want, hash, 0); err != nil {
				t.Fatalf("RegisterProcess: %v", err)
			}
			got, found, err := s.ProcessByToken(ctx, token)
			if err != nil || !found {
				t.Fatalf("ProcessByToken: %v, found %v", err, found)
			}
			if got.Container != want.Container || got.Digest != want.Digest ||
				got.FanoutSecret != secret || strings.Join(got.Permits.Tools, ",") != "fs_read" {
				t.Errorf("the new registration reads back as %+v, want the columns it was written with", got)
			}
			// The mounts are the column an upgraded store gained last, and a
			// Process that reads back without them is one that would come
			// back after a reboot seeing no Files.
			if len(got.Mounts) != 2 || got.Mounts[0].Source != "/home/loki/notes" ||
				got.Mounts[1].Mode != mounts.ModeRW {
				t.Errorf("the new registration holds the mounts %+v, want the two it was written with", got.Mounts)
			}

			id := "01930000-0000-7000-8000-0000000000c2"
			if err := s.CreateApproval(ctx, store.Approval{
				ID: id, Requester: "loki", Tool: "proc_run",
				Input:       json.RawMessage(`{"package":"/org/handbook"}`),
				RequestedAt: fixtureBase,
			}, 0); err != nil {
				t.Fatalf("CreateApproval: %v", err)
			}
			queued, found, err := s.Approval(ctx, id)
			if err != nil || !found {
				t.Fatalf("Approval: %v, found %v", err, found)
			}
			if queued.State != store.StatePending {
				t.Errorf("the new approval is %s, want pending", queued.State)
			}

			// Every filter of the query surface, over the records the fixture
			// wrote. A filter for an attribute the era never stamped matches
			// nothing, which is the answer and not an error.
			yes, no := true, false
			for _, c := range []struct {
				name   string
				filter store.Filter
				rows   int
			}{
				{"user", store.Filter{User: "loki"}, 2},
				{"package", store.Filter{Package: "/home/kilo/tool"}, 1},
				{"process", store.Filter{Process: "p-alpha"}, 1},
				{"path", store.Filter{Path: "/org/handbook"}, 1},
				{"tool", store.Filter{Tool: "pkg_build"}, 1},
				{"eval true", store.Filter{Eval: &yes}, 1},
				{"eval false", store.Filter{Eval: &no}, 1},
				{"producer", store.Filter{Producer: "p-alpha"}, boolCount(f.producer, 1)},
				{"caller", store.Filter{Caller: "p-beta"}, boolCount(f.caller, 1)},
				{"internal true", store.Filter{Internal: &yes}, 0},
				{"internal false", store.Filter{Internal: &no}, f.spans},
				{"time range", store.Filter{
					Since: fixtureBase.Add(-150 * time.Minute),
					Until: fixtureBase.Add(-90 * time.Minute)}, 1},
				{"every filter at once", store.Filter{
					User: "kilo", Package: "/home/kilo/tool", Path: "/home/kilo/tool",
					Tool: "pkg_build", Eval: &yes, Producer: producerOf(f, "p-alpha"),
					Caller: callerOf(f, "p-beta"), Internal: &no,
					Since: fixtureBase.Add(-24 * time.Hour), Until: fixtureBase,
					Limit: store.MaxLimit}, 1},
			} {
				page, err := s.Query(ctx, store.SignalTraces, c.filter)
				if err != nil {
					t.Fatalf("query by %s: %v", c.name, err)
				}
				if len(page.Spans) != c.rows {
					t.Errorf("the %s filter matched %d spans, want %d", c.name, len(page.Spans), c.rows)
				}
			}

			// The sweep runs last, because the second one empties the store.
			// Nothing in the fixture is older than its own retention at the
			// moment the fixture was written.
			kept, err := s.Sweep(ctx, fixtureBase)
			if err != nil {
				t.Fatalf("Sweep: %v", err)
			}
			if kept.Total() != 0 {
				t.Errorf("the sweep removed %+v at the fixture's own time, want nothing", kept)
			}
			gone, err := s.Sweep(ctx, fixtureBase.Add(365*24*time.Hour))
			if err != nil {
				t.Fatalf("Sweep a year on: %v", err)
			}
			if gone.Traces != int64(f.spans) || gone.Logs != int64(f.logs) || gone.Metrics != int64(f.metrics) {
				t.Errorf("the sweep a year on removed %+v, want every record of the fixture", gone)
			}
		})
	}
}

// TestMigratedSchemaEqualsAFreshOne is the one that fails loudly when a
// migration step is missed: after the upgrade the fixture holds the same
// tables, the same indexes and the same columns as a store created today.
func TestMigratedSchemaEqualsAFreshOne(t *testing.T) {
	fresh := freshStorePath(t)
	wantNames := masterNames(t, fresh)
	wantColumns := tableColumns(t, fresh, wantNames)

	for _, f := range fixtures {
		t.Run(f.tag, func(t *testing.T) {
			path := copyFixture(t, f.tag)
			s, err := store.Open(path)
			if err != nil {
				t.Fatalf("open the %s fixture: %v", f.tag, err)
			}
			if err := s.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			gotNames := masterNames(t, path)
			for _, line := range differences(gotNames, wantNames) {
				t.Errorf("the schema of the migrated store differs: %s", line)
			}
			gotColumns := tableColumns(t, path, gotNames)
			for table, want := range wantColumns {
				got, ok := gotColumns[table]
				if !ok {
					t.Errorf("the migrated store has no %s table", table)
					continue
				}
				for _, line := range differences(got, want) {
					t.Errorf("the %s table of the migrated store differs: %s", table, line)
				}
			}
		})
	}
}

// TestNoColumnWasDroppedOrRenamed is the downgrade note. An older kitbashd
// reads the file an upgrade left behind, and it selects the columns it was
// built with by name, so a migration may only add: every column any released
// store wrote is still there under the same name in a fresh one.
func TestNoColumnWasDroppedOrRenamed(t *testing.T) {
	fresh := freshStorePath(t)
	freshColumns := columnNames(t, fresh)

	for _, f := range fixtures {
		t.Run(f.tag, func(t *testing.T) {
			// The fixture is read as it was written, not migrated, because
			// the question is what the old binary knows about.
			for table, columns := range columnNames(t, copyFixture(t, f.tag)) {
				for _, column := range columns {
					if !has(freshColumns[table], column) {
						t.Errorf("%s.%s was written by %s and a fresh store has no such column: "+
							"a downgraded kitbashd selects it by name", table, column, f.tag)
					}
				}
			}
		})
	}
}

// freshStorePath creates a store with the current code and returns its file,
// closed, so it can be read as a schema.
func freshStorePath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fresh.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open a fresh store: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close a fresh store: %v", err)
	}
	return path
}

// openRaw reads a store file without the store package, so nothing is migrated
// by the act of looking.
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// masterNames is every table and index of a store, sorted, with the ones
// SQLite creates for a primary key included: they are part of the shape.
//
// An index carries its definition and not only its name. CREATE INDEX IF NOT
// EXISTS keeps the index an old store already has, so an index that is
// redefined over other columns under the same name is never reapplied on an
// upgrade, and a comparison of names alone would call that migrated store
// equal to a fresh one. A table carries its name only: what a table is made of
// is compared column by column below, and ALTER TABLE rewrites the statement
// SQLite stored for it, so its text differs between the two by construction.
func masterNames(t *testing.T, path string) []string {
	t.Helper()
	rows, err := openRaw(t, path).Query(
		"SELECT type, name, ifnull(sql, '') FROM sqlite_master WHERE type IN ('table','index') ORDER BY type, name")
	if err != nil {
		t.Fatalf("read the schema of %s: %v", path, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var kind, name, statement string
		if err := rows.Scan(&kind, &name, &statement); err != nil {
			t.Fatalf("read the schema of %s: %v", path, err)
		}
		entry := kind + " " + name
		if kind == "index" {
			// The alignment inside the statement is not the schema: the
			// columns it names are. An index SQLite created for a primary key
			// has no statement at all, which is the empty string.
			entry += " " + strings.Join(strings.Fields(statement), " ")
		}
		names = append(names, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the schema of %s: %v", path, err)
	}
	sort.Strings(names)
	return names
}

// tableColumns is every column of every table, by name, with its type, its
// null constraint and its default. The order columns were added in is not
// compared: a migration appends and a fresh store declares, so the same
// schema arrives in two orders.
func tableColumns(t *testing.T, path string, names []string) map[string][]string {
	t.Helper()
	db := openRaw(t, path)
	out := map[string][]string{}
	for _, name := range names {
		table, ok := strings.CutPrefix(name, "table ")
		if !ok || strings.HasPrefix(table, "sqlite_") {
			continue
		}
		rows, err := db.Query("PRAGMA table_info(" + table + ")")
		if err != nil {
			t.Fatalf("read the columns of %s: %v", table, err)
		}
		var columns []string
		for rows.Next() {
			var index, notNull, pk int
			var column, kind string
			var dflt sql.NullString
			if err := rows.Scan(&index, &column, &kind, &notNull, &dflt, &pk); err != nil {
				rows.Close()
				t.Fatalf("read the columns of %s: %v", table, err)
			}
			columns = append(columns, fmt.Sprintf("%s %s notnull=%d default=%q pk=%d",
				column, kind, notNull, dflt.String, pk))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("read the columns of %s: %v", table, err)
		}
		rows.Close()
		sort.Strings(columns)
		out[table] = columns
	}
	return out
}

// columnNames is the same read reduced to the names, which is what the
// downgrade note is about.
func columnNames(t *testing.T, path string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for table, columns := range tableColumns(t, path, masterNames(t, path)) {
		for _, column := range columns {
			out[table] = append(out[table], column[:strings.Index(column, " ")])
		}
	}
	return out
}

// differences reports what one sorted schema listing holds that the other does
// not, in both directions. A missed migration step is one or two lines of it,
// so the failure names them rather than printing two whole schemas to compare
// by eye.
func differences(got, want []string) []string {
	var lines []string
	for _, entry := range got {
		if !has(want, entry) {
			lines = append(lines, "the migrated store has "+entry+" and a fresh one does not")
		}
	}
	for _, entry := range want {
		if !has(got, entry) {
			lines = append(lines, "a fresh store has "+entry+" and the migrated one does not")
		}
	}
	return lines
}

func has(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

// producerOf and callerOf are what a record of that era carries: the value the
// fixture wrote, or nothing at all for a release that had no such attribute.
func producerOf(f fixture, value string) string {
	if f.producer {
		return value
	}
	return ""
}

func callerOf(f fixture, value string) string {
	if f.caller {
		return value
	}
	return ""
}

func boolCount(stamped bool, rows int) int {
	if stamped {
		return rows
	}
	return 0
}
