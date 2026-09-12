// Command genfixture writes the store fixture of one released tag. It is
// built against that tag's internal/store, not against the current one, so it
// lives here only as the record of how the fixture was produced. See
// internal/store/testdata/README.md for the commands that run it.
//
// This is the v0.6.0 era: signals carry producer and caller but no internal,
// processes carry a container and a digest but no fan out secret and no permits,
// and approvals exist in all three states.
package main

import (
	"context"
	"encoding/json"
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
					Producer: "p-alpha", Caller: "p-beta"},
			},
			{
				TraceID: "33333333333333333333333333333333", SpanID: "3333333333333333",
				Name:    "proc_run",
				StartNS: base.Add(-time.Hour).UnixNano(),
				EndNS:   base.Add(-time.Hour + 2*time.Second).UnixNano(),
				Attributes: store.Attributes{User: "loki", Package: "/org/handbook", Tool: "proc_run",
					Eval: &notEval, Producer: "loki"},
			},
		},
		Logs: []store.Log{
			{TimeNS: base.Add(-3 * time.Hour).UnixNano(), Severity: "INFO", Body: "the sweep removed nothing",
				Attributes: store.Attributes{User: "loki", Package: "/org/handbook", Tool: "fs_read"}},
			{TimeNS: base.Add(-2 * time.Hour).UnixNano(), Severity: "ERROR", Body: "the build failed",
				TraceID: "22222222222222222222222222222222", SpanID: "2222222222222222",
				Attributes: store.Attributes{User: "kilo", Package: "/home/kilo/tool", Tool: "pkg_build",
					Eval: &eval, Producer: "p-alpha", Caller: "p-beta"}},
			{TimeNS: base.Add(-time.Hour).UnixNano(), Severity: "WARN", Body: "the token was refused",
				Attributes: store.Attributes{User: "loki", Tool: "proc_run", Producer: "loki"}},
		},
		Metrics: []store.Metric{
			{TimeNS: base.Add(-3 * time.Hour).UnixNano(), Name: "kitbash.calls", Value: 1, Unit: "1",
				Attributes: store.Attributes{User: "loki", Package: "/org/handbook", Tool: "fs_read"}},
			{TimeNS: base.Add(-2 * time.Hour).UnixNano(), Name: "kitbash.calls", Value: 2, Unit: "1",
				Attributes: store.Attributes{User: "kilo", Package: "/home/kilo/tool", Tool: "pkg_build",
					Eval: &eval, Producer: "p-alpha", Caller: "p-beta"}},
			{TimeNS: base.Add(-time.Hour).UnixNano(), Name: "kitbash.bytes", Value: 4096, Unit: "By",
				Attributes: store.Attributes{User: "loki", Producer: "loki"}},
		},
	}); err != nil {
		return err
	}

	traces, logs, metrics := "720h", "168h", "90d"
	if _, err := s.SetRetention(ctx, store.RetentionSet{Traces: &traces, Logs: &logs, Metrics: &metrics}); err != nil {
		return err
	}

	// The token hashes are of fixed tokens rather than minted ones, so the
	// fixture is the same file every time it is built and a test can look a
	// Process up by the token the README names.
	if err := s.RegisterProcess(ctx, store.Process{
		ID: "01930000-0000-7000-8000-0000000000a1", Owner: "loki", Admin: true,
		Package: "/org/handbook", Name: "handbook",
		Container: "kitbash-handbook", Digest: "sha256:aaaa",
		Expose: "mcp", Endpoint: "http://127.0.0.1:8081",
		Subscriptions: []string{store.SubscriptionTelemetry},
		RegisteredAt:  base.Add(-24 * time.Hour),
	}, store.HashToken("fixture-token-alpha"), 0); err != nil {
		return err
	}
	if err := s.RegisterProcess(ctx, store.Process{
		ID: "01930000-0000-7000-8000-0000000000a2", Owner: "kilo",
		Package: "/home/kilo/tool", Name: "tool",
		Container: "kitbash-tool", Digest: "sha256:bbbb",
		Expose: "none", Subscriptions: []string{},
		RegisteredAt: base.Add(-12 * time.Hour),
	}, store.HashToken("fixture-token-beta"), 0); err != nil {
		return err
	}

	// One approval in each state, so the migrated store is read with a queue
	// that is not all pending.
	if err := s.CreateApproval(ctx, store.Approval{
		ID: "01930000-0000-7000-8000-0000000000b1", Requester: "loki", Tool: "proc_run",
		Input:       json.RawMessage(`{"package":"/org/handbook"}`),
		RequestedAt: base.Add(-6 * time.Hour),
	}, 0); err != nil {
		return err
	}
	if err := s.CreateApproval(ctx, store.Approval{
		ID: "01930000-0000-7000-8000-0000000000b2", Requester: "kilo", Tool: "pkg_build",
		Input:       json.RawMessage(`{"package":"/home/kilo/tool"}`),
		RequestedAt: base.Add(-5 * time.Hour),
	}, 0); err != nil {
		return err
	}
	if _, _, err := s.ClaimApproval(ctx, "01930000-0000-7000-8000-0000000000b2", "loki", "looks fine",
		base.Add(-4*time.Hour)); err != nil {
		return err
	}
	if _, err := s.ResultApproval(ctx, "01930000-0000-7000-8000-0000000000b2", "loki",
		json.RawMessage(`{"ok":true}`)); err != nil {
		return err
	}
	if err := s.CreateApproval(ctx, store.Approval{
		ID: "01930000-0000-7000-8000-0000000000b3", Requester: "kilo", Tool: "proc_stop",
		Input:       json.RawMessage(`{"process":"01930000-0000-7000-8000-0000000000a2"}`),
		RequestedAt: base.Add(-3 * time.Hour),
	}, 0); err != nil {
		return err
	}
	if _, _, err := s.RejectApproval(ctx, "01930000-0000-7000-8000-0000000000b3", "loki", "not this one",
		base.Add(-2*time.Hour)); err != nil {
		return err
	}

	// Close checkpoints the write ahead log, so the fixture is one file with
	// no sidecar carrying records a reader of it would miss.
	return s.Close()
}
