package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/sysusers"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// A container that died outside proc_stop keeps the port its route was built
// with: the state is read when the route is built, at a registration, a start
// and a restore, and a crash, an OOM kill or a podman stop by the owner
// rebuilds nothing. The proxy then dials a port nothing is bound to and
// answers 502, when the honest answer is the one a stopped Process gets, see
// issue #149.

// TestADeadContainerLosesItsPortOnTheFirstRequest is the whole of the fix: the
// first request after the death is answered 503 and the port is off the table,
// so nothing after it dials a port the container has freed.
func TestADeadContainerLosesItsPortOnTheFirstRequest(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	req := h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)
	if rec := h.request(http.MethodGet, name, "/", nil); rec.Code != http.StatusOK {
		t.Fatalf("before the death %s answered %d, want it served", name, rec.Code)
	}

	// The container dies on its own: the runtime calls it exited and nothing
	// told kitbashd, so the route still carries the port.
	up.server.Close()
	fake.SetState(req.Container, podman.StateExited)
	if port, held := h.server.proxy.portOf(req.ID); !held || port == 0 {
		t.Fatalf("the route is %d, %t before the request, want it still carrying the port", port, held)
	}

	rec := h.request(http.MethodGet, name, "/", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 on the first request after the container died", rec.Code)
	}
	if body := rec.Body.String(); body != name+" is registered on this host and its Process is not running.\n" {
		t.Errorf("body = %q, want the short body a stopped Process answers with", body)
	}
	port, held := h.server.proxy.portOf(req.ID)
	if !held {
		t.Fatal("the name is gone entirely, want it held and answering 503")
	}
	if port != 0 {
		t.Errorf("the table still carries port %d, so the next request dials a port the container freed", port)
	}
	// And the name still answers 503 rather than 404, because the
	// registration stands.
	if rec := h.request(http.MethodGet, name, "/", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("the second request answered %d, want 503", rec.Code)
	}
}

// A Process that is up and refusing connections is not a Process that is gone.
// The re-resolve asks the runtime and believes it: a container the runtime
// calls running keeps its port and its 502, which is what a member reads when
// their application is listening on the wrong address inside the container.
func TestARunningContainerThatRefusesStillAnswers502(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	req := h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)

	// Nothing is listening any more, and the runtime still calls the
	// container running, which is what registerServed staged.
	up.server.Close()

	rec := h.request(http.MethodGet, name, "/", nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for a running container that refuses connections", rec.Code)
	}
	port, held := h.server.proxy.portOf(req.ID)
	if !held || port == 0 {
		t.Errorf("the route is %d, %t, want a running Process to keep its port", port, held)
	}
}

// serveProbedProxy is a daemon that both routes and probes: the probe is the
// only thing on this host that asks a Process whether it is there on a
// cadence, so it is what notices a container that came back.
func serveProbedProxy(t *testing.T) (*harness, *sysusers.Fake) {
	t.Helper()
	fake := sysusers.NewFake()
	h := serveWith(t, Options{
		Admin:             func(*user.User) (bool, error) { return false, nil },
		Users:             fake,
		Runner:            fake,
		Domain:            testDomain,
		TLS:               TLSGateway,
		CertDir:           filepath.Join(t.TempDir(), "certs"),
		PublicAddress:     testPublicAddress,
		Resolver:          newZone(),
		HealthMinInterval: testHealthInterval,
	})
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid()})
	return h, fake
}

// restartableStub is a Process that can be taken away and put back on the same
// port, which is what a container that died and was started again looks like
// from this host: the port a container publishes is the port it publishes
// again, and what changes is whether anything is bound to it.
type restartableStub struct {
	t    *testing.T
	port int
	mu   sync.Mutex
	ln   net.Listener
	srv  *http.Server
}

func newRestartableStub(t *testing.T) *restartableStub {
	t.Helper()
	stub := &restartableStub{t: t}
	stub.listen()
	_, port, err := net.SplitHostPort(stub.ln.Addr().String())
	if err != nil {
		t.Fatalf("the stub is listening on %q: %v", stub.ln.Addr(), err)
	}
	stub.port, err = strconv.Atoi(port)
	if err != nil {
		t.Fatalf("the stub port %q: %v", port, err)
	}
	t.Cleanup(stub.stop)
	return stub
}

// listen serves on the stub's port, taking the first free one when it has none
// yet.
func (s *restartableStub) listen() {
	s.t.Helper()
	address := "127.0.0.1:0"
	if s.port != 0 {
		address = net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port))
	}
	ln, err := net.Listen("tcp", address)
	if err != nil {
		s.t.Fatalf("listening on %s: %v", address, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("the Process answered"))
	})}
	s.mu.Lock()
	s.ln, s.srv = ln, srv
	s.mu.Unlock()
	go srv.Serve(ln)
}

// url is the endpoint this Process publishes.
func (s *restartableStub) url() string {
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port))
}

// stop takes the Process away, leaving the port refusing connections, which is
// what a container that died leaves behind.
func (s *restartableStub) stop() {
	s.mu.Lock()
	srv, ln := s.srv, s.ln
	s.srv, s.ln = nil, nil
	s.mu.Unlock()
	if srv == nil {
		return
	}
	srv.Close()
	ln.Close()
}

// TestTheProbeRetracksAContainerThatIsRunningAgain is the other half of the
// fix. A container that died outside proc_stop keeps its port and a container
// started again outside proc_run never gets one back, and the probe is the
// only thing on this host that asks a Process whether it is there on a
// cadence, so it is what puts both right.
func TestTheProbeRetracksAContainerThatIsRunningAgain(t *testing.T) {
	h, fake := serveProbedProxy(t)
	stub := newRestartableStub(t)
	req := h.registerProbed(fake, stub.url(), "/healthz", "20ms")
	fake.SetState(req.Container, podman.StateRunning)
	h.server.trackRouteFor(context.Background(), h.process(req.ID))
	name := h.defaultName(req.Name)
	if port, held := h.server.proxy.portOf(req.ID); !held || port == 0 {
		t.Fatalf("the route is %d, %t before anything, want it serving", port, held)
	}

	// The container dies on its own, which nothing told kitbashd.
	stub.stop()
	fake.SetState(req.Container, podman.StateExited)
	h.probeLoop()
	waitFor(t, "the probe to drop the port of a container that stopped", func() bool {
		port, held := h.server.proxy.portOf(req.ID)
		return held && port == 0
	})
	if rec := h.request(http.MethodGet, name, "/", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 while the container is not running", rec.Code)
	}

	// And it is started again on the port it publishes, which nothing told
	// kitbashd either.
	stub.listen()
	fake.SetState(req.Container, podman.StateRunning)
	waitFor(t, "the probe to put the port back once the container is running again", func() bool {
		port, held := h.server.proxy.portOf(req.ID)
		return held && port != 0
	})
	if rec := h.request(http.MethodGet, name, "/", nil); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want the Process served again", rec.Code)
	}
}

// A Process that is up and answering something other than 2xx on its health
// path is unhealthy and not gone: the probe says what the application
// answered and the route says what the runtime holds, and only the second
// decides what is forwarded to.
func TestAnUnhealthyProcessKeepsItsPort(t *testing.T) {
	h, fake := serveProbedProxy(t)
	stub := newProbeStub(t, http.StatusInternalServerError)
	req := h.registerProbed(fake, stub.server.URL, "/healthz", "20ms")
	fake.SetState(req.Container, podman.StateRunning)
	h.server.trackRouteFor(context.Background(), h.process(req.ID))
	h.probeLoop()

	waitFor(t, "the Process to be probed", func() bool {
		_, _, probed := h.server.probes.reading(req.ID)
		return probed
	})
	if port, held := h.server.proxy.portOf(req.ID); !held || port == 0 {
		t.Errorf("the route is %d, %t, want an unhealthy but running Process to keep its port", port, held)
	}
}

// The proxy is what a Process's own container publishes and nothing else, so a
// re-resolve that cannot read the runtime leaves the table as it was: what the
// port says is the last thing this daemon knew, and 502 is the honest answer
// to a forward that failed for a reason nothing here could read.
func TestARuntimeThatWillNotAnswerLeavesTheRouteAlone(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	req := h.registerServed(fake, up, testProcessName, "")
	up.server.Close()
	fake.ConfigErr = errors.New("podman: the runtime is not answering")

	rec := h.request(http.MethodGet, h.defaultName(testProcessName), "/", nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 when the runtime says nothing about the container", rec.Code)
	}
	if port, held := h.server.proxy.portOf(req.ID); !held || port == 0 {
		t.Errorf("the route is %d, %t, want it untouched when nothing could be read", port, held)
	}
}

// A route whose Process is registered with no container at all is never
// re-resolved: there is nothing to ask the runtime about.
func TestARouteWithNoContainerIsNotReResolved(t *testing.T) {
	h, _, _ := serveProxying(t, testDomain, TLSGateway)
	if h.server.dropDeadRoute(context.Background(), route{id: "x", owner: h.user}) {
		t.Error("a route with no container name was dropped, want it left alone")
	}
}

// countingRunner counts how often the daemon asks the runtime about a
// container, which is one podman inspect per call on a real host. Adopted from
// the review of #159.
type countingRunner struct {
	*sysusers.Fake
	inspects atomic.Int64
}

func (c *countingRunner) ContainerConfig(ctx context.Context, m sysusers.Member, container string) (sysusers.ContainerConfig, error) {
	c.inspects.Add(1)
	return c.Fake.ContainerConfig(ctx, m, container)
}

func serveCounted(t *testing.T) (*harness, *sysusers.Fake, *countingRunner) {
	t.Helper()
	fake := sysusers.NewFake()
	counted := &countingRunner{Fake: fake}
	h := serveWith(t, Options{
		Admin:         func(*user.User) (bool, error) { return false, nil },
		Users:         fake,
		Runner:        counted,
		Domain:        testDomain,
		TLS:           TLSGateway,
		CertDir:       filepath.Join(t.TempDir(), "certs"),
		PublicAddress: testPublicAddress,
		Resolver:      newZone(),
	})
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid()})
	return h, fake, counted
}

// What a 502 costs. A container that is up and refusing connections is a 502
// for as long as that lasts, and the request rate is the internet's to set:
// without a window a caller holding the URL would set the rate this daemon
// inspects containers at, one podman child per request. Adopted from the
// review of #159.
func TestARefusingContainerCostsOneInspectPerWindow(t *testing.T) {
	h, fake, counted := serveCounted(t)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)
	up.server.Close() // the application is gone, the container is not

	counted.inspects.Store(0)
	for range 50 {
		if rec := h.request(http.MethodGet, name, "/", nil); rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", rec.Code)
		}
	}
	if n := counted.inspects.Load(); n > 1 {
		t.Errorf("50 refused requests cost %d runtime calls; a caller with the URL sets the rate", n)
	}
}

// And what a 503 costs, on the answer that drops the port: the first request
// asks and the rest read the table, because the port is gone by then.
func TestADeadContainerCostsOneInspect(t *testing.T) {
	h, fake, counted := serveCounted(t)
	up := newUpstream(t)
	req := h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)
	up.server.Close()
	fake.SetState(req.Container, podman.StateExited)

	counted.inspects.Store(0)
	for range 21 {
		if rec := h.request(http.MethodGet, name, "/", nil); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
	}
	if n := counted.inspects.Load(); n > 1 {
		t.Errorf("21 requests for a container that is gone cost %d runtime calls", n)
	}
}

// A container the runtime does not have at all is a container that is not
// running, which is what podman rm by the owner leaves behind. It is the
// answer routePort gives no port for either. Adopted from the review of #159.
func TestAContainerTheRuntimeDoesNotHaveLosesItsPort(t *testing.T) {
	h, fake, counted := serveCounted(t)
	up := newUpstream(t)
	req := h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)
	up.server.Close()
	fake.Missing = map[string]bool{req.Container: true}

	counted.inspects.Store(0)
	rec := h.request(http.MethodGet, name, "/", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 for a container the runtime does not have", rec.Code)
	}
	if port, held := h.server.proxy.portOf(req.ID); held && port != 0 {
		t.Errorf("the route still carries port %d of a container that is gone", port)
	}
	for range 20 {
		h.request(http.MethodGet, name, "/", nil)
	}
	if n := counted.inspects.Load(); n > 1 {
		t.Errorf("21 requests for a gone container cost %d runtime calls", n)
	}
}

// Concurrent refusals are one inspect: the first caller asks and the rest wait
// for what it found rather than each starting a call of their own.
func TestConcurrentRefusalsShareOneInspect(t *testing.T) {
	h, fake, counted := serveCounted(t)
	up := newUpstream(t)
	req := h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)
	up.server.Close()
	fake.SetState(req.Container, podman.StateExited)

	counted.inspects.Store(0)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.request(http.MethodGet, name, "/", nil)
		}()
	}
	wg.Wait()
	if n := counted.inspects.Load(); n > 1 {
		t.Errorf("16 requests at once cost %d runtime calls, want the one they share", n)
	}
}

// A route built again forgets what a re-resolve found about it: the table has
// just been told what the runtime says, so an answer from before it is not
// about the route that is there now. Without that, a container that came back
// would go on being answered 503 from what the last inspect found, for as long
// as the window lasts.
func TestBuildingARouteAgainForgetsTheReResolve(t *testing.T) {
	h, fake, counted := serveCounted(t)
	up := newRestartableStub(t)
	req := h.registerProbed(fake, up.url(), "/healthz", "30s")
	fake.SetState(req.Container, podman.StateRunning)
	h.server.trackRouteFor(context.Background(), h.process(req.ID))
	name := h.defaultName(req.Name)

	up.stop()
	fake.SetState(req.Container, podman.StateExited)
	if rec := h.request(http.MethodGet, name, "/", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 once the container stopped", rec.Code)
	}

	// The container is running again and the route is built again, inside the
	// window the answer above would otherwise stand for. The application is
	// still not listening, so this request fails to dial as well and the
	// answer has to come from a new inspect rather than the old one.
	fake.SetState(req.Container, podman.StateRunning)
	h.server.trackRouteFor(context.Background(), h.process(req.ID))
	counted.inspects.Store(0)
	rec := h.request(http.MethodGet, name, "/", nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502: the container is running and the answer of the last inspect is stale", rec.Code)
	}
	if n := counted.inspects.Load(); n != 1 {
		t.Errorf("the request cost %d runtime calls, want the one that reads the route as it is now", n)
	}
	if port, held := h.server.proxy.portOf(req.ID); !held || port == 0 {
		t.Errorf("the route is %d, %t, want the port the rebuilt route carries", port, held)
	}
}

// A connection this daemon did not fail to dial is not a container that is
// gone: a Process that took the connection and reset it was there, and asking
// the runtime about it would be a runtime call the request rate sets.
func TestAResetThatIsNotADialIsNotReResolved(t *testing.T) {
	h, fake, counted := serveCounted(t)
	port := newResettingStub(t)
	req := processRequest{
		ID:        uuid.V7(),
		Package:   "/org/sensorium",
		Name:      testProcessName,
		Container: "kitbash-reset-" + strings.Split(uuid.V7(), "-")[0],
		Digest:    testDigest,
		Expose:    ExposeHTTP,
		Endpoint:  "http://127.0.0.1:" + strconv.Itoa(port),
	}
	fake.Publish(req.Container, port)
	fake.SetState(req.Container, podman.StateRunning)
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	// The container is gone as far as the runtime is concerned, so a
	// re-resolve would drop the port: what keeps it is that this failure is
	// not a dial.
	fake.SetState(req.Container, podman.StateExited)

	counted.inspects.Store(0)
	rec := h.request(http.MethodGet, h.defaultName(testProcessName), "/", nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for a Process that took the connection and reset it", rec.Code)
	}
	if n := counted.inspects.Load(); n != 0 {
		t.Errorf("a reset that was not a dial cost %d runtime calls, want none", n)
	}
	if port, held := h.server.proxy.portOf(req.ID); !held || port == 0 {
		t.Errorf("the route is %d, %t, want it untouched by a failure that was not a dial", port, held)
	}
}

// newResettingStub takes every connection and resets it, which is what a
// container that dies mid request leaves: a read that fails with the same
// errno a refused dial does, on a connection something accepted.
func newResettingStub(t *testing.T) int {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.AcceptTCP()
			if err != nil {
				return
			}
			// Linger zero makes the close a reset rather than a shutdown, so
			// the client reads ECONNRESET off a connection that was accepted.
			conn.SetLinger(0)
			conn.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// A forward that had begun answering is neither re-resolved nor answered
// again: the status is on the wire already and a second WriteHeader is a log
// line and nothing more.
func TestAForwardThatBeganAnsweringIsNotAnsweredAgain(t *testing.T) {
	h, fake, counted := serveCounted(t)
	up := newUpstream(t)
	req := h.registerServed(fake, up, testProcessName, "")
	fake.SetState(req.Container, podman.StateExited)

	rec := httptest.NewRecorder()
	recorder := &recordingWriter{ResponseWriter: rec}
	recorder.WriteHeader(http.StatusOK)
	target, _ := h.server.proxy.lookup(h.defaultName(testProcessName))
	r := httptest.NewRequest(http.MethodGet, "http://kitbash/", nil)
	r = r.WithContext(context.WithValue(r.Context(), routeKey{}, target))

	counted.inspects.Store(0)
	h.server.forwardFailed(recorder, r, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED})
	if recorder.code() != http.StatusOK {
		t.Errorf("the recorded status is %d, want the 200 that was already written", recorder.code())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a second body of %q was written after the answer had begun", rec.Body.String())
	}
	if n := counted.inspects.Load(); n != 0 {
		t.Errorf("a forward that had begun answering cost %d runtime calls, want none", n)
	}
}
