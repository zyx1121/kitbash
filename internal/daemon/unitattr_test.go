package daemon

import (
	"net/http"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/otlp"
)

// kitbash.unit is the one attribute a container of a pod adds to what it
// exports, and it is held to the units of the Process the token names: a
// producer naming a unit of somebody else's Process, or one of nobody's, would
// put the answer to a query in its own hands, see PLAN.md section 2.4.
//
// The attribute is dropped and the record is kept, which is the rule
// kitbash.caller follows: what a producer got wrong is one attribute, and
// losing the record would lose what it was about.
func TestAProcessKeepsTheUnitItHasAndLosesTheOneItDoesNot(t *testing.T) {
	h := serve(t, false)
	base := h.serveTCP()

	req := registration("")
	req.Container = "kitbash-board-board-web"
	req.Digest = testDigest
	req.Pod = "kitbash-board-board"
	req.Units = []unitRequest{
		{Name: "cache", Container: "kitbash-board-board-cache", Digest: cacheDigest},
		{Name: "web", Container: "kitbash-board-board-web", Digest: testDigest, Face: true},
	}
	token, res, body := h.register(req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("register = %d %s, want 200", res.StatusCode, body)
	}

	now := time.Now()
	for _, sent := range []struct {
		name string
		unit string
		want string
	}{
		{name: "known", unit: "cache", want: "cache"},
		{name: "unknown", unit: "postgres", want: ""},
		{name: "none", unit: "", want: ""},
	} {
		claims := map[string]any{}
		if sent.unit != "" {
			claims[otlp.AttrUnit] = sent.unit
		}
		export := processExport(sent.name, claims, false, now)
		if res, body := h.exportTCP(base, pathTraces, token, export); res.StatusCode != http.StatusOK {
			t.Fatalf("export %s = %d %s, want 200", sent.name, res.StatusCode, body)
		}
	}

	held := map[string]string{}
	for _, span := range h.spans() {
		held[span.Name] = span.Unit
	}
	if len(held) != 3 {
		t.Fatalf("the store holds %d spans, want all three: a record is never dropped for an attribute", len(held))
	}
	for name, want := range map[string]string{"known": "cache", "unknown": "", "none": ""} {
		if held[name] != want {
			t.Errorf("the %s record carries the unit %q, want %q", name, held[name], want)
		}
	}
}

// A Process of one unit has no units at all, so every kitbash.unit a producer
// of one sends names nothing and is dropped.
func TestASingleUnitProcessCarriesNoUnitOnItsRecords(t *testing.T) {
	h := serve(t, false)
	base := h.serveTCP()

	token, res, body := h.register(registration(""))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("register = %d %s, want 200", res.StatusCode, body)
	}
	export := processExport("claimed", map[string]any{otlp.AttrUnit: "web"}, false, time.Now())
	if res, body := h.exportTCP(base, pathTraces, token, export); res.StatusCode != http.StatusOK {
		t.Fatalf("export = %d %s, want 200", res.StatusCode, body)
	}
	spans := h.spans()
	if len(spans) != 1 {
		t.Fatalf("the store holds %d spans, want the one that was sent", len(spans))
	}
	if spans[0].Unit != "" {
		t.Errorf("the record carries the unit %q, want none", spans[0].Unit)
	}
}
