package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/user"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/store"
)

// testHealthInterval is the floor the probe tests run with. A test cannot wait
// out MinHealthInterval to see a second probe, so the daemon is given a floor
// of its own, which is what Options.HealthMinInterval exists for.
const testHealthInterval = 20 * time.Millisecond

// probeStub is one Process answering the path kitbashd requests: the status it
// answers with, and how many requests it received.
type probeStub struct {
	server   *httptest.Server
	requests atomic.Int64
	status   atomic.Int64
	paths    chan string
}

// newProbeStub serves a health path on the loopback address, which is the only
// place kitbashd probes.
func newProbeStub(t *testing.T, status int) *probeStub {
	t.Helper()
	stub := &probeStub{paths: make(chan string, 64)}
	stub.status.Store(int64(status))
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.requests.Add(1)
		select {
		case stub.paths <- r.URL.Path:
		default:
		}
		w.WriteHeader(int(stub.status.Load()))
		w.Write([]byte("ok"))
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

// serveProbing starts a daemon whose probe floor is short enough for a test,
// and runs its probe loop until the test is over.
func serveProbing(t *testing.T) *harness {
	t.Helper()
	h := serveWith(t, Options{
		Admin:             func(*user.User) (bool, error) { return false, nil },
		HealthMinInterval: testHealthInterval,
	})
	h.probeLoop()
	return h
}

// probeLoop runs the daemon's health loop for the life of the test, which is
// what kitbashd does after restore.
func (h *harness) probeLoop() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.server.HealthLoop(ctx)
	}()
	h.t.Cleanup(func() {
		cancel()
		if _, returned := recvWithin(done); !returned {
			h.t.Errorf("waited %s for HealthLoop to return after the context was cancelled", waitBudget)
		}
	})
}

// probeRegistration is one registration of a Process that declares a health
// path on the endpoint given.
func probeRegistration(endpoint, path, interval string) processRequest {
	req := registration("")
	req.Expose = ExposeHTTP
	req.Endpoint = endpoint
	req.Health = &healthRequest{HTTP: path, Interval: interval}
	return req
}

// metrics reads the metric records the store holds, newest first.
func (h *harness) metrics() []store.Metric {
	h.t.Helper()
	page, err := h.store.Query(context.Background(), store.SignalMetrics, store.Filter{})
	if err != nil {
		h.t.Fatalf("Query: %v", err)
	}
	return page.Metrics
}

// healthRecords is every kitbash.health record the store holds.
func (h *harness) healthRecords() []store.Metric {
	h.t.Helper()
	var out []store.Metric
	for _, m := range h.metrics() {
		if m.Name == HealthMetric {
			out = append(out, m)
		}
	}
	return out
}

// waitForHealth waits for at least one kitbash.health record and answers the
// newest.
func (h *harness) waitForHealth(what string) store.Metric {
	h.t.Helper()
	var records []store.Metric
	waitFor(h.t, what, func() bool {
		records = h.healthRecords()
		return len(records) > 0
	})
	return records[0]
}

// TestHealthProbeRecordsAHealthyProcess is the sentence of issue #92: a
// Process that declares a health path is probed, and what the probe saw is one
// metric record carrying the four attributes PLAN.md section 2.4 requires.
func TestHealthProbeRecordsAHealthyProcess(t *testing.T) {
	h := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)

	req := probeRegistration(stub.server.URL, "/healthz", "")
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}

	record := h.waitForHealth("the first health record")
	if record.Value != healthyValue {
		t.Errorf("value = %v, want %v for a Process answering 200", record.Value, float64(healthyValue))
	}
	if got := record.Attributes.Other[AttrHealthStatus]; got != "200" {
		t.Errorf("%s = %v, want the HTTP status", AttrHealthStatus, got)
	}
	// The four attributes, which are what makes the record queryable by the
	// member, by the Package and by the Process.
	if record.Attributes.User != h.user {
		t.Errorf("user = %q, want the owner %q", record.Attributes.User, h.user)
	}
	if record.Attributes.Package != req.Package {
		t.Errorf("package = %q, want %q", record.Attributes.Package, req.Package)
	}
	if record.Attributes.Process != req.ID {
		t.Errorf("process = %q, want %q", record.Attributes.Process, req.ID)
	}
	if record.Attributes.Path != "/healthz" {
		t.Errorf("path = %q, want the probed path", record.Attributes.Path)
	}
	// The producer is the daemon: a probe is not something the Process said
	// about itself. It is not an internal cause either, or its owner could
	// not read it.
	if record.Attributes.Producer != InternalProducer {
		t.Errorf("producer = %q, want %q", record.Attributes.Producer, InternalProducer)
	}
	if record.Attributes.Internal != nil {
		t.Errorf("internal = %v, want a record the owner reads", *record.Attributes.Internal)
	}
	if path := <-stub.paths; path != "/healthz" {
		t.Errorf("the Process was asked for %q, want /healthz", path)
	}
}

// The record is answered by tel_query, which is where a member reads it: the
// probe is written through the path every stored record takes.
func TestHealthProbeIsAnsweredByTheQuerySurface(t *testing.T) {
	h := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)
	h.register(probeRegistration(stub.server.URL, "/healthz", ""))
	h.waitForHealth("the first health record")

	res, body := h.postJSON(http.MethodPost, queryPath, queryRequest{Signal: store.SignalMetrics})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("query status = %d, body %s", res.StatusCode, body)
	}
	var got queryResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("query body %q: %v", body, err)
	}
	if len(got.Records) == 0 {
		t.Fatalf("tel_query answered no records, want the health probe")
	}
	first, ok := got.Records[0].(map[string]any)
	if !ok {
		t.Fatalf("record = %#v, want an object", got.Records[0])
	}
	if first["name"] != HealthMetric {
		t.Errorf("name = %v, want %s", first["name"], HealthMetric)
	}
}

// A Process that answers 500 is unhealthy, and the record says which status it
// answered with. Nothing else happens: the registration stands and the
// container is not touched.
func TestHealthProbeRecordsAnUnhealthyProcess(t *testing.T) {
	h := serveProbing(t)
	stub := newProbeStub(t, http.StatusInternalServerError)

	req := probeRegistration(stub.server.URL, "/healthz", "")
	h.register(req)

	record := h.waitForHealth("the health record of a Process answering 500")
	if record.Value != unhealthyValue {
		t.Errorf("value = %v, want %v for a Process answering 500", record.Value, float64(unhealthyValue))
	}
	if got := record.Attributes.Other[AttrHealthStatus]; got != "500" {
		t.Errorf("%s = %v, want 500", AttrHealthStatus, got)
	}
	// A probe records and does nothing else: the Process it found unhealthy
	// is registered exactly as it was.
	_, found, err := h.store.Process(context.Background(), req.ID)
	if err != nil || !found {
		t.Errorf("the unhealthy Process is no longer registered (%v, found %v); a probe changes no state", err, found)
	}
}

// A Process nothing answers for is unhealthy with the class of the failure
// rather than a status, because there was none.
func TestHealthProbeRecordsAProcessItCannotReach(t *testing.T) {
	h := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)
	endpoint := stub.server.URL
	// The port is closed before anything is registered, so the probe finds
	// nothing listening where the registration says the Process is.
	stub.server.Close()

	h.register(probeRegistration(endpoint, "/healthz", ""))

	record := h.waitForHealth("the health record of a Process nothing answers for")
	if record.Value != unhealthyValue {
		t.Errorf("value = %v, want %v for a Process nothing answers for", record.Value, float64(unhealthyValue))
	}
	if got := record.Attributes.Other[AttrHealthStatus]; got != healthUnreachable {
		t.Errorf("%s = %v, want %q", AttrHealthStatus, got, healthUnreachable)
	}
}

// The probe repeats on the interval the manifest declared, which is what makes
// it a probe and not a reading taken once at registration.
func TestHealthProbeRepeatsOnItsInterval(t *testing.T) {
	h := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)

	h.register(probeRegistration(stub.server.URL, "/healthz", "10ms"))

	waitFor(t, "three probes on the interval", func() bool { return stub.requests.Load() >= 3 })
	waitFor(t, "a record for each probe", func() bool { return len(h.healthRecords()) >= 3 })
}

// An interval under the floor is raised to it rather than refused: the
// manifest is valid, and what it asks for is not something the daemon does.
func TestHealthProbeIntervalIsHeldToTheFloor(t *testing.T) {
	pr := newProber(MinHealthInterval)
	cases := []struct {
		spelling string
		want     time.Duration
	}{
		{"", DefaultHealthInterval},
		{"100ms", MinHealthInterval},
		{"5s", MinHealthInterval},
		{"45s", 45 * time.Second},
		{"2m", 2 * time.Minute},
		{"whenever", DefaultHealthInterval},
	}
	for _, tc := range cases {
		if got := pr.every(tc.spelling); got != tc.want {
			t.Errorf("every(%q) = %s, want %s", tc.spelling, got, tc.want)
		}
	}
}

// Unregistering a Process stops its probe. The registration is what kitbashd
// probes from, so a Process nobody runs is a Process nobody requests.
func TestHealthProbeStopsWhenTheProcessIsUnregistered(t *testing.T) {
	h := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)

	req := probeRegistration(stub.server.URL, "/healthz", "10ms")
	h.register(req)
	waitFor(t, "the first probes", func() bool { return stub.requests.Load() >= 2 })

	res, body := h.do(http.MethodDelete, processesPath+"/"+req.ID, "", nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("unregister status = %d, body %s", res.StatusCode, body)
	}
	if n := h.server.probes.count(); n != 0 {
		t.Fatalf("%d Processes are still probed after unregistering the one", n)
	}

	// Whatever was in flight at the moment of the DELETE may still land, so
	// the count is read after a pause and must then hold still for a window
	// many intervals long.
	time.Sleep(10 * testHealthInterval)
	settled := stub.requests.Load()
	time.Sleep(20 * testHealthInterval)
	if got := stub.requests.Load(); got != settled {
		t.Errorf("the Process was probed %d more times after it was unregistered", got-settled)
	}
}

// A Process that declares no path, or that publishes no endpoint, is not
// probed at all. Probing is what a Package asks for, not what every Process
// gets.
func TestHealthProbeNeedsADeclarationAndAnEndpoint(t *testing.T) {
	h := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)

	noPath := registration("")
	noPath.Expose = ExposeHTTP
	noPath.Endpoint = stub.server.URL
	h.register(noPath)

	noEndpoint := registration("")
	noEndpoint.Health = &healthRequest{HTTP: "/healthz"}
	h.register(noEndpoint)

	if n := h.server.probes.count(); n != 0 {
		t.Errorf("%d Processes are probed, want none of these two", n)
	}
	time.Sleep(10 * testHealthInterval)
	if n := stub.requests.Load(); n != 0 {
		t.Errorf("a Process that declared no path was probed %d times", n)
	}
}

// The daemon probes the Processes it finds registered at start, which is what
// a host that rebooted has: the loop runs after restore and loads them itself.
func TestHealthLoopProbesWhatIsAlreadyRegistered(t *testing.T) {
	h := serveWith(t, Options{
		Admin:             func(*user.User) (bool, error) { return false, nil },
		HealthMinInterval: testHealthInterval,
	})
	stub := newProbeStub(t, http.StatusOK)
	if err := h.store.RegisterProcess(context.Background(), store.Process{
		ID:           registration("").ID,
		Owner:        h.user,
		Package:      "/org/sensorium",
		Expose:       ExposeHTTP,
		Endpoint:     stub.server.URL,
		Health:       store.Health{HTTP: "/healthz", Interval: "10ms"},
		RegisteredAt: time.Now().UTC(),
	}, "hash-of-a-token", 0); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}

	h.probeLoop()
	h.waitForHealth("the health record of a Process that was registered before the daemon started")
}

// processes_list carries the declaration and the most recent reading, which is
// what proc_list publishes as health: {last, healthy}.
func TestProcessesListCarriesTheLastProbe(t *testing.T) {
	h := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)

	req := probeRegistration(stub.server.URL, "/healthz", "10ms")
	h.register(req)
	h.waitForHealth("the first health record")

	var listed listedProcess
	waitFor(t, "the reading on processes_list", func() bool {
		res, body := h.do(http.MethodGet, processesPath, "", nil)
		if res.StatusCode != http.StatusOK {
			h.t.Fatalf("list status = %d, body %s", res.StatusCode, body)
		}
		var got processList
		if err := json.Unmarshal(body, &got); err != nil {
			h.t.Fatalf("list body %q: %v", body, err)
		}
		for _, p := range got.Processes {
			if p.ID == req.ID && p.Health.Healthy != nil {
				listed = p
				return true
			}
		}
		return false
	})
	if listed.Health.HTTP != "/healthz" || listed.Health.Interval != "10ms" {
		t.Errorf("health declaration = %+v, want the one registered", listed.Health)
	}
	if !*listed.Health.Healthy {
		t.Errorf("healthy = false for a Process answering 200")
	}
	if _, err := time.Parse(time.RFC3339Nano, listed.Health.Last); err != nil {
		t.Errorf("last = %q, want an RFC 3339 time: %v", listed.Health.Last, err)
	}
}

// The declaration survives a restart, because it travels with the
// registration: restore has no manifest to read.
func TestHealthDeclarationIsStoredWithTheRegistration(t *testing.T) {
	h := serve(t, false)
	req := probeRegistration("http://127.0.0.1:40275", "/healthz", "45s")
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	p, found, err := h.store.Process(context.Background(), req.ID)
	if err != nil || !found {
		t.Fatalf("Process: %v, found %v", err, found)
	}
	if p.Health.HTTP != "/healthz" || p.Health.Interval != "45s" {
		t.Errorf("stored health = %+v, want the declaration", p.Health)
	}
	if p.Health.Last != "" || p.Health.Healthy != nil {
		t.Errorf("stored health = %+v, want no reading in the store", p.Health)
	}
}

// A registration kitbashd cannot probe is refused with a fix, rather than
// stored as a declaration nothing acts on.
func TestRegisteringRefusesAHealthBlockItCannotProbe(t *testing.T) {
	h := serve(t, false)
	cases := []struct {
		name   string
		health healthRequest
	}{
		{"no path", healthRequest{Interval: "30s"}},
		{"not a path", healthRequest{HTTP: "http://127.0.0.1:40275/healthz"}},
		{"a space in the path", healthRequest{HTTP: "/health z"}},
		{"not an interval", healthRequest{HTTP: "/healthz", Interval: "half a minute"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := probeRegistration("http://127.0.0.1:40275", "", "")
			req.Health = &healthRequest{HTTP: tc.health.HTTP, Interval: tc.health.Interval}
			res, body := h.postJSON(http.MethodPost, processesPath, req)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, body %s", res.StatusCode, body)
			}
			if fix := h.problemOf(res, body).Fix; fix == "" {
				t.Error("the problem carries no fix")
			}
		})
	}
}
