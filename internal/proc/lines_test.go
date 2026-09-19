package proc_test

import (
	"encoding/json"
	"testing"

	"github.com/zyx1121/kitbash/internal/proc"
)

// listing is two Processes of two Packages, one of them exposed over http.
func listing() *proc.ListResult {
	return &proc.ListResult{Processes: []proc.Process{
		{
			ID: "0199a000-0000-7000-8000-000000000001", Name: "app", Package: "/home/tester/app",
			Digest: "sha256:" + "a1", State: proc.StateRunning, Expose: "http",
			URL: "https://app.tester.example.org", StartedAt: "2026-09-19T00:00:00Z",
		},
		{
			ID: "0199a000-0000-7000-8000-000000000002", Name: "ffmpeg", Package: "/home/tester/ffmpeg",
			Digest: "sha256:" + "b2", State: proc.StateStopped, Expose: "mcp",
			Tools: []string{"ffmpeg_transcode"},
		},
	}}
}

// A listing nobody narrowed is one line per Process: what it takes to name one
// and ask about it, see PLAN.md section 4.5.
func TestLinesCarryWhatNamingAProcessTakes(t *testing.T) {
	lines := listing().Lines()
	if len(lines.Processes) != 2 {
		t.Fatalf("Lines returned %+v, want both Processes", lines.Processes)
	}
	body, err := json.Marshal(lines.Processes[0])
	if err != nil {
		t.Fatalf("encoding a line: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decoding a line: %v", err)
	}
	for _, want := range []string{"id", "name", "package", "state", "expose", "url"} {
		if _, held := fields[want]; !held {
			t.Errorf("a line carries no %s: %s", want, body)
		}
	}
	if len(fields) != 6 {
		t.Errorf("a line carries %v, want those six fields and nothing else", fields)
	}
	// A Process the proxy serves no address for carries none.
	second, err := json.Marshal(lines.Processes[1])
	if err != nil {
		t.Fatalf("encoding a line: %v", err)
	}
	if string(second) == "" || len(second) == 0 {
		t.Fatal("the second line is empty")
	}
	var absent map[string]any
	if err := json.Unmarshal(second, &absent); err != nil {
		t.Fatalf("decoding a line: %v", err)
	}
	if _, held := absent["url"]; held {
		t.Errorf("a Process with no address carries a url: %s", second)
	}
}

// Asking about one Package answers that Package's Processes in full.
func TestOfPackageKeepsTheDetailAndNarrowsTheSet(t *testing.T) {
	out := listing().OfPackage("/home/tester/app")
	if len(out.Processes) != 1 {
		t.Fatalf("OfPackage returned %+v, want the one Process of that Package", out.Processes)
	}
	if out.Processes[0].Digest == "" || out.Processes[0].StartedAt == "" {
		t.Errorf("the Process is %+v, want it in full", out.Processes[0])
	}
	if got := listing().OfPackage("/home/tester/nothing"); len(got.Processes) != 0 {
		t.Errorf("OfPackage of a path with no Process is %+v, want none", got.Processes)
	}
}
