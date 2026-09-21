package daemon

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/otlp"
	"github.com/zyx1121/kitbash/internal/store"
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

// The health probe is the one record kitbashd writes about a single unit of a
// composed Process: it requests the face, which is the only unit that
// publishes a port, so the reading is that unit's and the record says so, see
// PLAN.md section 2.4.
func TestTheProbeOfAPodCarriesTheFaceUnit(t *testing.T) {
	h, fake := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)

	req := probeRegistration(stub.server.URL, "/healthz", "")
	req.Container = "kitbash-board-board-web"
	req.Pod = "kitbash-board-board"
	req.Units = []unitRequest{
		{Name: "web", Container: "kitbash-board-board-web", Digest: req.Digest, Face: true},
		{Name: "cache", Container: "kitbash-board-board-cache", Digest: cacheDigest},
	}
	port, ok := endpointPort(stub.server.URL)
	if !ok {
		t.Fatalf("the endpoint %q names no port", stub.server.URL)
	}
	fake.Publish(req.Container, port)
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register = %d %s, want 200", res.StatusCode, body)
	}

	record := h.waitForHealth("the health record of a pod")
	if record.Attributes.Unit != "web" {
		t.Errorf("the probe records the unit %q, want the face unit web", record.Attributes.Unit)
	}
	if record.Attributes.Process != req.ID {
		t.Errorf("the probe records the Process %q, want %q", record.Attributes.Process, req.ID)
	}
}

// A Process of one unit is the Package itself, so its probe names no unit: a
// record that carried one would be about a container the surface has no name
// for.
func TestTheProbeOfASingleUnitProcessCarriesNoUnit(t *testing.T) {
	h, fake := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)
	h.registerProbed(fake, stub.server.URL, "/healthz", "")

	record := h.waitForHealth("the health record of a Process of one unit")
	if record.Attributes.Unit != "" {
		t.Errorf("the probe records the unit %q, want none", record.Attributes.Unit)
	}
}

// tel_query takes unit as a filter, which is what makes the attribute worth
// stamping: "what did the cache do" is one query rather than a page of the
// Process's records read by hand, see PLAN.md section 5.6.
//
// The other half is that a Process of one unit carries none, so a query by
// unit never answers with its records.
func TestAQueryByUnitAnswersThatUnitAndNoOther(t *testing.T) {
	h, fake := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)

	// A Process that runs as a pod. Its probe requests the face, so the
	// records it writes carry the unit web.
	pod := probeRegistration(stub.server.URL, "/healthz", "")
	pod.Container = "kitbash-board-board-web"
	pod.Pod = "kitbash-board-board"
	pod.Units = []unitRequest{
		{Name: "web", Container: "kitbash-board-board-web", Digest: pod.Digest, Face: true},
		{Name: "cache", Container: "kitbash-board-board-cache", Digest: cacheDigest},
	}
	port, ok := endpointPort(stub.server.URL)
	if !ok {
		t.Fatalf("the endpoint %q names no port", stub.server.URL)
	}
	fake.Publish(pod.Container, port)
	if _, res, body := h.register(pod); res.StatusCode != http.StatusOK {
		t.Fatalf("register = %d %s, want 200", res.StatusCode, body)
	}

	// A Process of one unit beside it, probed the same way, whose records
	// carry no unit at all.
	alone := h.registerProbed(fake, stub.server.URL, "/healthz", "")

	// Both have to have been probed before the query means anything: a filter
	// that answers nothing of the second Process because the second Process
	// has written nothing yet is not the assertion this makes.
	waitFor(t, "a health record of each Process", func() bool {
		var composed, single bool
		for _, record := range h.healthRecords() {
			switch record.Attributes.Process {
			case pod.ID:
				composed = true
			case alone.ID:
				single = true
			}
		}
		return composed && single
	})

	res, body := h.postJSON(http.MethodPost, "/kitbash/v1/query",
		queryRequest{Signal: store.SignalMetrics, Unit: "web"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("query status = %d, body %s", res.StatusCode, body)
	}
	var got struct {
		Records []metricRecord `json:"records"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(got.Records) == 0 {
		t.Fatal("a query by unit answered nothing, want the probe records of the face")
	}
	for _, record := range got.Records {
		if record.Attributes.Unit != "web" {
			t.Errorf("a record of the unit %q came back for a query of web: %+v", record.Attributes.Unit, record)
		}
		if record.Attributes.Process != pod.ID {
			t.Errorf("a record of the Process %s came back, want the pod %s", record.Attributes.Process, pod.ID)
		}
	}

	// A unit nothing is called answers the empty page rather than everything.
	res, body = h.postJSON(http.MethodPost, "/kitbash/v1/query",
		queryRequest{Signal: store.SignalMetrics, Unit: "cache"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("query status = %d, body %s", res.StatusCode, body)
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(got.Records) != 0 {
		t.Errorf("a query for the unit nothing wrote about answered %d records", len(got.Records))
	}
}
