package server

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/mounts"
	"github.com/zyx1121/kitbash/internal/proc"
)

// schemaURL is where a schema under test is registered with the compiler. It
// is never fetched: the compiler this repository builds loads nothing, see
// manifest.NewCompiler.
const schemaURL = "https://kitbash.zyx.tw/test/schema.json"

// compileSchema compiles one published schema with the validator the surface
// validates Package tool input with, so a schema is held to what a client's
// validator would say about it rather than to a reading of it.
func compileSchema(t *testing.T, raw json.RawMessage) *jsonschema.Schema {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the schema is not JSON: %v", err)
	}
	c := manifest.NewCompiler()
	if err := c.AddResource(schemaURL, doc); err != nil {
		t.Fatalf("adding the schema: %v", err)
	}
	compiled, err := c.Compile(schemaURL)
	if err != nil {
		t.Fatalf("compiling the schema: %v", err)
	}
	return compiled
}

// validate runs one answer through a compiled schema the way a client does.
func validate(t *testing.T, schema *jsonschema.Schema, answer any) error {
	t.Helper()
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encoding the answer: %v", err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	return schema.Validate(value)
}

// aProcess is a Process carrying everything proc_list can say about one.
func aProcess() proc.Process {
	healthy := proc.Health{Last: "2026-09-19T00:00:00Z", Healthy: true}
	return proc.Process{
		ID:        "0199a000-0000-7000-8000-000000000001",
		Name:      "app",
		Package:   "/home/tester/app",
		Digest:    "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		State:     proc.StateRunning,
		Expose:    "http",
		Endpoint:  "http://127.0.0.1:8080",
		URL:       "https://app.tester.example.org",
		StartedAt: "2026-09-19T00:00:00Z",
		Runner:    "/org/runner",
		Problem:   "the mount source changed between validation and start",
		Fix:       "Run the Package again once the folder is back.",
		Health:    &healthy,
		Mounts: []mounts.Resolved{
			{Source: "/home/tester/app-data", Target: "/data", Mode: mounts.ModeRW},
		},
	}
}

// aJob is one scheduled Process as proc_run and proc_list answer about it:
// waiting between its runs, with the expression kitbashd holds, the tick it
// will make and the run it finished. It is a second shape the one published
// schema has to take, see PLAN.md section 2.3.
func aJob() proc.Process {
	return proc.Process{
		ID:       "0199a000-0000-7000-8000-000000000002",
		Name:     "weather",
		Package:  "/home/tester/weather",
		Digest:   "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		State:    proc.StateScheduled,
		Expose:   "none",
		Schedule: "0 8 * * *",
		NextRun:  "2026-09-20T08:00:00Z",
		LastRun: &proc.LastRun{
			StartedAt:  "2026-09-19T08:00:00Z",
			ExitCode:   0,
			DurationMs: 1200,
		},
	}
}

// TestProcListOutputValidatesBothShapes is the guard on proc_list answering
// two shapes through one schema: a Process in full and a line have to validate
// against the published output, and a client's validator is what says so.
func TestProcListOutputValidatesBothShapes(t *testing.T) {
	schema := compileSchema(t, procListOutputSchema)
	full := &proc.ListResult{Processes: []proc.Process{aProcess()}}

	if err := validate(t, schema, full); err != nil {
		t.Errorf("a Process in full does not validate against proc_list's output:\n%v", err)
	}
	if err := validate(t, schema, full.Lines()); err != nil {
		t.Errorf("a line does not validate against proc_list's output:\n%v", err)
	}
	if err := validate(t, schema, full.OfPackage("/home/tester/app")); err != nil {
		t.Errorf("a listing of one Package does not validate against proc_list's output:\n%v", err)
	}
	// And the other shape the same schema answers: a job, waiting, with the
	// tick it will make and the run it finished.
	jobs := &proc.ListResult{Processes: []proc.Process{aJob()}}
	if err := validate(t, schema, jobs); err != nil {
		t.Errorf("a scheduled Process does not validate against proc_list's output:\n%v", err)
	}
	if err := validate(t, schema, jobs.Lines()); err != nil {
		t.Errorf("a line of a scheduled Process does not validate against proc_list's output:\n%v", err)
	}

	// And the schema is doing work: an answer that is not a Process is
	// refused, so the two above passing means something.
	refused := map[string]any{"processes": []any{map[string]any{
		"id": "0199a000-0000-7000-8000-000000000001", "name": "app",
		"package": "/home/tester/app", "state": "wobbly",
	}}}
	if err := validate(t, schema, refused); err == nil {
		t.Error("a Process in a state the surface has no name for validated")
	}
	missing := map[string]any{"processes": []any{map[string]any{"id": "x"}}}
	if err := validate(t, schema, missing); err == nil {
		t.Error("a Process with no name, package or state validated")
	}
}

// A line is one call's worth of answer, so a job's line carries the two fields
// that say it is a job: without them an agent that has just registered one
// reads scheduled and has to ask again by Package to learn what for, which is
// the call issue #154 is about. They are absent from the line of a Process
// that stays up, the way url is.
func TestALineOfAJobCarriesItsScheduleAndNextRun(t *testing.T) {
	lines := (&proc.ListResult{Processes: []proc.Process{aJob(), aProcess()}}).Lines()
	encoded, err := json.Marshal(lines)
	if err != nil {
		t.Fatalf("encoding the lines: %v", err)
	}
	var decoded struct {
		Processes []map[string]any `json:"processes"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decoding the lines: %v", err)
	}
	if len(decoded.Processes) != 2 {
		t.Fatalf("the listing has %d lines, want two", len(decoded.Processes))
	}
	job, running := decoded.Processes[0], decoded.Processes[1]
	if job["schedule"] != aJob().Schedule {
		t.Errorf("the line of a job carries schedule %v, want %q", job["schedule"], aJob().Schedule)
	}
	if job["nextRun"] != aJob().NextRun {
		t.Errorf("the line of a job carries nextRun %v, want %q", job["nextRun"], aJob().NextRun)
	}
	for _, field := range []string{"schedule", "nextRun"} {
		if _, carried := running[field]; carried {
			t.Errorf("the line of a Process that stays up carries %s: %v", field, running[field])
		}
	}
	// The line stays a line: the fields a member asks about one Package for
	// are still only in the full shape.
	for _, field := range []string{"digest", "lastRun", "mounts", "health"} {
		if _, carried := job[field]; carried {
			t.Errorf("a line carries %s, which is the answer to a narrower question", field)
		}
	}
}

// proc_run answers the same two shapes, so its own published schema is held to
// both: a Process that is up and a job that is waiting.
func TestProcRunOutputValidatesAProcessAndAJob(t *testing.T) {
	schema := compileSchema(t, procRunOutputSchema)
	for name, answer := range map[string]proc.Process{"a Process": aProcess(), "a job": aJob()} {
		if err := validate(t, schema, answer); err != nil {
			t.Errorf("%s does not validate against proc_run's output:\n%v", name, err)
		}
	}
	// The schema is doing work: a state the surface has no name for is
	// refused, so the two above passing means something.
	refused := map[string]any{
		"id": "0199a000-0000-7000-8000-000000000001", "name": "app",
		"package": "/home/tester/app",
		"digest":  "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"state":   "waiting",
	}
	if err := validate(t, schema, refused); err == nil {
		t.Error("a Process in a state the surface has no name for validated")
	}
}

// The other answers a client parses, held to their own published schemas the
// same way: what the tool returns is what the schema says it returns.
func TestPublishedOutputsValidateWhatTheToolsAnswer(t *testing.T) {
	build := compileSchema(t, pkgBuildOutputSchema)
	if err := validate(t, build, map[string]any{
		"path":   "/home/tester/app",
		"digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"commit": "1111111111111111111111111111111111111111",
		"log":    "STEP 1: FROM node:22-alpine\n",
	}); err != nil {
		t.Errorf("a build does not validate against pkg_build's output:\n%v", err)
	}

	packages := compileSchema(t, pkgListOutputSchema)
	if err := validate(t, packages, map[string]any{"packages": []any{
		map[string]any{"path": "/home/tester/app", "name": "app"},
		map[string]any{"path": "/org/handbook", "name": "handbook",
			"digest": "sha256:2222222222222222222222222222222222222222222222222222222222222222"},
	}}); err != nil {
		t.Errorf("a Package listing does not validate against pkg_list's output:\n%v", err)
	}
}
