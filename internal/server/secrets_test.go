package server_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
)

// The value these tests set. Every assertion that it did not travel looks for
// this one string, so a leak into a span, a log line or a listing is named
// rather than guessed at.
const secretValue = "sk-the-value-that-must-not-travel"

// The three tools of the secrets family are the member's whole handling of a
// credential: set it, see the name, drop it. Each is forwarded to kitbashd and
// answered with the daemon's own body, see PLAN.md section 2.3.
func TestSecretsFamilyIsServedThroughTheDaemon(t *testing.T) {
	tr := newTracedWithDaemon(t)

	res := call(t, tr.session, "secrets_set", map[string]any{
		"name": "ANTHROPIC_API_KEY", "value": secretValue,
	})
	ok(t, res, "secrets_set")
	var set struct {
		Name    string `json:"name"`
		Updated string `json:"updated"`
	}
	decodeResult(t, res, &set)
	if set.Name != "ANTHROPIC_API_KEY" || set.Updated == "" {
		t.Errorf("secrets_set answered %+v, want the name and a timestamp", set)
	}
	held := tr.daemon.Secrets()
	if len(held) != 1 || held[0].Name != "ANTHROPIC_API_KEY" || held[0].Value != secretValue {
		t.Fatalf("the daemon holds %+v, want the one value the agent set", held)
	}

	res = call(t, tr.session, "secrets_list", map[string]any{})
	ok(t, res, "secrets_list")
	var list struct {
		Secrets []struct {
			Name    string `json:"name"`
			Updated string `json:"updated"`
		} `json:"secrets"`
	}
	decodeResult(t, res, &list)
	if len(list.Secrets) != 1 || list.Secrets[0].Name != "ANTHROPIC_API_KEY" {
		t.Fatalf("secrets_list answered %+v, want the one name", list.Secrets)
	}
	if answer := textOf(t, res); strings.Contains(answer, secretValue) {
		t.Errorf("secrets_list answered %s, want the name and no value", answer)
	}

	res = call(t, tr.session, "secrets_remove", map[string]any{"name": "ANTHROPIC_API_KEY"})
	ok(t, res, "secrets_remove")
	var removed struct {
		Name    string `json:"name"`
		Removed bool   `json:"removed"`
	}
	decodeResult(t, res, &removed)
	if !removed.Removed {
		t.Errorf("secrets_remove answered %+v, want it removed", removed)
	}
	if len(tr.daemon.Secrets()) != 0 {
		t.Errorf("the daemon still holds %+v after the removal", tr.daemon.Secrets())
	}
}

// The span of secrets_set carries the tool name and nothing of the value. It
// is the one call on this surface whose argument must never be recorded, and
// tracing.go reads a path and never an argument by name, see PLAN.md 2.3.
func TestSecretsSetRecordsNoValueInItsSpan(t *testing.T) {
	tr := newTracedWithDaemon(t)

	ok(t, call(t, tr.session, "secrets_set", map[string]any{
		"name": "ANTHROPIC_API_KEY", "value": secretValue,
	}), "secrets_set")
	tr.flush(t)

	span, only := tr.daemon.Span("secrets_set")
	if !only {
		t.Fatalf("want exactly one secrets_set span, got %+v", tr.daemon.Spans())
	}
	if got := span.Attributes[telemetry.AttrTool]; got != "secrets_set" {
		t.Errorf("%s is %q, want secrets_set", telemetry.AttrTool, got)
	}
	if span.Status != "ok" {
		t.Errorf("status is %q, want ok", span.Status)
	}
	for key, value := range span.Attributes {
		if strings.Contains(value, secretValue) {
			t.Errorf("the span carries the value in %s = %q", key, value)
		}
	}
	// Not in the span's own fields either: a name, a status message or a path
	// built out of the arguments would carry it just as far.
	encoded, err := json.Marshal(span)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), secretValue) {
		t.Errorf("the span is %s, want no value anywhere in it", encoded)
	}
	// And not in the session's own log, which is where an internal cause goes.
	if strings.Contains(tr.logs.String(), secretValue) {
		t.Errorf("the session log carries the value: %s", tr.logs.String())
	}
}

// A refusal quotes no value, wherever it was decided. The schema is checked by
// the SDK before the handler runs and it reports a violation by printing what
// it refused, so the one tool whose argument is a secret is not quoted back,
// see neverRepeated in middleware.go.
func TestSecretsSetRefusalCarriesNoValue(t *testing.T) {
	tr := newTracedWithDaemon(t)

	for _, value := range []string{
		strings.Repeat("x", 8193),
		secretValue + "\nsecond line",
		"",
	} {
		res := call(t, tr.session, "secrets_set", map[string]any{
			"name": "ANTHROPIC_API_KEY", "value": value,
		})
		prob := problemOf(t, res)
		if prob.Slug() != problem.SlugBadRequest {
			t.Fatalf("problem is %s, want bad-request", prob.Slug())
		}
		if value != "" && strings.Contains(prob.Detail+prob.Fix, value) {
			t.Errorf("the refusal is %+v, want one that does not quote the value", prob)
		}
		if strings.Contains(prob.Detail+prob.Fix, "xxxxxxxxxx") {
			t.Errorf("the refusal is %+v, want one that does not quote the value", prob)
		}
	}
	if len(tr.daemon.Secrets()) != 0 {
		t.Errorf("a refused value was stored: %+v", tr.daemon.Secrets())
	}
}

// Without kitbashd there is nowhere for a value to live, so the three tools
// answer internal with the fix that names the daemon, the way the users family
// does, see PLAN.md section 4.6.
func TestSecretsFamilyNeedsTheDaemon(t *testing.T) {
	tr := newTraced(t, filepath.Join(t.TempDir(), "absent.sock"))

	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"secrets_set", map[string]any{"name": "ANTHROPIC_API_KEY", "value": secretValue}},
		{"secrets_list", map[string]any{}},
		{"secrets_remove", map[string]any{"name": "ANTHROPIC_API_KEY"}},
	} {
		t.Run(c.tool, func(t *testing.T) {
			prob := problemOf(t, call(t, tr.session, c.tool, c.args))
			if prob.Slug() != problem.SlugInternal {
				t.Fatalf("problem is %s, want internal", prob.Slug())
			}
			if !strings.Contains(prob.Fix, "kitbashd") {
				t.Errorf("fix is %q, want the one that names the daemon", prob.Fix)
			}
			if strings.Contains(prob.Detail+prob.Fix, secretValue) {
				t.Errorf("the refusal is %+v, want no value in it", prob)
			}
		})
	}
}

// The three tools are on the surface a member is served, beside the families
// that were there before.
func TestSecretsToolsAreOnTheSurface(t *testing.T) {
	tr := newTracedWithDaemon(t)
	list, err := tr.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range list.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"secrets_set", "secrets_list", "secrets_remove"} {
		if !names[want] {
			t.Errorf("%s is not on the surface", want)
		}
	}
}

// decodeResult reads the answer kitbashd composed, which the surface passes
// through as structured content and as the text block beside it.
func decodeResult(t *testing.T, res *mcp.CallToolResult, out any) {
	t.Helper()
	if err := json.Unmarshal([]byte(textOf(t, res)), out); err != nil {
		t.Fatalf("decoding %s: %v", textOf(t, res), err)
	}
}
