// Command genfixture writes the store fixture of one released tag. It is
// built against that tag's internal/store, not against the current one, so it
// lives here only as the record of how the fixture was produced. See
// internal/store/testdata/README.md for the commands that run it.
//
// This is the v0.3.0 era: the store is Telemetry and retention only. Signals
// carry no producer, no caller and no internal, and there is neither a
// processes table nor an approvals one.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/zyx1121/kitbash/internal/store"
)

// base is the instant every record is placed around, so a fixture built today
// and one rebuilt next year hold the same timestamps.
var base = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: genfixture <path>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(path string) error {
	s, err := store.Open(path)
	if err != nil {
		return err
	}
	ctx := context.Background()

	eval, notEval := true, false
	if err := s.Insert(ctx, store.Export{
		Spans: []store.Span{
			{
				TraceID: "11111111111111111111111111111111", SpanID: "1111111111111111",
				Name:    "fs_read",
				StartNS: base.Add(-3 * time.Hour).UnixNano(),
				EndNS:   base.Add(-3*time.Hour + 5*time.Millisecond).UnixNano(),
				Status:  store.StatusOK,
				Attributes: store.Attributes{User: "loki", Package: "/org/handbook", Process: "p-alpha",
					Path: "/org/handbook/readme.md", Tool: "fs_read",
					Other: map[string]any{"http.status": float64(200)}},
			},
			{
				TraceID: "22222222222222222222222222222222", SpanID: "2222222222222222",
				Name:          "pkg_build",
				StartNS:       base.Add(-2 * time.Hour).UnixNano(),
				EndNS:         base.Add(-2*time.Hour + time.Second).UnixNano(),
				Status:        store.StatusError,
				StatusMessage: "the build failed",
				Attributes: store.Attributes{User: "kilo", Package: "/home/kilo/tool",
					Path: "/home/kilo/tool", Tool: "pkg_build", Eval: &eval,
				},
			},
			{
				TraceID: "33333333333333333333333333333333", SpanID: "3333333333333333",
				Name:    "proc_run",
				StartNS: base.Add(-time.Hour).UnixNano(),
				EndNS:   base.Add(-time.Hour + 2*time.Second).UnixNano(),
				Attributes: store.Attributes{User: "loki", Package: "/org/handbook", Tool: "proc_run",
					Eval: &notEval},
			},
		},
		Logs: []store.Log{
			{TimeNS: base.Add(-3 * time.Hour).UnixNano(), Severity: "INFO", Body: "the sweep removed nothing",
				Attributes: store.Attributes{User: "loki", Package: "/org/handbook", Tool: "fs_read"}},
			{TimeNS: base.Add(-2 * time.Hour).UnixNano(), Severity: "ERROR", Body: "the build failed",
				TraceID: "22222222222222222222222222222222", SpanID: "2222222222222222",
				Attributes: store.Attributes{User: "kilo", Package: "/home/kilo/tool", Tool: "pkg_build",
					Eval: &eval}},
			{TimeNS: base.Add(-time.Hour).UnixNano(), Severity: "WARN", Body: "the token was refused",
				Attributes: store.Attributes{User: "loki", Tool: "proc_run"}},
		},
		Metrics: []store.Metric{
			{TimeNS: base.Add(-3 * time.Hour).UnixNano(), Name: "kitbash.calls", Value: 1, Unit: "1",
				Attributes: store.Attributes{User: "loki", Package: "/org/handbook", Tool: "fs_read"}},
			{TimeNS: base.Add(-2 * time.Hour).UnixNano(), Name: "kitbash.calls", Value: 2, Unit: "1",
				Attributes: store.Attributes{User: "kilo", Package: "/home/kilo/tool", Tool: "pkg_build",
					Eval: &eval}},
			{TimeNS: base.Add(-time.Hour).UnixNano(), Name: "kitbash.bytes", Value: 4096, Unit: "By",
				Attributes: store.Attributes{User: "loki"}},
		},
	}); err != nil {
		return err
	}

	traces, logs, metrics := "720h", "168h", "90d"
	if _, err := s.SetRetention(ctx, store.RetentionSet{Traces: &traces, Logs: &logs, Metrics: &metrics}); err != nil {
		return err
	}

	// Close checkpoints the write ahead log, so the fixture is one file with
	// no sidecar carrying records a reader of it would miss.
	return s.Close()
}
