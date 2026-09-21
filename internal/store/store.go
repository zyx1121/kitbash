// Package store holds Telemetry: the spans, log records and metric points
// kitbashd receives over OTLP, plus the retention settings that bound them.
// It is one SQLite file opened through a pure Go driver, see PLAN.md section
// 4.7, and it holds no transport types the way internal/fs holds none.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go driver, so the binary stays static
)

// logger writes where the operator reads, the same shape internal/daemon uses.
// This package says almost nothing: what it writes is a record it read and
// could not make sense of, which is the operator's to see and nobody else's.
var logger = log.New(os.Stderr, "kitbashd: ", log.LstdFlags)

// Signals a query and a retention window can name, see spec/mcp-surface.yaml.
const (
	SignalTraces  = "traces"
	SignalLogs    = "logs"
	SignalMetrics = "metrics"
)

// Statuses a span can carry, the short form of the OTLP status code.
const (
	StatusOK    = "ok"
	StatusError = "error"
	StatusUnset = "unset"
)

// DefaultLimit and MaxLimit bound one query page, see the tel_query schema.
const (
	DefaultLimit = 100
	MaxLimit     = 1000
)

// FileMode and DirMode are what the store and its directory are kept at. The
// socket is the only way to the records inside.
const (
	FileMode = 0o600
	DirMode  = 0o700
)

// Attributes are the kitbash attributes in their short form. A record carries
// them as typed columns so a query never parses JSON to filter, see PLAN.md
// section 2.4. Other holds every remaining attribute by its wire name.
//
// Producer is who wrote the record: the member for a session record, the
// Process id for one that arrived with a Process token. It is stamped by the
// daemon and never read from the body.
//
// Caller is the Process whose MCP session recorded the record. The producer
// sends the credential of its session and the daemon rewrites it to the
// Process id, so this column is never what a producer claimed, see PLAN.md
// section 2.3. It is empty for a record a member's own session wrote.
//
// Internal marks a record as the cause of an internal problem, which only an
// admin reads: it carries host paths and third party output, and a member is
// given the problem's instance to quote instead, see PLAN.md section 2.4.
// Unit is the unit of a Process one record is about, for a Package that runs
// as a pod. kitbashd stamps it on the records it writes about a unit itself,
// and a producer that sends one keeps it only when it names a unit of that
// Process, see PLAN.md sections 2.4 and 5.6. A record about a Process of one
// unit carries none. Its column is kitbash_unit, because the metrics table
// already has a unit, which is the unit of measure of a data point; for the
// same reason a Metric's own Unit is the measure and this one is reached
// through the embedded Attributes.
type Attributes struct {
	User     string         `json:"user,omitempty"`
	Package  string         `json:"package,omitempty"`
	Process  string         `json:"process,omitempty"`
	Unit     string         `json:"unit,omitempty"`
	Path     string         `json:"path,omitempty"`
	Tool     string         `json:"tool,omitempty"`
	Eval     *bool          `json:"eval,omitempty"`
	Producer string         `json:"producer,omitempty"`
	Caller   string         `json:"caller,omitempty"`
	Internal *bool          `json:"internal,omitempty"`
	Other    map[string]any `json:"-"`
}

// Span is one record of the traces signal.
type Span struct {
	TraceID       string
	SpanID        string
	ParentSpanID  string
	Name          string
	StartNS       int64
	EndNS         int64
	Status        string
	StatusMessage string
	Attributes
}

// Log is one record of the logs signal.
type Log struct {
	TimeNS   int64
	Severity string
	Body     string
	TraceID  string
	SpanID   string
	Attributes
}

// Metric is one record of the metrics signal: a single data point, already
// flattened. No producer emits metrics before M4, see PLAN.md section 5.5.
//
// Unit here is the unit of measure and shadows the kitbash attribute of the
// same name: the kitbash unit of a metric is Attributes.Unit, written out.
type Metric struct {
	TimeNS int64
	Name   string
	Value  float64
	Unit   string
	Attributes
}

// Export is one export request, stored whole or not at all. Partial success is
// not used, see spec/kitbashd-api.yaml.
type Export struct {
	Spans   []Span
	Logs    []Log
	Metrics []Metric
}

// Empty reports whether the export carries no records at all.
func (e Export) Empty() bool {
	return len(e.Spans) == 0 && len(e.Logs) == 0 && len(e.Metrics) == 0
}

// Filter selects records of one signal. The zero value matches everything
// within the time range, which is what an admin querying the whole machine
// sends. Path is a prefix match, every other string is exact.
//
// Internal selects the causes of internal problems: true returns only them,
// false only the records that are not one, and nil both. The daemon sends
// false for every caller who is not an admin, see PLAN.md section 2.4.
type Filter struct {
	User     string
	Package  string
	Process  string
	Unit     string
	Path     string
	Tool     string
	Eval     *bool
	Producer string
	Caller   string
	Internal *bool
	Since    time.Time
	Until    time.Time
	Limit    int
}

// Page is one answer to Query. Exactly one slice is populated, the one the
// signal names, newest first. Truncated is true when more records matched than
// the limit asked for.
type Page struct {
	Signal    string
	Spans     []Span
	Logs      []Log
	Metrics   []Metric
	Truncated bool
}

// Retention is the window per signal, as a duration such as 720h or 30d.
type Retention struct {
	Traces  string `json:"traces"`
	Logs    string `json:"logs"`
	Metrics string `json:"metrics"`
}

// DefaultRetention is what a fresh store answers, see PLAN.md section 2.4.
func DefaultRetention() Retention {
	return Retention{Traces: "30d", Logs: "14d", Metrics: "30d"}
}

// RetentionSet is a change to one or more windows. A nil field is left alone.
type RetentionSet struct {
	Traces  *string `json:"traces,omitempty"`
	Logs    *string `json:"logs,omitempty"`
	Metrics *string `json:"metrics,omitempty"`
}

// Empty reports whether the change names no signal at all.
func (r RetentionSet) Empty() bool {
	return r.Traces == nil && r.Logs == nil && r.Metrics == nil
}

// SweepCounts is how many records one sweep deleted per signal.
type SweepCounts struct {
	Traces  int64 `json:"traces"`
	Logs    int64 `json:"logs"`
	Metrics int64 `json:"metrics"`
}

// Total is the sum across signals, which is what decides whether a sweep is
// worth a log line.
func (c SweepCounts) Total() int64 { return c.Traces + c.Logs + c.Metrics }

// Store is the Telemetry database. It is safe for concurrent use.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens or creates the store at path. The file is journalled in WAL mode
// so a sweep never blocks an export, and every statement waits up to five
// seconds for a lock rather than failing busy.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty path")
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// One writer at a time. SQLite serialises writes anyway and a single
	// connection turns lock contention into queueing inside the process.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// The file holds every member's Telemetry, so only the daemon reads it.
	// This happens before the first write, so the journal and shared memory
	// files SQLite creates from it inherit the same mode.
	if err := os.Chmod(path, FileMode); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: chmod %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(indexes); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: indexes: %w", err)
	}
	return &Store{db: db, path: path}, nil
}

// Path is the file the store was opened from, which health reports.
func (s *Store) Path() string { return s.path }

// Close flushes and closes the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	// A WAL checkpoint on the way out keeps the sidecar files from carrying
	// records a reader of the main file would miss.
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		s.db.Close()
		return fmt.Errorf("store: checkpoint: %w", err)
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: close: %w", err)
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS spans (
  id             INTEGER PRIMARY KEY,
  trace_id       TEXT    NOT NULL,
  span_id        TEXT    NOT NULL,
  parent_span_id TEXT    NOT NULL DEFAULT '',
  name           TEXT    NOT NULL,
  start_ns       INTEGER NOT NULL,
  end_ns         INTEGER NOT NULL,
  status         TEXT    NOT NULL DEFAULT 'unset',
  status_message TEXT    NOT NULL DEFAULT '',
  user           TEXT    NOT NULL DEFAULT '',
  package        TEXT    NOT NULL DEFAULT '',
  process        TEXT    NOT NULL DEFAULT '',
  kitbash_unit   TEXT    NOT NULL DEFAULT '',
  path           TEXT    NOT NULL DEFAULT '',
  tool           TEXT    NOT NULL DEFAULT '',
  eval           INTEGER,
  producer       TEXT    NOT NULL DEFAULT '',
  caller         TEXT    NOT NULL DEFAULT '',
  internal       INTEGER,
  other          TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS logs (
  id        INTEGER PRIMARY KEY,
  time_ns   INTEGER NOT NULL,
  severity  TEXT    NOT NULL DEFAULT '',
  body      TEXT    NOT NULL DEFAULT '',
  trace_id  TEXT    NOT NULL DEFAULT '',
  span_id   TEXT    NOT NULL DEFAULT '',
  user      TEXT    NOT NULL DEFAULT '',
  package   TEXT    NOT NULL DEFAULT '',
  process   TEXT    NOT NULL DEFAULT '',
  kitbash_unit TEXT NOT NULL DEFAULT '',
  path      TEXT    NOT NULL DEFAULT '',
  tool      TEXT    NOT NULL DEFAULT '',
  eval      INTEGER,
  producer  TEXT    NOT NULL DEFAULT '',
  caller    TEXT    NOT NULL DEFAULT '',
  internal  INTEGER,
  other     TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS metrics (
  id      INTEGER PRIMARY KEY,
  time_ns INTEGER NOT NULL,
  name    TEXT    NOT NULL,
  value   REAL    NOT NULL DEFAULT 0,
  unit    TEXT    NOT NULL DEFAULT '',
  user    TEXT    NOT NULL DEFAULT '',
  package TEXT    NOT NULL DEFAULT '',
  process TEXT    NOT NULL DEFAULT '',
  kitbash_unit TEXT NOT NULL DEFAULT '',
  path    TEXT    NOT NULL DEFAULT '',
  tool    TEXT    NOT NULL DEFAULT '',
  eval    INTEGER,
  producer TEXT    NOT NULL DEFAULT '',
  caller  TEXT    NOT NULL DEFAULT '',
  internal INTEGER,
  other   TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS processes (
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
  schedule      TEXT    NOT NULL DEFAULT '',
  token_hash    TEXT    NOT NULL,
  fanout_secret TEXT    NOT NULL DEFAULT '',
  registered_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS removals (
  name        TEXT    PRIMARY KEY,
  state       TEXT    NOT NULL DEFAULT 'removing',
  step        TEXT    NOT NULL DEFAULT '',
  archived    TEXT    NOT NULL DEFAULT '',
  started_at  INTEGER NOT NULL,
  finished_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS builds (
  id         INTEGER PRIMARY KEY,
  path       TEXT    NOT NULL,
  commit_sha TEXT    NOT NULL,
  digest     TEXT    NOT NULL,
  builder    TEXT    NOT NULL,
  built_at   INTEGER NOT NULL,
  size       INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS approvals (
  id           TEXT    PRIMARY KEY,
  requester    TEXT    NOT NULL,
  tool         TEXT    NOT NULL,
  input        TEXT    NOT NULL DEFAULT '',
  state        TEXT    NOT NULL DEFAULT 'pending',
  requested_at INTEGER NOT NULL,
  decided_at   INTEGER NOT NULL DEFAULT 0,
  decided_by   TEXT    NOT NULL DEFAULT '',
  note         TEXT    NOT NULL DEFAULT '',
  reason       TEXT    NOT NULL DEFAULT '',
  result       TEXT    NOT NULL DEFAULT ''
);
`

// indexes are created after the migration, because one of them is over a
// column a store written by an earlier version does not have yet.
const indexes = `
CREATE INDEX IF NOT EXISTS spans_start    ON spans(start_ns);
CREATE INDEX IF NOT EXISTS spans_user     ON spans(user, start_ns);
CREATE INDEX IF NOT EXISTS spans_package  ON spans(package, start_ns);
CREATE INDEX IF NOT EXISTS spans_producer ON spans(producer, start_ns);
CREATE INDEX IF NOT EXISTS spans_caller   ON spans(caller, start_ns);
CREATE INDEX IF NOT EXISTS spans_internal ON spans(internal, start_ns);
CREATE INDEX IF NOT EXISTS spans_unit     ON spans(kitbash_unit, start_ns);

CREATE INDEX IF NOT EXISTS logs_time     ON logs(time_ns);
CREATE INDEX IF NOT EXISTS logs_user     ON logs(user, time_ns);
CREATE INDEX IF NOT EXISTS logs_package  ON logs(package, time_ns);
CREATE INDEX IF NOT EXISTS logs_producer ON logs(producer, time_ns);
CREATE INDEX IF NOT EXISTS logs_caller   ON logs(caller, time_ns);
CREATE INDEX IF NOT EXISTS logs_internal ON logs(internal, time_ns);
CREATE INDEX IF NOT EXISTS logs_unit     ON logs(kitbash_unit, time_ns);

CREATE INDEX IF NOT EXISTS metrics_time     ON metrics(time_ns);
CREATE INDEX IF NOT EXISTS metrics_user     ON metrics(user, time_ns);
CREATE INDEX IF NOT EXISTS metrics_package  ON metrics(package, time_ns);
CREATE INDEX IF NOT EXISTS metrics_producer ON metrics(producer, time_ns);
CREATE INDEX IF NOT EXISTS metrics_caller   ON metrics(caller, time_ns);
CREATE INDEX IF NOT EXISTS metrics_internal ON metrics(internal, time_ns);
CREATE INDEX IF NOT EXISTS metrics_unit     ON metrics(kitbash_unit, time_ns);

CREATE INDEX IF NOT EXISTS processes_owner ON processes(owner);
CREATE UNIQUE INDEX IF NOT EXISTS processes_token ON processes(token_hash);

CREATE INDEX IF NOT EXISTS removals_state ON removals(state);

CREATE UNIQUE INDEX IF NOT EXISTS builds_unique ON builds(path, commit_sha, digest);
CREATE INDEX IF NOT EXISTS builds_path        ON builds(path, commit_sha);
CREATE INDEX IF NOT EXISTS builds_builder     ON builds(builder);
CREATE INDEX IF NOT EXISTS builds_by_builder  ON builds(path, builder);

CREATE INDEX IF NOT EXISTS approvals_requester ON approvals(requester, state);
CREATE INDEX IF NOT EXISTS approvals_state     ON approvals(state);
`

// Insert writes one export request in a single transaction, so a request that
// fails halfway leaves nothing behind.
func (s *Store) Insert(ctx context.Context, e Export) error {
	if e.Empty() {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()

	if len(e.Spans) > 0 {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO spans
			(trace_id, span_id, parent_span_id, name, start_ns, end_ns, status, status_message,
			 user, package, process, kitbash_unit, path, tool, eval, producer, caller, internal, other)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return fmt.Errorf("store: prepare spans: %w", err)
		}
		defer stmt.Close()
		for _, sp := range e.Spans {
			other, err := encodeOther(sp.Other)
			if err != nil {
				return err
			}
			if _, err := stmt.ExecContext(ctx, sp.TraceID, sp.SpanID, sp.ParentSpanID, sp.Name,
				sp.StartNS, sp.EndNS, status(sp.Status), sp.StatusMessage,
				sp.User, sp.Package, sp.Process, sp.Unit, sp.Path, sp.Tool, boolArg(sp.Eval), sp.Producer, sp.Caller,
				boolArg(sp.Internal), other); err != nil {
				return fmt.Errorf("store: insert span: %w", err)
			}
		}
	}
	if len(e.Logs) > 0 {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO logs
			(time_ns, severity, body, trace_id, span_id, user, package, process, kitbash_unit, path, tool,
			 eval, producer, caller, internal, other)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return fmt.Errorf("store: prepare logs: %w", err)
		}
		defer stmt.Close()
		for _, l := range e.Logs {
			other, err := encodeOther(l.Other)
			if err != nil {
				return err
			}
			if _, err := stmt.ExecContext(ctx, l.TimeNS, l.Severity, l.Body, l.TraceID, l.SpanID,
				l.User, l.Package, l.Process, l.Unit, l.Path, l.Tool, boolArg(l.Eval), l.Producer, l.Caller,
				boolArg(l.Internal), other); err != nil {
				return fmt.Errorf("store: insert log: %w", err)
			}
		}
	}
	if len(e.Metrics) > 0 {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO metrics
			(time_ns, name, value, unit, user, package, process, kitbash_unit, path, tool, eval, producer,
			 caller, internal, other)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return fmt.Errorf("store: prepare metrics: %w", err)
		}
		defer stmt.Close()
		for _, m := range e.Metrics {
			// The receiver drops these before they reach here. The check is
			// repeated because a value JSON cannot spell would make every
			// later query of its window fail, which is too expensive a
			// failure to leave to one caller getting it right.
			if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
				return fmt.Errorf("store: the metric %q carries a value JSON cannot represent", m.Name)
			}
			other, err := encodeOther(m.Other)
			if err != nil {
				return err
			}
			if _, err := stmt.ExecContext(ctx, m.TimeNS, m.Name, m.Value, m.Unit,
				m.User, m.Package, m.Process, m.Attributes.Unit, m.Path, m.Tool, boolArg(m.Eval), m.Producer, m.Caller,
				boolArg(m.Internal), other); err != nil {
				return fmt.Errorf("store: insert metric: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// Query answers one page of a signal, newest first. It reads one record more
// than the limit to learn whether more matched, and reports that as Truncated
// without returning the extra record.
func (s *Store) Query(ctx context.Context, signal string, f Filter) (Page, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	page := Page{Signal: signal}

	timeColumn := "time_ns"
	table := ""
	switch signal {
	case SignalTraces:
		table, timeColumn = "spans", "start_ns"
	case SignalLogs:
		table = "logs"
	case SignalMetrics:
		table = "metrics"
	default:
		return page, fmt.Errorf("store: unknown signal %q", signal)
	}

	columns := map[string]string{
		"spans":   "trace_id, span_id, parent_span_id, name, start_ns, end_ns, status, status_message, user, package, process, kitbash_unit, path, tool, eval, producer, caller, internal, other",
		"logs":    "time_ns, severity, body, trace_id, span_id, user, package, process, kitbash_unit, path, tool, eval, producer, caller, internal, other",
		"metrics": "time_ns, name, value, unit, user, package, process, kitbash_unit, path, tool, eval, producer, caller, internal, other",
	}[table]

	where, args := conditions(timeColumn, f)
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY %s DESC, id DESC LIMIT ?",
		columns, table, where, timeColumn)
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return page, fmt.Errorf("store: query %s: %w", signal, err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		count++
		if count > limit {
			page.Truncated = true
			break
		}
		switch signal {
		case SignalTraces:
			var sp Span
			var eval, internal sql.NullBool
			var other string
			if err := rows.Scan(&sp.TraceID, &sp.SpanID, &sp.ParentSpanID, &sp.Name, &sp.StartNS, &sp.EndNS,
				&sp.Status, &sp.StatusMessage, &sp.User, &sp.Package, &sp.Process, &sp.Unit, &sp.Path, &sp.Tool,
				&eval, &sp.Producer, &sp.Caller, &internal, &other); err != nil {
				return page, fmt.Errorf("store: scan span: %w", err)
			}
			sp.Eval = nullBool(eval)
			sp.Internal = nullBool(internal)
			if sp.Other, err = decodeOther(other); err != nil {
				return page, err
			}
			page.Spans = append(page.Spans, sp)
		case SignalLogs:
			var l Log
			var eval, internal sql.NullBool
			var other string
			if err := rows.Scan(&l.TimeNS, &l.Severity, &l.Body, &l.TraceID, &l.SpanID,
				&l.User, &l.Package, &l.Process, &l.Unit, &l.Path, &l.Tool, &eval, &l.Producer, &l.Caller,
				&internal, &other); err != nil {
				return page, fmt.Errorf("store: scan log: %w", err)
			}
			l.Eval = nullBool(eval)
			l.Internal = nullBool(internal)
			if l.Other, err = decodeOther(other); err != nil {
				return page, err
			}
			page.Logs = append(page.Logs, l)
		case SignalMetrics:
			var m Metric
			var eval, internal sql.NullBool
			var other string
			if err := rows.Scan(&m.TimeNS, &m.Name, &m.Value, &m.Unit,
				&m.User, &m.Package, &m.Process, &m.Attributes.Unit, &m.Path, &m.Tool, &eval, &m.Producer, &m.Caller,
				&internal, &other); err != nil {
				return page, fmt.Errorf("store: scan metric: %w", err)
			}
			m.Eval = nullBool(eval)
			m.Internal = nullBool(internal)
			if m.Other, err = decodeOther(other); err != nil {
				return page, err
			}
			page.Metrics = append(page.Metrics, m)
		}
	}
	if err := rows.Err(); err != nil {
		return page, fmt.Errorf("store: query %s: %w", signal, err)
	}
	return page, nil
}

// conditions builds the WHERE clause shared by every signal. The time range is
// always bounded: the caller resolves the defaults before it gets here.
func conditions(timeColumn string, f Filter) (string, []any) {
	var clauses []string
	var args []any
	add := func(clause string, arg any) {
		clauses = append(clauses, clause)
		args = append(args, arg)
	}
	if !f.Since.IsZero() {
		add(timeColumn+" >= ?", f.Since.UnixNano())
	}
	if !f.Until.IsZero() {
		add(timeColumn+" <= ?", f.Until.UnixNano())
	}
	if f.User != "" {
		add("user = ?", f.User)
	}
	if f.Package != "" {
		add("package = ?", f.Package)
	}
	if f.Process != "" {
		add("process = ?", f.Process)
	}
	if f.Unit != "" {
		add("kitbash_unit = ?", f.Unit)
	}
	if f.Tool != "" {
		add("tool = ?", f.Tool)
	}
	if f.Path != "" {
		add(`path LIKE ? ESCAPE '\'`, likePrefix(f.Path))
	}
	if f.Eval != nil {
		add("eval = ?", *f.Eval)
	}
	if f.Producer != "" {
		add("producer = ?", f.Producer)
	}
	if f.Caller != "" {
		add("caller = ?", f.Caller)
	}
	// A record that is not an internal cause carries no internal at all, so
	// excluding them is a test for NULL as well as for false: "internal = 0"
	// alone would return nothing and every member's query would be empty.
	if f.Internal != nil {
		if *f.Internal {
			add("internal = ?", true)
		} else {
			clauses = append(clauses, "(internal IS NULL OR internal = 0)")
		}
	}
	if len(clauses) == 0 {
		return "1", nil
	}
	return strings.Join(clauses, " AND "), args
}

// likePrefix turns a path into a LIKE pattern that matches it and everything
// under it, with the wildcard characters of LIKE escaped.
func likePrefix(prefix string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(prefix) + "%"
}

// Retention reads the window per signal, falling back to the defaults for a
// signal no admin has set.
func (s *Store) Retention(ctx context.Context) (Retention, error) {
	r := DefaultRetention()
	rows, err := s.db.QueryContext(ctx, "SELECT key, value FROM settings WHERE key IN (?,?,?)",
		settingKey(SignalTraces), settingKey(SignalLogs), settingKey(SignalMetrics))
	if err != nil {
		return r, fmt.Errorf("store: read retention: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return r, fmt.Errorf("store: read retention: %w", err)
		}
		switch key {
		case settingKey(SignalTraces):
			r.Traces = value
		case settingKey(SignalLogs):
			r.Logs = value
		case settingKey(SignalMetrics):
			r.Metrics = value
		}
	}
	if err := rows.Err(); err != nil {
		return r, fmt.Errorf("store: read retention: %w", err)
	}
	return r, nil
}

// SetRetention changes the named windows and returns the full retention after
// the change. Every value is validated before anything is written.
func (s *Store) SetRetention(ctx context.Context, set RetentionSet) (Retention, error) {
	changes := map[string]string{}
	for signal, value := range map[string]*string{
		SignalTraces:  set.Traces,
		SignalLogs:    set.Logs,
		SignalMetrics: set.Metrics,
	} {
		if value == nil {
			continue
		}
		if _, err := ParseDuration(*value); err != nil {
			return Retention{}, err
		}
		changes[signal] = *value
	}
	if len(changes) == 0 {
		return s.Retention(ctx)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Retention{}, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()
	for signal, value := range changes {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO settings (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
			settingKey(signal), value); err != nil {
			return Retention{}, fmt.Errorf("store: write retention: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Retention{}, fmt.Errorf("store: commit: %w", err)
	}
	return s.Retention(ctx)
}

// Sweep deletes every record older than its signal's window and reports how
// many went. It runs on start and once an hour, see PLAN.md section 2.4.
func (s *Store) Sweep(ctx context.Context, now time.Time) (SweepCounts, error) {
	var counts SweepCounts
	r, err := s.Retention(ctx)
	if err != nil {
		return counts, err
	}
	for _, sweep := range []struct {
		table  string
		column string
		window string
		count  *int64
	}{
		{"spans", "start_ns", r.Traces, &counts.Traces},
		{"logs", "time_ns", r.Logs, &counts.Logs},
		{"metrics", "time_ns", r.Metrics, &counts.Metrics},
	} {
		d, err := ParseDuration(sweep.window)
		if err != nil {
			return counts, err
		}
		n, err := s.deleteOlder(ctx, sweep.table, sweep.column, now.Add(-d).UnixNano())
		*sweep.count = n
		if err != nil {
			return counts, err
		}
	}
	return counts, nil
}

// SweepBatch is how many records one delete statement removes. The store has
// a single writer, so an unbounded delete over a large table would hold it for
// the whole scan and every export would wait behind it. Deleting in batches
// gives the exports waiting on the connection a turn between statements.
const SweepBatch = 5000

// deleteOlder removes records before the cutoff, a batch at a time, and
// returns how many went. A batch that fails still reports what was already
// deleted, because those records are gone whatever the caller does next.
func (s *Store) deleteOlder(ctx context.Context, table, column string, cutoff int64) (int64, error) {
	// A subquery rather than DELETE ... LIMIT, which needs a compile time
	// option not every SQLite build carries.
	statement := fmt.Sprintf("DELETE FROM %s WHERE id IN (SELECT id FROM %s WHERE %s < ? ORDER BY id LIMIT %d)",
		table, table, column, SweepBatch)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := s.db.ExecContext(ctx, statement, cutoff)
		if err != nil {
			return total, fmt.Errorf("store: sweep %s: %w", table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("store: sweep %s: %w", table, err)
		}
		total += n
		if n < SweepBatch {
			return total, nil
		}
	}
}

// settingKey names the retention row of one signal.
func settingKey(signal string) string { return "retention." + signal }

// MinRetention is the shortest window an admin can set. A window of zero
// would delete every record on the next sweep, including the one being
// written, which is not a retention policy but a way to lose Telemetry.
const MinRetention = time.Hour

// MaxRetention is the longest window the arithmetic below can represent.
var MaxRetention = time.Duration(math.MaxInt64)

// ParseDuration reads a retention window. The accepted form is the one
// spec/mcp-surface.yaml declares, ^[0-9]+(h|d)$, and d is 24h.
//
// The multiplication is checked before it happens. A window that overflows
// time.Duration would wrap to a small one, and the next sweep would delete
// records the admin had just asked kitbash to keep for centuries.
func ParseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, invalidDuration(s)
	}
	unit := s[len(s)-1]
	digits := s[:len(s)-1]

	var scale int64
	switch unit {
	case 'h':
		scale = int64(time.Hour)
	case 'd':
		scale = int64(24 * time.Hour)
	default:
		return 0, invalidDuration(s)
	}

	var n int64
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, invalidDuration(s)
		}
		if n > (math.MaxInt64-int64(c-'0'))/10 {
			return 0, tooLongDuration(s)
		}
		n = n*10 + int64(c-'0')
		if n > math.MaxInt64/scale {
			return 0, tooLongDuration(s)
		}
	}
	d := time.Duration(n * scale)
	if d < MinRetention {
		return 0, fmt.Errorf("store: a retention window of %q is shorter than the minimum of %s", s, MinRetention)
	}
	return d, nil
}

func invalidDuration(s string) error {
	return fmt.Errorf("store: %q is not a duration such as 720h or 30d", s)
}

func tooLongDuration(s string) error {
	return fmt.Errorf("store: a retention window of %q is longer than kitbash can represent", s)
}

// status keeps an unknown status out of the column the query surface promises.
func status(s string) string {
	switch s {
	case StatusOK, StatusError:
		return s
	default:
		return StatusUnset
	}
}

// boolArg keeps an absent eval NULL rather than false, so a query for eval
// false does not match records that never carried the attribute.
func boolArg(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

func nullBool(v sql.NullBool) *bool {
	if !v.Valid {
		return nil
	}
	b := v.Bool
	return &b
}

func encodeOther(other map[string]any) (string, error) {
	if len(other) == 0 {
		return "", nil
	}
	b, err := json.Marshal(other)
	if err != nil {
		return "", fmt.Errorf("store: encode attributes: %w", err)
	}
	return string(b), nil
}

func decodeOther(raw string) (map[string]any, error) {
	if raw == "" {
		return nil, nil
	}
	var other map[string]any
	if err := json.Unmarshal([]byte(raw), &other); err != nil {
		return nil, fmt.Errorf("store: decode attributes: %w", err)
	}
	return other, nil
}
