package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/sysusers"
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
