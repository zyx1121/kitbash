package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
	"github.com/zyx1121/kitbash/internal/uuid"
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

// blockingStub is a Process that takes a request and answers nothing until the
// test lets it, which is how a request in flight is held still while the
// registration behind it changes.
type blockingStub struct {
	server  *httptest.Server
	arrived atomic.Int64
	held    chan struct{}
	once    sync.Once
}

// newBlockingStub serves a health path that waits.
func newBlockingStub(t *testing.T) *blockingStub {
	t.Helper()
	stub := &blockingStub{held: make(chan struct{})}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.arrived.Add(1)
		<-stub.held
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		stub.release()
		stub.server.Close()
	})
	return stub
}

// release answers every request this stub is holding, once.
func (s *blockingStub) release() { s.once.Do(func() { close(s.held) }) }

// process reads one registration back, for a test that seeded it.
func (h *harness) process(id string) store.Process {
	h.t.Helper()
	p, found, err := h.store.Process(context.Background(), id)
	if err != nil || !found {
		h.t.Fatalf("Process %s: %v, found %v", id, err, found)
	}
	return p
}

// serveProbing starts a daemon whose probe floor is short enough for a test,
// on a fake host: what a container publishes is what kitbashd checks a probe
// against, so it is the test's to stage. The probe loop runs until the test is
// over, which is what kitbashd does after restore.
func serveProbing(t *testing.T) (*harness, *sysusers.Fake) {
	t.Helper()
	h, fake := serveProbingHost(t)
	h.probeLoop()
	return h, fake
}

// serveProbingHost is serveProbing without the loop, for a test that seeds the
// store first and starts the loop itself.
func serveProbingHost(t *testing.T) (*harness, *sysusers.Fake) {
	t.Helper()
	fake := sysusers.NewFake()
	h := serveWith(t, Options{
		Admin:             func(*user.User) (bool, error) { return false, nil },
		Users:             fake,
		Runner:            fake,
		HealthMinInterval: testHealthInterval,
	})
	// The member runs as whoever runs the tests: a start writes the
	// environment file to that account, and root's uid is not the test's to
	// hand a file to.
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid()})
	return h, fake
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
// path on the endpoint given, with a container name of its own so a test can
// say what that container publishes.
func probeRegistration(endpoint, path, interval string) processRequest {
	req := registration("")
	req.Expose = ExposeHTTP
	req.Endpoint = endpoint
	req.Container = "kitbash-probe-" + strings.Split(req.ID, "-")[0]
	// The digest is what a start runs, so a test that goes on to start this
	// Process registers one, the way proc_run does.
	req.Digest = testDigest
	req.Health = &healthRequest{HTTP: path, Interval: interval}
	return req
}

// registerProbed registers a Process whose container publishes the port its
// endpoint names, which is the only thing kitbashd agrees to probe.
func (h *harness) registerProbed(fake *sysusers.Fake, endpoint, path, interval string) processRequest {
	h.t.Helper()
	req := probeRegistration(endpoint, path, interval)
	port, ok := endpointPort(endpoint)
	if !ok {
		h.t.Fatalf("the endpoint %q names no port", endpoint)
	}
	fake.Publish(req.Container, port)
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		h.t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	return req
}

// storedProbe writes one registration that declares a probe straight to the
// store, with its container publishing the endpoint's port, which is what a
// daemon that has just started finds. It answers the Process id.
func (h *harness) storedProbe(fake *sysusers.Fake, container, endpoint, interval string) string {
	h.t.Helper()
	return h.storedProbeOf(fake, h.user, container, endpoint, interval)
}

// storedProbeOf is storedProbe for a Process of another member, which is what
// an admin removing that member leaves behind.
func (h *harness) storedProbeOf(fake *sysusers.Fake, owner, container, endpoint, interval string) string {
	h.t.Helper()
	port, ok := endpointPort(endpoint)
	if !ok {
		h.t.Fatalf("the endpoint %q names no port", endpoint)
	}
	fake.Publish(container, port)
	_, hash, err := store.NewToken()
	if err != nil {
		h.t.Fatalf("NewToken: %v", err)
	}
	id := uuid.V7()
	if err := h.store.RegisterProcess(context.Background(), store.Process{
		ID:           id,
		Owner:        owner,
		Package:      "/org/sensorium",
		Container:    container,
		Expose:       ExposeHTTP,
		Endpoint:     endpoint,
		Health:       store.Health{HTTP: "/healthz", Interval: interval},
		RegisteredAt: time.Now().UTC(),
	}, hash, store.Quota{}); err != nil {
		h.t.Fatalf("RegisterProcess: %v", err)
	}
	return id
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
	h, fake := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)

	req := h.registerProbed(fake, stub.server.URL, "/healthz", "")

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
	h, fake := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)
	h.registerProbed(fake, stub.server.URL, "/healthz", "")
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
	h, fake := serveProbing(t)
	stub := newProbeStub(t, http.StatusInternalServerError)

	req := h.registerProbed(fake, stub.server.URL, "/healthz", "")

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
	h, fake := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)
	endpoint := stub.server.URL
	// The port is closed before anything is registered, so the probe finds
	// nothing listening where the registration says the Process is.
	stub.server.Close()

	h.registerProbed(fake, endpoint, "/healthz", "")

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
	h, fake := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)

	h.registerProbed(fake, stub.server.URL, "/healthz", "10ms")

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
	h, fake := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)

	req := h.registerProbed(fake, stub.server.URL, "/healthz", "10ms")
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
	h, fake := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)

	noPath := registration("")
	noPath.Expose = ExposeHTTP
	noPath.Endpoint = stub.server.URL
	h.register(noPath)

	noEndpoint := registration("")
	noEndpoint.Container = "kitbash-probe-none"
	noEndpoint.Health = &healthRequest{HTTP: "/healthz"}
	fake.Publish(noEndpoint.Container, 40275)
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
	h, fake := serveProbingHost(t)
	stub := newProbeStub(t, http.StatusOK)
	h.storedProbe(fake, "kitbash-probe-restored", stub.server.URL, "10ms")

	h.probeLoop()
	h.waitForHealth("the health record of a Process that was registered before the daemon started")
}

// processes_list carries the declaration and the most recent reading, which is
// what proc_list publishes as health: {last, healthy}.
func TestProcessesListCarriesTheLastProbe(t *testing.T) {
	h, fake := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)

	req := h.registerProbed(fake, stub.server.URL, "/healthz", "10ms")
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
	h, fake := serveProbingHost(t)
	req := h.registerProbed(fake, "http://127.0.0.1:40275", "/healthz", "45s")
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
	h, _ := serveProbingHost(t)
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

// The port a probe requests is not the member's to choose. kitbashd asks the
// runtime, as the owner, what that Process's own container publishes, and a
// registration naming anything else is refused: without this a member could
// have the daemon request any loopback port on a schedule and read the status
// back off kitbash.health, see verifyProbe.
func TestRegisteringRefusesAPortTheContainerDoesNotPublish(t *testing.T) {
	h, fake := serveProbingHost(t)
	stub := newProbeStub(t, http.StatusOK)
	published, _ := endpointPort(stub.server.URL)

	req := probeRegistration(stub.server.URL, "/healthz", "")
	// The container is there and publishes a different port, which is the
	// neighbour's port this registration is trying to be pointed at.
	fake.Publish(req.Container, published+1)
	res, body := h.postJSON(http.MethodPost, processesPath, req)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	prob := h.problemOf(res, body)
	if prob.Slug() != problem.SlugNotPermitted {
		t.Errorf("problem is %s, want not-permitted", prob.Slug())
	}
	if !strings.Contains(prob.Fix, "own published port") {
		t.Errorf("fix is %q, want it to name the Process's own published port", prob.Fix)
	}
	if _, found, _ := h.store.Process(context.Background(), req.ID); found {
		t.Error("the refused registration was written anyway")
	}
	if n := h.server.probes.count(); n != 0 {
		t.Errorf("%d Processes are probed after a refused registration", n)
	}
}

// The port the container does publish is accepted and probed.
func TestRegisteringAcceptsThePortTheContainerPublishes(t *testing.T) {
	h, fake := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)

	h.registerProbed(fake, stub.server.URL, "/healthz", "")
	h.waitForHealth("the health record of a Process whose container publishes the port")
}

// A runtime that will not say what a container publishes is a registration
// refused: kitbashd probes what it has checked and nothing it has not.
func TestRegisteringRefusesAProbeItCannotCheck(t *testing.T) {
	h, fake := serveProbingHost(t)
	stub := newProbeStub(t, http.StatusOK)
	port, _ := endpointPort(stub.server.URL)

	req := probeRegistration(stub.server.URL, "/healthz", "")
	fake.Publish(req.Container, port)
	fake.ConfigErr = errors.New("podman: the runtime is not answering")

	res, body := h.postJSON(http.MethodPost, processesPath, req)
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	if _, found, _ := h.store.Process(context.Background(), req.ID); found {
		t.Error("a registration whose probe could not be checked was written anyway")
	}
}

// proc_run registers the Process before its container exists, so that
// registration is accepted and probed by nothing until the start that creates
// the container checks the port.
func TestAProcessIsProbedFromTheStartThatCreatesItsContainer(t *testing.T) {
	h, fake := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)
	port, _ := endpointPort(stub.server.URL)

	req := probeRegistration(stub.server.URL, "/healthz", "")
	fake.Missing = map[string]bool{req.Container: true}
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	if n := h.server.probes.count(); n != 0 {
		t.Fatalf("%d Processes are probed before the container exists", n)
	}

	// The start creates the container, publishing the port the registration
	// named, and the probe begins there.
	fake.Missing = map[string]bool{}
	res, body := h.start(req.ID, startRequest{
		Publish: []portMapping{{HostPort: port, ContainerPort: 8080}},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("start status = %d, body %s", res.StatusCode, body)
	}
	h.waitForHealth("the health record of a Process probed from its start")
}

// Registering the same Process again does not start a second probe and does
// not bring the next one forward: one Process is requested at most once per
// interval however often it is registered, see track.
func TestReRegisteringDoesNotBypassTheInterval(t *testing.T) {
	h, fake := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)

	// An interval far longer than this test, so every request after the first
	// one would have to be a registration bypassing the schedule.
	req := h.registerProbed(fake, stub.server.URL, "/healthz", "10m")
	waitFor(t, "the first probe", func() bool { return stub.requests.Load() >= 1 })

	for range 10 {
		if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
			t.Fatalf("register status = %d, body %s", res.StatusCode, body)
		}
	}
	time.Sleep(10 * testHealthInterval)
	if n := stub.requests.Load(); n != 1 {
		t.Errorf("the Process was requested %d times, want the one its interval allows", n)
	}
	if n := len(h.healthRecords()); n != 1 {
		t.Errorf("%d health records, want the one its interval allows", n)
	}
}

// A reading of a declaration that was replaced while the request was in flight
// is dropped: it is about an endpoint this Process no longer names.
func TestAReadingOfAReplacedDeclarationIsDropped(t *testing.T) {
	h, fake := serveProbing(t)
	slow := newBlockingStub(t)
	quick := newProbeStub(t, http.StatusOK)

	req := h.registerProbed(fake, slow.server.URL, "/healthz", "10m")
	waitFor(t, "the request that is now in flight", func() bool { return slow.arrived.Load() >= 1 })

	// The same Process is registered again at another endpoint, which its
	// container publishes as well, while the first request is still waiting.
	quickPort, _ := endpointPort(quick.server.URL)
	fake.Publish(req.Container, quickPort)
	moved := req
	moved.Endpoint = quick.server.URL
	if _, res, body := h.register(moved); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	slow.release()

	time.Sleep(10 * testHealthInterval)
	if n := len(h.healthRecords()); n != 0 {
		t.Errorf("%d health records, want none: the reading was about the endpoint that was replaced", n)
	}
	if n := h.server.probes.count(); n != 1 {
		t.Errorf("%d Processes are probed, want the one that was registered again", n)
	}
}

// A reading of a Process that was unregistered while its request was in flight
// is dropped as well, and nothing is written for it.
func TestAReadingOfAnUnregisteredProcessIsDropped(t *testing.T) {
	h, fake := serveProbing(t)
	slow := newBlockingStub(t)

	req := h.registerProbed(fake, slow.server.URL, "/healthz", "10m")
	waitFor(t, "the request that is now in flight", func() bool { return slow.arrived.Load() >= 1 })

	res, body := h.do(http.MethodDelete, processesPath+"/"+req.ID, "", nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("unregister status = %d, body %s", res.StatusCode, body)
	}
	slow.release()

	time.Sleep(10 * testHealthInterval)
	if n := len(h.healthRecords()); n != 0 {
		t.Errorf("%d health records, want none for a Process that is no longer registered", n)
	}
}

// A Process that does not answer in time is unhealthy with the class of the
// failure, which is not the same problem as nothing listening.
func TestAProbeThatTimesOutIsRecordedAsATimeout(t *testing.T) {
	h, _ := serveProbingHost(t)
	slow := newBlockingStub(t)
	t.Cleanup(slow.release)

	// The deadline of the caller is what runs out here, which is the same
	// path HealthTimeout takes without a test waiting five seconds for it.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	healthy, status := h.server.request(ctx, probe{endpoint: slow.server.URL, path: "/healthz"})
	if healthy {
		t.Error("a Process that never answered was recorded as healthy")
	}
	if status != healthTimedOut {
		t.Errorf("status = %q, want %q", status, healthTimedOut)
	}
}

// Removing a member unregisters their Processes, and nothing of theirs is
// probed afterwards.
func TestRemovingAMemberStopsProbingTheirProcesses(t *testing.T) {
	h, fake := serveUsers(t, true)
	stub := newProbeStub(t, http.StatusOK)
	fake.Add(sysusers.Member{Name: "alice", UID: 1007})
	id := h.storedProbeOf(fake, "alice", "kitbash-probe-alice", stub.server.URL, "10ms")

	h.server.loadProbes(context.Background(), []store.Process{h.process(id)})
	if n := h.server.probes.count(); n != 1 {
		t.Fatalf("%d Processes are probed, want the one of alice", n)
	}

	res, body := h.do(http.MethodDelete, usersPath+"/alice", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("remove status = %d, body %s", res.StatusCode, body)
	}
	if n := h.server.probes.count(); n != 0 {
		t.Errorf("%d Processes of a removed member are still probed", n)
	}
}

// Restore unregisters a Process whose container the runtime no longer has, and
// stops probing it with it.
func TestRestoreStopsProbingAProcessWhoseContainerIsGone(t *testing.T) {
	h, fake := serveUsers(t, false)
	stub := newProbeStub(t, http.StatusOK)
	id := h.storedProbe(fake, "kitbash-probe-gone", stub.server.URL, "10ms")

	h.server.loadProbes(context.Background(), []store.Process{h.process(id)})
	if n := h.server.probes.count(); n != 1 {
		t.Fatalf("%d Processes are probed, want the one that is registered", n)
	}

	fake.Missing = map[string]bool{"kitbash-probe-gone": true}
	h.server.Restore(context.Background())
	if n := h.server.probes.count(); n != 0 {
		t.Errorf("%d Processes are still probed after restore unregistered them", n)
	}
}

// A Process a run kit owns is not probed: kitbashd does not supervise it, and
// there is no container of it here to check an endpoint against. A Package
// that declares one anyway is refused rather than ignored, see PLAN.md
// section 3.
func TestRegisteringRefusesAProbeOnAProcessARunKitOwns(t *testing.T) {
	h, _ := serveProbing(t)
	stub := newProbeStub(t, http.StatusOK)

	req := probeRegistration(stub.server.URL, "/healthz", "")
	// A Process a kit owns carries a runner and no container, which is what
	// proc_run registers when a manifest names one.
	req.Runner = "/org/pve-runner"
	req.Container = ""
	req.Digest = ""
	res, body := h.postJSON(http.MethodPost, processesPath, req)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	prob := h.problemOf(res, body)
	if prob.Slug() != problem.SlugNotPermitted {
		t.Errorf("problem is %s, want not-permitted", prob.Slug())
	}
	if !strings.Contains(prob.Detail, "run kit owns") {
		t.Errorf("detail is %q, want it to say a run kit owns this Process", prob.Detail)
	}
	if n := h.server.probes.count(); n != 0 {
		t.Errorf("%d Processes are probed, want none", n)
	}

	// The same Process without a health block is registered as it was, and
	// still nothing probes it.
	kitOwned := registration("")
	kitOwned.Runner = "/org/pve-runner"
	kitOwned.Expose = ExposeHTTP
	kitOwned.Endpoint = stub.server.URL
	if _, res, body := h.register(kitOwned); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	if n := h.server.probes.count(); n != 0 {
		t.Errorf("%d Processes are probed, want none for a Process a kit owns", n)
	}
	time.Sleep(10 * testHealthInterval)
	if n := stub.requests.Load(); n != 0 {
		t.Errorf("a Process a run kit owns was probed %d times", n)
	}
}

// The prober leaves a stored registration a kit owns alone as well, which is
// what a daemon that has just started finds for one.
func TestTheProbeLoopSkipsAProcessARunKitOwns(t *testing.T) {
	h, fake := serveProbingHost(t)
	stub := newProbeStub(t, http.StatusOK)
	id := h.storedProbe(fake, "kitbash-probe-kit", stub.server.URL, "10ms")
	p := h.process(id)
	p.Runner = "/org/pve-runner"
	p.Container = ""
	if err := h.store.RegisterProcess(context.Background(), p, "hash-of-a-token", store.Quota{}); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}

	h.probeLoop()
	time.Sleep(10 * testHealthInterval)
	if n := h.server.probes.count(); n != 0 {
		t.Errorf("%d Processes are probed, want none for a Process a kit owns", n)
	}
	if n := stub.requests.Load(); n != 0 {
		t.Errorf("a Process a run kit owns was probed %d times", n)
	}
	if n := len(h.healthRecords()); n != 0 {
		t.Errorf("%d health records for a Process a run kit owns", n)
	}
}
