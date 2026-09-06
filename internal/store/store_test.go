package store

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// open builds an empty store in a temporary directory.
func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "kitbashd.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func boolPtr(b bool) *bool { return &b }

// base is a fixed instant so a test never races the wall clock.
var base = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

// seed writes three spans, three logs and three metrics one hour apart.
func seed(t *testing.T, s *Store) {
	t.Helper()
	export := Export{
		Spans: []Span{
			{
				TraceID: "aa", SpanID: "01", Name: "fs_list",
				StartNS: base.Add(-3 * time.Hour).UnixNano(), EndNS: base.Add(-3*time.Hour + 5*time.Millisecond).UnixNano(),
				Status: StatusOK,
				Attributes: Attributes{User: "alice", Package: "/org/ffmpeg", Process: "p1",
					Path: "/org/handbook", Tool: "fs_list", Other: map[string]any{"http.status": float64(200)}},
			},
			{
				TraceID: "bb", SpanID: "02", Name: "pkg_build",
				StartNS: base.Add(-2 * time.Hour).UnixNano(), EndNS: base.Add(-2*time.Hour + time.Second).UnixNano(),
				Status: StatusError, StatusMessage: "build failed",
				Attributes: Attributes{User: "bob", Package: "/home/bob/tool", Path: "/home/bob/tool",
					Tool: "pkg_build", Eval: boolPtr(true)},
			},
			{
				TraceID: "cc", SpanID: "03", Name: "proc_run",
				StartNS: base.Add(-time.Hour).UnixNano(), EndNS: base.Add(-time.Hour + 2*time.Second).UnixNano(),
				Attributes: Attributes{User: "alice", Package: "/org/ffmpeg", Tool: "proc_run", Eval: boolPtr(false)},
			},
		},
		Logs: []Log{
			{TimeNS: base.Add(-3 * time.Hour).UnixNano(), Severity: "INFO", Body: "old",
				Attributes: Attributes{User: "alice"}},
			{TimeNS: base.Add(-time.Hour).UnixNano(), Severity: "ERROR", Body: "new",
				Attributes: Attributes{User: "bob"}},
		},
		Metrics: []Metric{
			{TimeNS: base.Add(-3 * time.Hour).UnixNano(), Name: "calls", Value: 1, Unit: "1",
				Attributes: Attributes{User: "alice"}},
			{TimeNS: base.Add(-time.Hour).UnixNano(), Name: "calls", Value: 2, Unit: "1",
				Attributes: Attributes{User: "bob"}},
		},
	}
	if err := s.Insert(context.Background(), export); err != nil {
		t.Fatalf("Insert: %v", err)
	}
}

func TestQueryFilters(t *testing.T) {
	s := open(t)
	seed(t, s)
	ctx := context.Background()

	cases := []struct {
		name   string
		filter Filter
		want   []string // span names, newest first
	}{
		{"everything", Filter{}, []string{"proc_run", "pkg_build", "fs_list"}},
		{"by user", Filter{User: "alice"}, []string{"proc_run", "fs_list"}},
		{"by package", Filter{Package: "/org/ffmpeg"}, []string{"proc_run", "fs_list"}},
		{"by process", Filter{Process: "p1"}, []string{"fs_list"}},
		{"by path prefix", Filter{Path: "/home/bob"}, []string{"pkg_build"}},
		{"by tool", Filter{Tool: "fs_list"}, []string{"fs_list"}},
		{"eval true", Filter{Eval: boolPtr(true)}, []string{"pkg_build"}},
		{"eval false", Filter{Eval: boolPtr(false)}, []string{"proc_run"}},
		{"time range", Filter{Since: base.Add(-150 * time.Minute), Until: base}, []string{"proc_run", "pkg_build"}},
		{"no match", Filter{User: "carol"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := s.Query(ctx, SignalTraces, tc.filter)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if page.Truncated {
				t.Errorf("Truncated = true, want false")
			}
			var got []string
			for _, sp := range page.Spans {
				got = append(got, sp.Name)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("names = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("names = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestQueryRoundTrip(t *testing.T) {
	s := open(t)
	seed(t, s)

	page, err := s.Query(context.Background(), SignalTraces, Filter{Tool: "fs_list"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(page.Spans))
	}
	got := page.Spans[0]
	if got.TraceID != "aa" || got.SpanID != "01" || got.Status != StatusOK {
		t.Errorf("span identity = %+v", got)
	}
	if got.Other["http.status"] != float64(200) {
		t.Errorf("other = %v, want http.status 200", got.Other)
	}
	if got.Eval != nil {
		t.Errorf("Eval = %v, want nil for a record without the attribute", *got.Eval)
	}
	if got.EndNS-got.StartNS != int64(5*time.Millisecond) {
		t.Errorf("duration = %d ns, want %d", got.EndNS-got.StartNS, int64(5*time.Millisecond))
	}
}

func TestQueryLogsAndMetrics(t *testing.T) {
	s := open(t)
	seed(t, s)
	ctx := context.Background()

	logs, err := s.Query(ctx, SignalLogs, Filter{})
	if err != nil {
		t.Fatalf("Query logs: %v", err)
	}
	if len(logs.Logs) != 2 || logs.Logs[0].Body != "new" {
		t.Fatalf("logs = %+v, want newest first", logs.Logs)
	}
	metrics, err := s.Query(ctx, SignalMetrics, Filter{User: "bob"})
	if err != nil {
		t.Fatalf("Query metrics: %v", err)
	}
	if len(metrics.Metrics) != 1 || metrics.Metrics[0].Value != 2 {
		t.Fatalf("metrics = %+v, want the one bob point", metrics.Metrics)
	}
	if _, err := s.Query(ctx, "traffic", Filter{}); err == nil {
		t.Error("Query with an unknown signal returned no error")
	}
}

func TestQueryTruncated(t *testing.T) {
	s := open(t)
	var export Export
	for i := range 5 {
		export.Spans = append(export.Spans, Span{
			TraceID: "aa", SpanID: "0" + string(rune('1'+i)), Name: "fs_list",
			StartNS: base.Add(time.Duration(i) * time.Minute).UnixNano(),
			EndNS:   base.Add(time.Duration(i) * time.Minute).UnixNano(),
			Status:  StatusOK,
		})
	}
	if err := s.Insert(context.Background(), export); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	page, err := s.Query(context.Background(), SignalTraces, Filter{Limit: 2})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(page.Spans))
	}
	if !page.Truncated {
		t.Error("Truncated = false, want true when more records matched")
	}

	all, err := s.Query(context.Background(), SignalTraces, Filter{Limit: 5})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if all.Truncated {
		t.Error("Truncated = true when the page held every match")
	}
}

func TestRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kitbashd.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	got, err := s.Retention(ctx)
	if err != nil {
		t.Fatalf("Retention: %v", err)
	}
	if got != DefaultRetention() {
		t.Errorf("Retention = %+v, want the defaults %+v", got, DefaultRetention())
	}

	value := "48h"
	got, err = s.SetRetention(ctx, RetentionSet{Logs: &value})
	if err != nil {
		t.Fatalf("SetRetention: %v", err)
	}
	if got.Logs != "48h" || got.Traces != "30d" {
		t.Errorf("Retention after set = %+v", got)
	}

	bad := "10m"
	if _, err := s.SetRetention(ctx, RetentionSet{Traces: &bad}); err == nil {
		t.Error("SetRetention accepted 10m")
	}
	after, err := s.Retention(ctx)
	if err != nil {
		t.Fatalf("Retention: %v", err)
	}
	if after.Traces != "30d" {
		t.Errorf("a refused set changed traces to %q", after.Traces)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The window survives a restart, see spec/kitbashd-api.yaml.
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, err = reopened.Retention(ctx)
	if err != nil {
		t.Fatalf("Retention: %v", err)
	}
	if got.Logs != "48h" {
		t.Errorf("Logs after restart = %q, want 48h", got.Logs)
	}
}

func TestSweep(t *testing.T) {
	s := open(t)
	seed(t, s)
	ctx := context.Background()

	traces, logs := "3h", "2h"
	if _, err := s.SetRetention(ctx, RetentionSet{Traces: &traces, Logs: &logs}); err != nil {
		t.Fatalf("SetRetention: %v", err)
	}

	// Two hours and a half after the newest record: the traces window keeps
	// everything from the last three hours, the logs window only the newest.
	now := base.Add(30 * time.Minute)
	counts, err := s.Sweep(ctx, now)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if counts.Traces != 1 {
		t.Errorf("swept traces = %d, want 1", counts.Traces)
	}
	if counts.Logs != 1 {
		t.Errorf("swept logs = %d, want 1", counts.Logs)
	}
	if counts.Metrics != 0 {
		t.Errorf("swept metrics = %d, want 0 under the 30d default", counts.Metrics)
	}
	if counts.Total() != 2 {
		t.Errorf("Total = %d, want 2", counts.Total())
	}

	page, err := s.Query(ctx, SignalTraces, Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Spans) != 2 {
		t.Fatalf("spans left = %d, want 2", len(page.Spans))
	}
	remaining, err := s.Query(ctx, SignalMetrics, Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(remaining.Metrics) != 2 {
		t.Errorf("metrics left = %d, want both", len(remaining.Metrics))
	}

	// A second sweep at the same instant deletes nothing more.
	again, err := s.Sweep(ctx, now)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if again.Total() != 0 {
		t.Errorf("second sweep deleted %d records", again.Total())
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"720h", 720 * time.Hour, true},
		{"30d", 30 * 24 * time.Hour, true},
		{"1h", time.Hour, true},
		{"10m", 0, false},
		{"d", 0, false},
		{"", 0, false},
		{"-1d", 0, false},
		{"1.5d", 0, false},
		{"30D", 0, false},
		// A window of zero would delete everything on the next sweep.
		{"0h", 0, false},
		{"0d", 0, false},
		// These overflow time.Duration. Wrapping would turn centuries into
		// minutes and the next sweep would delete what was meant to be kept.
		{"213504d", 0, false},
		{"5124096h", 0, false},
		{"99999999999999999999d", 0, false},
	}
	for _, tc := range cases {
		got, err := ParseDuration(tc.in)
		if tc.ok && err != nil {
			t.Errorf("ParseDuration(%q) = %v", tc.in, err)
			continue
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("ParseDuration(%q) accepted an invalid window", tc.in)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestInsertEmpty(t *testing.T) {
	s := open(t)
	if err := s.Insert(context.Background(), Export{}); err != nil {
		t.Fatalf("Insert of an empty export: %v", err)
	}
}

func TestInsertRefusesNonFiniteMetric(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		err := s.Insert(ctx, Export{Metrics: []Metric{{TimeNS: base.UnixNano(), Name: "calls", Value: value}}})
		if err == nil {
			t.Errorf("Insert stored the value %v, which JSON cannot represent", value)
		}
	}
	page, err := s.Query(ctx, SignalMetrics, Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Metrics) != 0 {
		t.Errorf("metrics = %d, want none stored", len(page.Metrics))
	}
}

func TestSweepBatches(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	// More rows than one delete statement removes, so the loop has to run
	// more than once.
	var export Export
	old := base.Add(-72 * time.Hour).UnixNano()
	for i := range SweepBatch + 10 {
		export.Logs = append(export.Logs, Log{TimeNS: old + int64(i), Severity: "INFO", Body: "old"})
	}
	export.Logs = append(export.Logs, Log{TimeNS: base.UnixNano(), Severity: "INFO", Body: "new"})
	if err := s.Insert(ctx, export); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	window := "24h"
	if _, err := s.SetRetention(ctx, RetentionSet{Logs: &window}); err != nil {
		t.Fatalf("SetRetention: %v", err)
	}
	counts, err := s.Sweep(ctx, base)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if counts.Logs != int64(SweepBatch+10) {
		t.Errorf("swept %d logs, want %d across batches", counts.Logs, SweepBatch+10)
	}
	page, err := s.Query(ctx, SignalLogs, Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Logs) != 1 || page.Logs[0].Body != "new" {
		t.Errorf("logs left = %d, want only the recent one", len(page.Logs))
	}
}

func TestStoreFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kitbashd.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if err := s.Insert(context.Background(), Export{Logs: []Log{{TimeNS: base.UnixNano(), Body: "hello"}}}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// The journal and shared memory files carry the same records, so they are
	// checked too.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("stat %s: %v", path+suffix, err)
		}
		if mode := info.Mode().Perm(); mode != FileMode {
			t.Errorf("%s has mode %04o, want %04o", path+suffix, mode, FileMode)
		}
	}
}
