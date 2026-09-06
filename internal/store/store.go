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
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go driver, so the binary stays static
)

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

// Attributes are the six kitbash attributes in their short form. A record
// carries them as typed columns so a query never parses JSON to filter, see
// PLAN.md section 2.4. Other holds every remaining attribute by its wire name.
type Attributes struct {
	User    string         `json:"user,omitempty"`
	Package string         `json:"package,omitempty"`
	Process string         `json:"process,omitempty"`
	Path    string         `json:"path,omitempty"`
	Tool    string         `json:"tool,omitempty"`
	Eval    *bool          `json:"eval,omitempty"`
	Other   map[string]any `json:"-"`
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
type Filter struct {
	User    string
	Package string
	Process string
	Path    string
	Tool    string
	Eval    *bool
	Since   time.Time
	Until   time.Time
	Limit   int
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
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
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
  path           TEXT    NOT NULL DEFAULT '',
  tool           TEXT    NOT NULL DEFAULT '',
  eval           INTEGER,
  other          TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS spans_start    ON spans(start_ns);
CREATE INDEX IF NOT EXISTS spans_user     ON spans(user, start_ns);
CREATE INDEX IF NOT EXISTS spans_package  ON spans(package, start_ns);

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
  path      TEXT    NOT NULL DEFAULT '',
  tool      TEXT    NOT NULL DEFAULT '',
  eval      INTEGER,
  other     TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS logs_time    ON logs(time_ns);
CREATE INDEX IF NOT EXISTS logs_user    ON logs(user, time_ns);
CREATE INDEX IF NOT EXISTS logs_package ON logs(package, time_ns);

CREATE TABLE IF NOT EXISTS metrics (
  id      INTEGER PRIMARY KEY,
  time_ns INTEGER NOT NULL,
  name    TEXT    NOT NULL,
  value   REAL    NOT NULL DEFAULT 0,
  unit    TEXT    NOT NULL DEFAULT '',
  user    TEXT    NOT NULL DEFAULT '',
  package TEXT    NOT NULL DEFAULT '',
  process TEXT    NOT NULL DEFAULT '',
  path    TEXT    NOT NULL DEFAULT '',
  tool    TEXT    NOT NULL DEFAULT '',
  eval    INTEGER,
  other   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS metrics_time    ON metrics(time_ns);
CREATE INDEX IF NOT EXISTS metrics_user    ON metrics(user, time_ns);
CREATE INDEX IF NOT EXISTS metrics_package ON metrics(package, time_ns);

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
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
			 user, package, process, path, tool, eval, other)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
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
				sp.User, sp.Package, sp.Process, sp.Path, sp.Tool, boolArg(sp.Eval), other); err != nil {
				return fmt.Errorf("store: insert span: %w", err)
			}
		}
	}
	if len(e.Logs) > 0 {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO logs
			(time_ns, severity, body, trace_id, span_id, user, package, process, path, tool, eval, other)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`)
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
				l.User, l.Package, l.Process, l.Path, l.Tool, boolArg(l.Eval), other); err != nil {
				return fmt.Errorf("store: insert log: %w", err)
			}
		}
	}
	if len(e.Metrics) > 0 {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO metrics
			(time_ns, name, value, unit, user, package, process, path, tool, eval, other)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return fmt.Errorf("store: prepare metrics: %w", err)
		}
		defer stmt.Close()
		for _, m := range e.Metrics {
			other, err := encodeOther(m.Other)
			if err != nil {
				return err
			}
			if _, err := stmt.ExecContext(ctx, m.TimeNS, m.Name, m.Value, m.Unit,
				m.User, m.Package, m.Process, m.Path, m.Tool, boolArg(m.Eval), other); err != nil {
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
		"spans":   "trace_id, span_id, parent_span_id, name, start_ns, end_ns, status, status_message, user, package, process, path, tool, eval, other",
		"logs":    "time_ns, severity, body, trace_id, span_id, user, package, process, path, tool, eval, other",
		"metrics": "time_ns, name, value, unit, user, package, process, path, tool, eval, other",
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
			var eval sql.NullBool
			var other string
			if err := rows.Scan(&sp.TraceID, &sp.SpanID, &sp.ParentSpanID, &sp.Name, &sp.StartNS, &sp.EndNS,
				&sp.Status, &sp.StatusMessage, &sp.User, &sp.Package, &sp.Process, &sp.Path, &sp.Tool,
				&eval, &other); err != nil {
				return page, fmt.Errorf("store: scan span: %w", err)
			}
			sp.Eval = nullBool(eval)
			if sp.Other, err = decodeOther(other); err != nil {
				return page, err
			}
			page.Spans = append(page.Spans, sp)
		case SignalLogs:
			var l Log
			var eval sql.NullBool
			var other string
			if err := rows.Scan(&l.TimeNS, &l.Severity, &l.Body, &l.TraceID, &l.SpanID,
				&l.User, &l.Package, &l.Process, &l.Path, &l.Tool, &eval, &other); err != nil {
				return page, fmt.Errorf("store: scan log: %w", err)
			}
			l.Eval = nullBool(eval)
			if l.Other, err = decodeOther(other); err != nil {
				return page, err
			}
			page.Logs = append(page.Logs, l)
		case SignalMetrics:
			var m Metric
			var eval sql.NullBool
			var other string
			if err := rows.Scan(&m.TimeNS, &m.Name, &m.Value, &m.Unit,
				&m.User, &m.Package, &m.Process, &m.Path, &m.Tool, &eval, &other); err != nil {
				return page, fmt.Errorf("store: scan metric: %w", err)
			}
			m.Eval = nullBool(eval)
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
	if f.Tool != "" {
		add("tool = ?", f.Tool)
	}
	if f.Path != "" {
		add(`path LIKE ? ESCAPE '\'`, likePrefix(f.Path))
	}
	if f.Eval != nil {
		add("eval = ?", *f.Eval)
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
		cutoff := now.Add(-d).UnixNano()
		res, err := s.db.ExecContext(ctx,
			fmt.Sprintf("DELETE FROM %s WHERE %s < ?", sweep.table, sweep.column), cutoff)
		if err != nil {
			return counts, fmt.Errorf("store: sweep %s: %w", sweep.table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return counts, fmt.Errorf("store: sweep %s: %w", sweep.table, err)
		}
		*sweep.count = n
	}
	return counts, nil
}

// settingKey names the retention row of one signal.
func settingKey(signal string) string { return "retention." + signal }

// ParseDuration reads a retention window. The accepted form is the one
// spec/mcp-surface.yaml declares, ^[0-9]+(h|d)$, and d is 24h.
func ParseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("store: %q is not a duration such as 720h or 30d", s)
	}
	unit := s[len(s)-1]
	digits := s[:len(s)-1]
	var n int64
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("store: %q is not a duration such as 720h or 30d", s)
		}
		n = n*10 + int64(c-'0')
		if n > 1<<40 {
			return 0, fmt.Errorf("store: %q is longer than kitbash keeps anything", s)
		}
	}
	switch unit {
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("store: %q is not a duration such as 720h or 30d", s)
	}
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
