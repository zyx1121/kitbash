package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// testDomain is the domain the proxy tests give the host, and testProcessName
// the Process name their default host name is derived from.
const (
	testDomain      = "kitbash.example"
	testProcessName = "app"
)

// upstream is one Process answering through the proxy: what it received and
// what it answers with.
type upstream struct {
	server *httptest.Server
	mu     sync.Mutex
	last   *http.Request
	status int
	body   string
}

// newUpstream serves on the loopback address, which is where a Process
// publishes its port and the only place the proxy forwards to.
func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{status: http.StatusOK, body: "the Process answered"}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.last = r.Clone(context.Background())
		status, body := u.status, u.body
		u.mu.Unlock()
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(u.server.Close)
	return u
}

// received is the request the Process was given, or nil.
func (u *upstream) received() *http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last
}

// port is the loopback port this Process publishes.
func (u *upstream) port(t *testing.T) int {
	t.Helper()
	port, ok := endpointPort(u.server.URL)
	if !ok {
		t.Fatalf("the stub URL %q names no port", u.server.URL)
	}
	return port
}

// serveProxying starts a daemon that has a domain, on a fake host: what a
// container publishes is what the routing table is built from, so it is the
// test's to stage.
func serveProxying(t *testing.T, domain, mode string) (*harness, *sysusers.Fake) {
	t.Helper()
	fake := sysusers.NewFake()
	h := serveWith(t, Options{
		Admin:   func(*user.User) (bool, error) { return false, nil },
		Users:   fake,
		Runner:  fake,
		Domain:  domain,
		TLS:     mode,
		CertDir: filepath.Join(t.TempDir(), "certs"),
	})
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid()})
	return h, fake
}

// registerServed registers one Process with expose: http whose container
// publishes the port its endpoint names, which is what the proxy forwards to.
func (h *harness) registerServed(fake *sysusers.Fake, u *upstream, name, hostname string) processRequest {
	h.t.Helper()
	req := processRequest{
		ID:        uuid.V7(),
		Package:   "/org/sensorium",
		Name:      name,
		Container: "kitbash-" + name + "-" + strings.Split(uuid.V7(), "-")[0],
		Digest:    testDigest,
		Expose:    ExposeHTTP,
		Endpoint:  u.server.URL,
		Hostname:  hostname,
	}
	fake.Publish(req.Container, u.port(h.t))
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		h.t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	return req
}

// request sends one request through the proxy, as the wire would.
func (h *harness) request(method, host, path string, headers map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	// The URL is built from the path alone and the Host is set after it: a
	// request off the wire carries the two separately, and half the point of
	// these tests is a Host that is not a URL at all.
	req := httptest.NewRequest(method, "http://kitbash"+path, nil)
	req.Host = host
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	h.server.ProxyHandler().ServeHTTP(rec, req)
	return rec
}

// defaultName is the name a Process of this test's member is served under.
func (h *harness) defaultName(name string) string {
	return name + "." + h.user + "." + testDomain
}

// ---------------------------------------------------------------- the names

// The name a Process is served under is derived from the Process, the member
// who runs it and the host's domain, or declared by the unit, see PLAN.md
// section 2.3.
func TestTheHostNameOfAProcess(t *testing.T) {
	http := store.Process{Owner: "loki", Name: "app", Expose: ExposeHTTP}
	cases := []struct {
		what    string
		process store.Process
		domain  string
		want    string
	}{
		{"an http Process is <name>.<member>.<domain>", http, testDomain, "app.loki.kitbash.example"},
		{"a host with no domain serves nothing", http, "", ""},
		{"a declared name is served instead", store.Process{
			Owner: "loki", Name: "app", Expose: ExposeHTTP, Hostname: "status.example.org",
		}, testDomain, "status.example.org"},
		{"a declared name needs a domain on the host too", store.Process{
			Owner: "loki", Name: "app", Expose: ExposeHTTP, Hostname: "status.example.org",
		}, "", ""},
		{"an mcp Process has no name", store.Process{
			Owner: "loki", Name: "app", Expose: ExposeMCP,
		}, testDomain, ""},
		{"a Process exposed as none has no name", store.Process{
			Owner: "loki", Name: "app", Expose: ExposeNone,
		}, testDomain, ""},
		{"a Process a run kit owns is not served here", store.Process{
			Owner: "loki", Name: "app", Expose: ExposeHTTP, Runner: "/org/pve-runner",
		}, testDomain, ""},
		{"a member whose account name is not a DNS label is not served", store.Process{
			Owner: "loki-", Name: "app", Expose: ExposeHTTP,
		}, testDomain, ""},
		{"a declared name that is not a name is not served", store.Process{
			Owner: "loki", Name: "app", Expose: ExposeHTTP, Hostname: "not a name",
		}, testDomain, ""},
	}
	for _, c := range cases {
		if got := hostFor(c.process, c.domain); got != c.want {
			t.Errorf("%s: hostFor = %q, want %q", c.what, got, c.want)
		}
	}
}

// The Host of a request decides which Process answers it, so what is read out
// of it is a name and nothing else.
func TestTheHostOfARequest(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		why  string
	}{
		{"app.loki.kitbash.example", "app.loki.kitbash.example", "a name"},
		{"app.loki.kitbash.example:8080", "app.loki.kitbash.example", "the port is not part of the name"},
		{"APP.Loki.Kitbash.Example", "app.loki.kitbash.example", "DNS is case insensitive"},
		{"", "", "a request with no Host names no Process"},
		{"app.loki.kitbash.example/../secret", "", "a path in the Host is header injection"},
		{"evil.example@app.loki.kitbash.example", "", "an at sign in the Host is header injection"},
		{"app.loki.kitbash.example\r\nX-Admin: 1", "", "a line break in the Host is header injection"},
		{"[::1]:80", "", "an address literal is not a name kitbash routes"},
		{"127.0.0.1:8080", "127.0.0.1", "four numeric labels read as a name; the table is what refuses it"},
	}
	for _, c := range cases {
		got, ok := hostOf(c.raw)
		if c.want == "" {
			if ok {
				t.Errorf("hostOf(%q) = %q, want a refusal: %s", c.raw, got, c.why)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("hostOf(%q) = %q, %v, want %q: %s", c.raw, got, ok, c.want, c.why)
		}
	}
}

// ------------------------------------------------------------- the routing

// The sentence of the milestone: a request for the name of a running Process
// reaches that Process.
func TestTheProxyForwardsToTheProcess(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")

	rec := h.request(http.MethodGet, h.defaultName(testProcessName), "/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); got != "the Process answered" {
		t.Errorf("body = %q, want the Process's own", got)
	}
	got := up.received()
	if got == nil {
		t.Fatal("the Process received nothing")
	}
	if got.URL.Path != "/status" {
		t.Errorf("the Process was asked for %q, want /status", got.URL.Path)
	}
	if got.Host != h.defaultName(testProcessName) {
		t.Errorf("the Process saw Host %q, want the name the request arrived under", got.Host)
	}
}

// A unit that declares a name of its own is served under it and not under the
// derived one: the member owns that name and its record points here.
func TestADeclaredHostNameIsServed(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "status.example.org")

	if rec := h.request(http.MethodGet, "status.example.org", "/", nil); rec.Code != http.StatusOK {
		t.Errorf("the declared name answered %d, want it served", rec.Code)
	}
	if rec := h.request(http.MethodGet, h.defaultName(testProcessName), "/", nil); rec.Code != http.StatusNotFound {
		t.Errorf("the derived name answered %d, want 404: a Process holds one name", rec.Code)
	}
}

// A name nobody holds never reaches a container, and what the caller is given
// is the problem class every kitbash error names.
func TestAnUnknownHostIsNotFound(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")

	rec := h.request(http.MethodGet, "nobody."+h.user+"."+testDomain, "/", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != ProblemContentType {
		t.Errorf("Content-Type = %q, want %q", ct, ProblemContentType)
	}
	var p problem.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	if want := problem.Base + problem.SlugNotFound; p.Type != want {
		t.Errorf("type = %q, want %q", p.Type, want)
	}
	if up.received() != nil {
		t.Error("a request for a name nobody holds reached a Process")
	}
}

// A Host that is not a name is refused before anything is looked up, and
// nothing reaches a Process.
func TestAHostThatIsNotANameIsRefused(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)

	for _, host := range []string{
		name + "/../etc/passwd",
		"evil.example@" + name,
		name + "\r\nX-Kitbash-User: root",
		"",
	} {
		rec := h.request(http.MethodGet, host, "/", nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("Host %q answered %d, want 400", host, rec.Code)
		}
		if body := rec.Body.String(); strings.Contains(body, "@") || strings.Contains(body, "passwd") {
			t.Errorf("Host %q was repeated back in %q; a refused Host is not echoed", host, body)
		}
	}
	if up.received() != nil {
		t.Error("a request whose Host is not a name reached a Process")
	}
}

// A Process that is registered and not running answers 503 on its own name:
// what a member reads at the address of a Process they stopped is that it is
// not running, not that the address is nobody's.
func TestAStoppedProcessIsUnavailable(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	req := h.registerServed(fake, up, testProcessName, "")

	// The container is gone, which is what stopping and removing one leaves.
	fake.Publish(req.Container)
	h.server.LoadRoutes(context.Background())

	rec := h.request(http.MethodGet, h.defaultName(testProcessName), "/", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "not running") {
		t.Errorf("body = %q, want a short answer saying the Process is not running", body)
	}
	if up.received() != nil {
		t.Error("a request for a stopped Process reached one")
	}
}

// The guard this milestone turns on: the port is the runtime's answer and
// never the member's word. A registration naming a port its own container does
// not publish is not forwarded to, whatever is listening there.
func TestOnlyAPortTheContainerPublishesIsForwardedTo(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSGateway)
	neighbour := newUpstream(t)

	// The registration names the neighbour's port, and this Process's own
	// container publishes something else entirely.
	req := processRequest{
		ID:        uuid.V7(),
		Package:   "/org/sensorium",
		Name:      testProcessName,
		Container: "kitbash-app-liar",
		Digest:    testDigest,
		Expose:    ExposeHTTP,
		Endpoint:  neighbour.server.URL,
	}
	fake.Publish(req.Container, neighbour.port(t)+1)
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}

	rec := h.request(http.MethodGet, h.defaultName(testProcessName), "/", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503: a port the container does not publish is not forwarded to", rec.Code)
	}
	if neighbour.received() != nil {
		t.Error("the proxy forwarded to a port the Process's own container does not publish")
	}
}

// ------------------------------------------------------------- the headers

// In acme mode kitbashd terminated TLS itself, so the three forwarded headers
// are what it says about the request and not what the client claimed.
func TestForwardedHeadersAreOverwritten(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSACME)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)

	rec := h.request(http.MethodGet, name, "/", map[string]string{
		"X-Forwarded-For":   "203.0.113.9",
		"X-Forwarded-Proto": "http",
		"X-Forwarded-Host":  "bank.example.org",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := up.received()
	if got == nil {
		t.Fatal("the Process received nothing")
	}
	if forwarded := got.Header.Get("X-Forwarded-For"); strings.Contains(forwarded, "203.0.113.9") {
		t.Errorf("X-Forwarded-For = %q, want the peer's own address: a client does not write its own chain", forwarded)
	}
	if proto := got.Header.Get("X-Forwarded-Proto"); proto != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want https: this daemon terminated TLS", proto)
	}
	if host := got.Header.Get("X-Forwarded-Host"); host != name {
		t.Errorf("X-Forwarded-Host = %q, want %q", host, name)
	}
}

// In gateway mode the gateway is the one thing that knows whether the member
// was on https, so its X-Forwarded-Proto is passed on, and nothing else is.
func TestGatewayModeTakesTheProtoFromTheGateway(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)

	cases := []struct{ sent, want, why string }{
		{"http", "http", "the gateway says the member was on plain http"},
		{"https", "https", "the gateway says the member was on https"},
		{"", "https", "a gateway that says nothing terminated TLS, which is what this mode is"},
		{"gopher", "https", "a value that is not a scheme is not passed on"},
		{"https, http", "https", "a list is not a scheme"},
	}
	for _, c := range cases {
		headers := map[string]string{"X-Forwarded-For": "203.0.113.9"}
		if c.sent != "" {
			headers["X-Forwarded-Proto"] = c.sent
		}
		if rec := h.request(http.MethodGet, name, "/", headers); rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		got := up.received()
		if proto := got.Header.Get("X-Forwarded-Proto"); proto != c.want {
			t.Errorf("X-Forwarded-Proto %q became %q, want %q: %s", c.sent, proto, c.want, c.why)
		}
		if forwarded := got.Header.Get("X-Forwarded-For"); strings.Contains(forwarded, "203.0.113.9") {
			t.Errorf("X-Forwarded-For = %q, want the peer's own address even in gateway mode", forwarded)
		}
	}
}

// ---------------------------------------------------------- the registration

// A name is one Process's. A registration declaring one another Process on
// this host already holds is a conflict, see PLAN.md section 2.3.
func TestADeclaredHostNameAnotherProcessHoldsIsAConflict(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "status.example.org")

	second := processRequest{
		ID:        uuid.V7(),
		Package:   "/org/other",
		Name:      "other",
		Container: "kitbash-other-other",
		Digest:    testDigest,
		Expose:    ExposeHTTP,
		Endpoint:  up.server.URL,
		Hostname:  "status.example.org",
	}
	_, res, body := h.register(second)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, body %s, want 409", res.StatusCode, body)
	}
	if p := h.problemOf(res, body); p.Slug() != problem.SlugConflict {
		t.Errorf("slug = %q, want %q", p.Slug(), problem.SlugConflict)
	}
	// And the row was not written: a refused registration leaves nothing.
	if _, found, err := h.store.Process(context.Background(), second.ID); err != nil || found {
		t.Errorf("the refused registration was stored: found %v, err %v", found, err)
	}
}

// The derived name of another member's Process is a name that is already
// held too, so declaring it is the same conflict.
func TestADeclaredHostNameThatIsAnotherProcessDerivedNameIsAConflict(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	held := h.registerServed(fake, up, testProcessName, "")

	second := processRequest{
		ID:        uuid.V7(),
		Package:   "/org/other",
		Name:      "other",
		Container: "kitbash-other-other",
		Digest:    testDigest,
		Expose:    ExposeHTTP,
		Endpoint:  up.server.URL,
		Hostname:  h.defaultName(held.Name),
	}
	_, res, body := h.register(second)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, body %s, want 409", res.StatusCode, body)
	}
}

// Registering the same Process again keeps its own name: a re-run is not a
// second claimant.
func TestARegistrationKeepsItsOwnDeclaredName(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	req := h.registerServed(fake, up, testProcessName, "status.example.org")

	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("re-registering answered %d, body %s", res.StatusCode, body)
	}
}

// A host name is refused before it is stored when it is not a name, when the
// unit is not exposed over HTTP, and when a run kit owns the Process.
func TestAHostNameThatKitbashdCannotServeIsRefused(t *testing.T) {
	h, _ := serveProxying(t, testDomain, TLSGateway)
	cases := []struct {
		what   string
		change func(*processRequest)
		status int
	}{
		{"a name that is not one", func(r *processRequest) { r.Hostname = "not a name" }, http.StatusBadRequest},
		{"a name with a path in it", func(r *processRequest) { r.Hostname = "app.example/admin" }, http.StatusBadRequest},
		{"a bare label", func(r *processRequest) { r.Hostname = "app" }, http.StatusBadRequest},
		{"an exposure with nothing to reach", func(r *processRequest) {
			r.Expose = ExposeMCP
			r.Endpoint = ""
			r.Hostname = "status.example.org"
		}, http.StatusBadRequest},
		{"a Process a run kit owns", func(r *processRequest) {
			r.Runner = "/org/pve-runner"
			r.Container = ""
			r.Hostname = "status.example.org"
		}, http.StatusForbidden},
	}
	for _, c := range cases {
		req := processRequest{
			ID: uuid.V7(), Package: "/org/sensorium", Name: testProcessName,
			Expose: ExposeHTTP, Endpoint: "http://127.0.0.1:40275",
			Container: "kitbash-app-app", Digest: testDigest,
		}
		c.change(&req)
		_, res, body := h.register(req)
		if res.StatusCode != c.status {
			t.Errorf("%s: status = %d, want %d, body %s", c.what, res.StatusCode, c.status, body)
		}
	}
}

// The registration answers the name kitbashd assigned, and the listing carries
// it, which is where proc_run and proc_list read the url from.
func TestTheRegistrationAndTheListingCarryTheHost(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	req := processRequest{
		ID: uuid.V7(), Package: "/org/sensorium", Name: testProcessName,
		Container: "kitbash-app-app", Digest: testDigest,
		Expose: ExposeHTTP, Endpoint: up.server.URL,
	}
	fake.Publish(req.Container, up.port(t))
	_, res, body := h.register(req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	var answer processResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if answer.Host != h.defaultName(testProcessName) {
		t.Errorf("host = %q, want %q", answer.Host, h.defaultName(testProcessName))
	}

	res, body = h.do(http.MethodGet, processesPath, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, body %s", res.StatusCode, body)
	}
	var listed processList
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(listed.Processes) != 1 || listed.Processes[0].Host != h.defaultName(testProcessName) {
		t.Errorf("the listing carries %+v, want the host name", listed.Processes)
	}
}

// A host with no domain answers no name at all: an http Process keeps its
// internal port, which is how kitbash worked before the proxy existed.
func TestAHostWithNoDomainServesNothing(t *testing.T) {
	h, fake := serveProxying(t, "", TLSACME)
	up := newUpstream(t)
	req := h.registerServed(fake, up, testProcessName, "")

	res, body := h.do(http.MethodGet, processesPath, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, body %s", res.StatusCode, body)
	}
	var listed processList
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(listed.Processes) != 1 || listed.Processes[0].Host != "" {
		t.Errorf("the listing carries a host on a domainless host: %+v", listed.Processes)
	}
	if h.server.proxy.count() != 0 {
		t.Errorf("the routing table holds %d names on a host with no domain", h.server.proxy.count())
	}
	rec := h.request(http.MethodGet, req.Name+"."+h.user+".kitbash.example", "/", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 on a host that routes nothing", rec.Code)
	}
	if up.received() != nil {
		t.Error("a host with no domain forwarded a request")
	}
}

// ---------------------------------------------------------------- the table

// The names travel with the registration, so the table is rebuilt from the
// store at every start: a Process that was reachable before a reboot is
// reachable after one, see PLAN.md section 2.3.
func TestRestoreRebuildsTheRoutingTable(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)

	// A registration written straight to the store, which is what a daemon
	// that has just started finds.
	container := "kitbash-app-restored"
	fake.Publish(container, up.port(t))
	_, hash, err := store.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	id := uuid.V7()
	if err := h.store.RegisterProcess(context.Background(), store.Process{
		ID: id, Owner: h.user, Package: "/org/sensorium", Name: testProcessName,
		Container: container, Expose: ExposeHTTP, Endpoint: up.server.URL,
		Hostname: "status.example.org", RegisteredAt: time.Now().UTC(),
	}, hash, 0); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}
	if h.server.proxy.count() != 0 {
		t.Fatalf("the table holds %d names before the restore", h.server.proxy.count())
	}

	h.server.Restore(context.Background())

	if rec := h.request(http.MethodGet, "status.example.org", "/", nil); rec.Code != http.StatusOK {
		t.Errorf("the restored name answered %d, want it served again", rec.Code)
	}
	if up.received() == nil {
		t.Error("the restored name did not reach the Process")
	}
}

// Unregistering a Process takes its name with it: a name is served while a
// Process is registered and not a moment longer.
func TestUnregisteringTakesTheNameOffTheTable(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	req := h.registerServed(fake, up, testProcessName, "")

	res, body := h.do(http.MethodDelete, processesPath+"/"+req.ID, "", nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("unregister status = %d, body %s", res.StatusCode, body)
	}
	if rec := h.request(http.MethodGet, h.defaultName(testProcessName), "/", nil); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 once the Process is unregistered", rec.Code)
	}
}

// A Process that moves from one name to another leaves no entry behind.
func TestANewNameReplacesTheOldOne(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	req := h.registerServed(fake, up, testProcessName, "")

	req.Hostname = "status.example.org"
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	if rec := h.request(http.MethodGet, "status.example.org", "/", nil); rec.Code != http.StatusOK {
		t.Errorf("the new name answered %d, want it served", rec.Code)
	}
	if rec := h.request(http.MethodGet, h.defaultName(testProcessName), "/", nil); rec.Code != http.StatusNotFound {
		t.Errorf("the old name answered %d, want 404: a Process holds one name", rec.Code)
	}
}

// The certificate policy is the routing table: a name nobody registered costs
// this host nothing at the certificate authority.
func TestCertificatesAreObtainedForServedNamesOnly(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSACME)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")
	if h.server.proxy.certs == nil {
		t.Fatal("a host in acme mode has no certificate client")
	}
	policy := h.server.proxy.certs.HostPolicy
	if err := policy(context.Background(), h.defaultName(testProcessName)); err != nil {
		t.Errorf("a served name was refused a certificate: %v", err)
	}
	for _, name := range []string{"nobody." + h.user + "." + testDomain, "bank.example.org", "not a name"} {
		if err := policy(context.Background(), name); err == nil {
			t.Errorf("%q was allowed a certificate and no Process is served there", name)
		}
	}
}

// ------------------------------------------------------------- the telemetry

// One forwarded request is one span with the four attributes, which is what
// makes "what was requested of my Processes" a tel_query, see PLAN.md 2.4.
func TestAForwardedRequestWritesOneSpan(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	req := h.registerServed(fake, up, testProcessName, "")

	if rec := h.request(http.MethodPost, h.defaultName(testProcessName), "/upload", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	spans := h.proxySpans()
	if len(spans) != 1 {
		t.Fatalf("the store holds %d proxy spans, want one per forwarded request", len(spans))
	}
	span := spans[0]
	if span.Attributes.User != h.user || span.Attributes.Package != req.Package ||
		span.Attributes.Process != req.ID || span.Attributes.Path != "/upload" {
		t.Errorf("the span carries %+v, want the four attributes of this Process", span.Attributes)
	}
	if span.Attributes.Producer != InternalProducer {
		t.Errorf("producer = %q, want %q: the daemon forwarded it", span.Attributes.Producer, InternalProducer)
	}
	if got := span.Attributes.Other[AttrRequestMethod]; got != http.MethodPost {
		t.Errorf("%s = %v, want POST", AttrRequestMethod, got)
	}
	if got := fmt.Sprint(span.Attributes.Other[AttrResponseCode]); got != "200" {
		t.Errorf("%s = %v, want 200", AttrResponseCode, got)
	}
	if got := span.Attributes.Other[AttrURLPath]; got != "/upload" {
		t.Errorf("%s = %v, want /upload", AttrURLPath, got)
	}
	if got := span.Attributes.Other[AttrProxyHost]; got != h.defaultName(testProcessName) {
		t.Errorf("%s = %v, want the name the request arrived under", AttrProxyHost, got)
	}
	if len(span.TraceID) != traceIDBytes*2 || len(span.SpanID) != spanIDBytes*2 {
		t.Errorf("the span is identified by %q and %q, want a trace id and a span id", span.TraceID, span.SpanID)
	}
}

// A request that arrives inside a trace is recorded in that trace rather than
// in one of its own.
func TestAForwardedRequestJoinsTheCallersTrace(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")
	const (
		trace  = "4bf92f3577b34da6a3ce929d0e0e4736"
		parent = "00f067aa0ba902b7"
	)

	h.request(http.MethodGet, h.defaultName(testProcessName), "/", map[string]string{
		"traceparent": "00-" + trace + "-" + parent + "-01",
	})

	spans := h.proxySpans()
	if len(spans) != 1 {
		t.Fatalf("the store holds %d proxy spans", len(spans))
	}
	if spans[0].TraceID != trace || spans[0].ParentSpanID != parent {
		t.Errorf("the span is %s/%s, want the caller's trace and span", spans[0].TraceID, spans[0].ParentSpanID)
	}
	if spans[0].SpanID == parent {
		t.Error("the span took the caller's span id rather than one of its own")
	}
}

// A traceparent this daemon cannot read is a trace of its own rather than a
// span with a parent nothing finds.
func TestAnUnreadableTraceparentIsIgnored(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")

	for _, header := range []string{
		"", "nonsense", "00-tooshort-00f067aa0ba902b7-01",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"99-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	} {
		h.request(http.MethodGet, h.defaultName(testProcessName), "/", map[string]string{"traceparent": header})
	}
	for _, span := range h.proxySpans() {
		if span.ParentSpanID != "" {
			t.Errorf("traceparent was read out of a header this daemon cannot parse: parent %q", span.ParentSpanID)
		}
		if len(span.TraceID) != traceIDBytes*2 {
			t.Errorf("the span carries the trace id %q", span.TraceID)
		}
	}
}

// proxySpans is every span one forwarded request wrote.
func (h *harness) proxySpans() []store.Span {
	h.t.Helper()
	page, err := h.store.Query(context.Background(), store.SignalTraces, store.Filter{})
	if err != nil {
		h.t.Fatalf("Query: %v", err)
	}
	var out []store.Span
	for _, span := range page.Spans {
		if span.Name == ProxySpan {
			out = append(out, span)
		}
	}
	return out
}

// ------------------------------------------------------------ the upgrade

// An http Process serves what it likes over one connection, which includes a
// WebSocket: the proxy hands the connection over rather than reading it.
func TestTheProxyHandsOverAWebSocketUpgrade(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSGateway)

	// A Process that answers 101 and echoes whatever follows, which is what
	// every WebSocket library does under its framing.
	echoed := make(chan string, 1)
	socket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "not an upgrade", http.StatusBadRequest)
			return
		}
		conn, reader, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("the Process could not take the connection: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		line, _ := reader.ReadString('\n')
		echoed <- strings.TrimSpace(line)
		fmt.Fprint(conn, "pong\n")
	}))
	t.Cleanup(socket.Close)

	port, _ := endpointPort(socket.URL)
	container := "kitbash-app-socket"
	fake.Publish(container, port)
	req := processRequest{
		ID: uuid.V7(), Package: "/org/sensorium", Name: testProcessName,
		Container: container, Digest: testDigest, Expose: ExposeHTTP, Endpoint: socket.URL,
	}
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}

	// The proxy is served over a real listener, because an upgrade is a
	// connection and not a response.
	front := httptest.NewServer(h.server.ProxyHandler())
	t.Cleanup(front.Close)
	address := strings.TrimPrefix(front.URL, "http://")
	conn, err := net.DialTimeout("tcp", address, waitBudget)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	defer conn.Close()
	name := h.defaultName(testProcessName)
	fmt.Fprintf(conn, "GET /socket HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", name)
	conn.SetDeadline(time.Now().Add(waitBudget))
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("the proxy answered %q, want 101 Switching Protocols", strings.TrimSpace(status))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read the headers: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	fmt.Fprint(conn, "ping\n")
	if got := <-echoed; got != "ping" {
		t.Errorf("the Process read %q over the upgraded connection, want ping", got)
	}
	answer, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read what the Process sent: %v", err)
	}
	if strings.TrimSpace(answer) != "pong" {
		t.Errorf("the connection carried %q back, want pong", strings.TrimSpace(answer))
	}
}

// ------------------------------------------------------------- the redirect

// In acme mode this daemon holds the certificates, so plain HTTP is a redirect
// and not a way to reach a Process without TLS.
func TestPlainHTTPRedirectsInACMEMode(t *testing.T) {
	h, fake := serveProxying(t, testDomain, TLSACME)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://"+name+"/status?q=1", nil)
	req.Host = name
	http.HandlerFunc(redirectToHTTPS).ServeHTTP(rec, req)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", rec.Code)
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location %q: %v", rec.Header().Get("Location"), err)
	}
	if location.Scheme != "https" || location.Host != name || location.Path != "/status" ||
		location.RawQuery != "q=1" {
		t.Errorf("Location = %q, want the same request over https", location)
	}
	if up.received() != nil {
		t.Error("a plain HTTP request reached a Process in acme mode")
	}
}
