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

	"github.com/zyx1121/kitbash/internal/podman"
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

// testPublicAddress is the address this host answers on in the proxy tests,
// which is what a declared host name is proved against.
const testPublicAddress = "203.0.113.9"

// zone is the resolver the proxy tests run against: what a name points at is
// the test's to say, and no test asks the internet. A name nothing was staged
// for resolves to this host, so a test about something else is not a test
// about DNS; a test about the proof stages its own answers.
type zone struct {
	mu      sync.Mutex
	addrs   map[string][]string
	cnames  map[string]string
	missing map[string]bool
	lookups int
}

func newZone() *zone {
	return &zone{addrs: map[string][]string{}, cnames: map[string]string{}, missing: map[string]bool{}}
}

// points stages what a name resolves to.
func (z *zone) points(name string, addresses ...string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.addrs[name] = addresses
}

// aliases stages a name as a CNAME of another.
func (z *zone) aliases(name, canonical string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.cnames[name] = canonical
}

// unknown stages a name nothing answers for.
func (z *zone) unknown(name string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.missing[name] = true
}

func (z *zone) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.lookups++
	if z.missing[host] {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	addresses, staged := z.addrs[host]
	if !staged {
		addresses = []string{testPublicAddress}
	}
	out := make([]net.IPAddr, 0, len(addresses))
	for _, address := range addresses {
		out = append(out, net.IPAddr{IP: net.ParseIP(address)})
	}
	return out, nil
}

func (z *zone) LookupCNAME(_ context.Context, host string) (string, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if canonical, staged := z.cnames[host]; staged {
		return canonical + ".", nil
	}
	// The system resolver answers the name itself when there is no alias.
	return host + ".", nil
}

// serveProxying starts a daemon that has a domain, on a fake host: what a
// container publishes is what the routing table is built from, and what a name
// resolves to is what a declared host name is proved against, so both are the
// test's to stage.
func serveProxying(t *testing.T, domain, mode string) (*harness, *sysusers.Fake, *zone) {
	t.Helper()
	return serveProxyingAs(t, domain, mode, false)
}

// serveProxyingAs is serveProxying with the caller's admin answer named, which
// is what users_remove needs.
func serveProxyingAs(t *testing.T, domain, mode string, admin bool) (*harness, *sysusers.Fake, *zone) {
	t.Helper()
	fake := sysusers.NewFake()
	names := newZone()
	h := serveWith(t, Options{
		Admin:         func(*user.User) (bool, error) { return admin, nil },
		Users:         fake,
		Runner:        fake,
		Domain:        domain,
		TLS:           mode,
		CertDir:       filepath.Join(t.TempDir(), "certs"),
		PublicAddress: testPublicAddress,
		Resolver:      names,
	})
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid()})
	return h, fake, names
}

// servedRequest is one registration with expose: http whose container
// publishes the port its endpoint names, which is what the proxy forwards to.
// A test that declares something else on it builds the request here and sends
// it itself.
func (h *harness) servedRequest(fake *sysusers.Fake, u *upstream, name string) processRequest {
	h.t.Helper()
	req := processRequest{
		ID:        uuid.V7(),
		Package:   "/org/sensorium",
		Name:      name,
		Container: "kitbash-" + name + "-" + strings.Split(uuid.V7(), "-")[0],
		Digest:    testDigest,
		Expose:    ExposeHTTP,
		Endpoint:  u.server.URL,
	}
	fake.Publish(req.Container, u.port(h.t))
	// A published port is not enough: what the proxy forwards to is a running
	// container, because a published port is creation configuration and
	// survives a stop, see routePort.
	fake.SetState(req.Container, podman.StateRunning)
	return req
}

// registerServed registers one such Process.
func (h *harness) registerServed(fake *sysusers.Fake, u *upstream, name, hostname string) processRequest {
	h.t.Helper()
	req := h.servedRequest(fake, u, name)
	req.Hostname = hostname
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		h.t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	return req
}

// registeredHost registers one Process and answers the name kitbashd said it
// serves it under, which is what proc_run turns into url.
func (h *harness) registeredHost(req processRequest) string {
	h.t.Helper()
	_, res, body := h.register(req)
	if res.StatusCode != http.StatusOK {
		h.t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	var got processResponse
	if err := json.Unmarshal(body, &got); err != nil {
		h.t.Fatalf("body %q: %v", body, err)
	}
	return got.Host
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
		{"a fan out receiver is not served at all", store.Process{
			Owner: "loki", Name: "observe-count", Expose: ExposeHTTP,
			Subscriptions: []string{store.SubscriptionTelemetry},
		}, testDomain, ""},
		{"and a name it declared is not served either", store.Process{
			Owner: "loki", Name: "observe-count", Expose: ExposeHTTP,
			Hostname: "records.example.org", Subscriptions: []string{store.SubscriptionTelemetry},
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
		{"127.0.0.1:8080", "", "four numeric labels are a name by shape, and an address is not a name kitbash routes"},
		{"192.168.1.1", "", "an address is refused whether or not it carries a port"},
		{"app.loki.kitbash.example.", "app.loki.kitbash.example", "one trailing dot is the root, which is the same name"},
		{"app.loki.kitbash.example.:443", "app.loki.kitbash.example", "the root and a port together"},
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
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
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
	h, fake, names := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	// The member pointed their own name at this host, which is what makes it
	// theirs to declare.
	names.points("status.example.org", testPublicAddress)
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
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
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
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
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

// A Process whose container publishes nothing at all answers 503 on its own
// name: what a member reads at the address of a Process that is not up is that
// it is not running, not that the address is nobody's. The stop path is below,
// where the container keeps its ports and loses its state.
func TestAProcessWithNoPublishedPortIsUnavailable(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	req := h.registerServed(fake, up, testProcessName, "")

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
		t.Error("a request for a Process that publishes nothing reached one")
	}
}

// The guard this milestone turns on: the port is the runtime's answer and
// never the member's word. A registration naming a port its own container does
// not publish is not forwarded to, whatever is listening there.
func TestOnlyAPortTheContainerPublishesIsForwardedTo(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
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
	h, fake, _ := serveProxying(t, testDomain, TLSACME)
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
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)

	cases := []struct{ sent, want, why string }{
		{"http", "http", "the gateway says the member was on plain http"},
		{"https", "https", "the gateway says the member was on https"},
		{"", "http", "a gateway that says nothing has said nothing, and plain HTTP is what arrived"},
		{"gopher", "http", "a value that is not a scheme is not passed on"},
		{"https, http", "http", "a list is not a scheme"},
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
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
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

// Under the host's domain there are derived names and nothing else. A unit
// that declares one there is refused before anything else is asked, so no
// member takes the operator's apex, a name beside it, or the namespace of a
// member who has not run a Process yet, see PLAN.md section 2.3.
func TestADeclaredHostNameUnderTheDomainIsRefused(t *testing.T) {
	claims := []struct{ name, why string }{
		{testDomain, "the operator's apex"},
		{"admin." + testDomain, "a name beside every member's"},
		{"app.bob." + testDomain, "another member's namespace, whether or not bob has run anything"},
		{"deep.app.bob." + testDomain, "anything at all under the domain"},
	}
	for _, claim := range claims {
		h, fake, names := serveProxying(t, testDomain, TLSACME)
		up := newUpstream(t)
		// The claimant even points the name at this host, which proves
		// nothing: under the domain the name is not theirs to declare.
		names.points(claim.name, testPublicAddress)
		req := processRequest{
			ID: uuid.V7(), Package: "/org/sensorium", Name: testProcessName,
			Container: "kitbash-app-claim", Digest: testDigest,
			Expose: ExposeHTTP, Endpoint: up.server.URL, Hostname: claim.name,
		}
		fake.Publish(req.Container, up.port(t))
		fake.SetState(req.Container, podman.StateRunning)
		_, res, body := h.register(req)
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s (%s): status = %d, want 422, body %s", claim.name, claim.why, res.StatusCode, body)
			continue
		}
		if p := h.problemOf(res, body); p.Slug() != problem.SlugInvalidManifest {
			t.Errorf("%s: slug = %q, want %q", claim.name, p.Slug(), problem.SlugInvalidManifest)
		}
		if rec := h.request(http.MethodGet, claim.name, "/", nil); rec.Code == http.StatusOK {
			t.Errorf("%s: the refused name is served anyway", claim.name)
		}
		if h.server.proxy.certs != nil {
			if err := h.server.proxy.certs.HostPolicy(context.Background(), claim.name); err == nil {
				t.Errorf("%s: a certificate would be obtained for a name nobody holds", claim.name)
			}
		}
	}
}

// An address is not a name: nothing resolves it here, so a unit that declares
// one is asking for something this proxy cannot serve. Four numeric labels are
// a name by shape, so they are refused by the rule rather than by the pattern;
// an address with colons in it is not even a name, and is refused a step
// earlier as the shape it is not, which is why this names the status.
func TestADeclaredHostNameThatIsAnAddressIsRefused(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	for claim, want := range map[string]int{
		"127.0.0.1":   http.StatusUnprocessableEntity,
		"203.0.113.9": http.StatusUnprocessableEntity,
		"::1":         http.StatusBadRequest,
		"2001:db8::1": http.StatusBadRequest,
	} {
		req := processRequest{
			ID: uuid.V7(), Package: "/org/sensorium", Name: testProcessName,
			Container: "kitbash-app-claim", Digest: testDigest,
			Expose: ExposeHTTP, Endpoint: up.server.URL, Hostname: claim,
		}
		fake.Publish(req.Container, up.port(t))
		fake.SetState(req.Container, podman.StateRunning)
		_, res, body := h.register(req)
		if res.StatusCode != want {
			t.Errorf("%s: status = %d, want %d, body %s", claim, res.StatusCode, want, body)
		}
	}
}

// Registering the same Process again keeps its own name: a re-run is not a
// second claimant.
func TestARegistrationKeepsItsOwnDeclaredName(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	req := h.registerServed(fake, up, testProcessName, "status.example.org")

	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("re-registering answered %d, body %s", res.StatusCode, body)
	}
}

// A host name is refused before it is stored when it is not a name, when the
// unit is not exposed over HTTP, and when a run kit owns the Process.
func TestAHostNameThatKitbashdCannotServeIsRefused(t *testing.T) {
	h, _, _ := serveProxying(t, testDomain, TLSGateway)
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
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
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
	h, fake, _ := serveProxying(t, "", TLSACME)
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
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
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
	}, hash, store.Quota{}); err != nil {
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
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
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
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
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
	h, fake, _ := serveProxying(t, testDomain, TLSACME)
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
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
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

// A traceparent off the internet is a stranger's claim: adopting it would let
// whoever sends one write their trace id into a member's Telemetry and join
// their requests to somebody else's trace. The span starts a trace of its own
// and the header is recorded as the claim it is.
func TestAForwardedRequestDoesNotAdoptTheCallersTrace(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
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
	if spans[0].TraceID == trace {
		t.Errorf("the span took the client's trace id %s, which is a stranger writing into a member's Telemetry", trace)
	}
	if spans[0].ParentSpanID != "" {
		t.Errorf("the span has the parent %s, want a trace of its own", spans[0].ParentSpanID)
	}
	if spans[0].SpanID == parent {
		t.Error("the span took the client's span id rather than one of its own")
	}
	// The claim is recorded, so a member whose own client sent one can still
	// follow it.
	want := "00-" + trace + "-" + parent + "-01"
	if got := spans[0].Attributes.Other[AttrClientTraceparent]; got != want {
		t.Errorf("%s = %v, want the header as it arrived", AttrClientTraceparent, got)
	}
}

// The claim is bounded: a header is not a place for a client to put bytes by
// the kilobyte.
func TestAClientTraceparentIsBounded(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")

	h.request(http.MethodGet, h.defaultName(testProcessName), "/", map[string]string{
		"traceparent": strings.Repeat("a", 4096),
	})
	spans := h.proxySpans()
	if len(spans) != 1 {
		t.Fatalf("the store holds %d proxy spans", len(spans))
	}
	got, _ := spans[0].Attributes.Other[AttrClientTraceparent].(string)
	if len(got) != MaxClientTraceparent {
		t.Errorf("the recorded claim is %d bytes, want it cut to %d", len(got), MaxClientTraceparent)
	}
}

// A traceparent this daemon cannot read is a trace of its own rather than a
// span with a parent nothing finds.
func TestAnUnreadableTraceparentIsIgnored(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
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
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)

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
	fake.SetState(container, podman.StateRunning)
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
	h, fake, _ := serveProxying(t, testDomain, TLSACME)
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

// ------------------------------------------------------- the member goes

// A member's names go with their account. What is left behind otherwise is a
// name that answers for a member this host no longer has, forwarding to a host
// port their container has freed, which is a port the next member's container
// may be given.
func TestRemovingAMemberTakesTheirNamesOffTheTable(t *testing.T) {
	h, fake, _ := serveProxyingAs(t, testDomain, TLSGateway, true)
	up := newUpstream(t)
	fake.Add(sysusers.Member{Name: "alice", UID: os.Getuid(), GID: os.Getgid()})

	container := "kitbash-app-alice"
	fake.Publish(container, up.port(t))
	fake.SetState(container, podman.StateRunning)
	_, hash, err := store.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if err := h.store.RegisterProcess(context.Background(), store.Process{
		ID: uuid.V7(), Owner: "alice", Package: "/org/sensorium", Name: testProcessName,
		Container: container, Digest: testDigest, Expose: ExposeHTTP,
		Endpoint: up.server.URL, RegisteredAt: time.Now().UTC(),
	}, hash, store.Quota{}); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}
	h.server.LoadRoutes(context.Background())
	name := testProcessName + ".alice." + testDomain
	if rec := h.request(http.MethodGet, name, "/", nil); rec.Code != http.StatusOK {
		t.Fatalf("before the removal %s answered %d, want it served", name, rec.Code)
	}

	h.removeMember("alice")
	if left, held := h.server.proxy.lookup(name); held {
		t.Errorf("%s is still routed to 127.0.0.1:%d after the member was removed", name, left.port)
	}
	if n := h.server.proxy.count(); n != 0 {
		t.Errorf("the routing table holds %d name(s) after the only member with one was removed", n)
	}
	before := up.received()
	rec := h.request(http.MethodGet, name, "/", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 once the member is gone", rec.Code)
	}
	if up.received() != before {
		t.Error("a removed member's name still reached a Process")
	}
}

// ------------------------------------------------------- the stopped port

// What a container publishes is the configuration it was created with and it
// survives a stop, so the published port alone would keep forwarding a name to
// a host port nothing is bound to any more. The state is what says the Process
// is down, and a name whose Process is down answers 503.
func TestAStoppedProcessIsUnavailable(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	req := h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)
	if rec := h.request(http.MethodGet, name, "/", nil); rec.Code != http.StatusOK {
		t.Fatalf("before the stop %s answered %d, want it served", name, rec.Code)
	}

	// Through the daemon's own handler, which is what proc_stop calls.
	res, body := h.do(http.MethodPost, processesPath+"/"+req.ID+"/stop", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d, body %s", res.StatusCode, body)
	}
	// The runtime still reports the port: that is the point of this test.
	config, err := fake.ContainerConfig(context.Background(), sysusers.Member{Name: h.user}, req.Container)
	if err != nil {
		t.Fatalf("ContainerConfig: %v", err)
	}
	if !publishes(config, up.port(t)) {
		t.Fatal("the fake dropped the published port on a stop, which podman does not")
	}

	left, held := h.server.proxy.lookup(name)
	if !held {
		t.Fatal("the name is gone entirely after a stop, want it held and answering 503")
	}
	if left.port != 0 {
		t.Errorf("the route still carries port %d after the stop, so the name forwards to a port the container no longer binds", left.port)
	}
	before := up.received()
	rec := h.request(http.MethodGet, name, "/", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if up.received() != before {
		t.Error("a request for a stopped Process reached one")
	}
}

// The four step start leaves a container prepared, with its ports in its
// configuration and its entrypoint not yet run. That is not a Process to
// forward to either.
func TestAPreparedProcessIsUnavailable(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	req := h.registerServed(fake, up, testProcessName, "")

	for _, state := range []string{podman.StateInitialized, podman.StateCreated, podman.StateExited, podman.StatePaused} {
		fake.SetState(req.Container, state)
		h.server.LoadRoutes(context.Background())
		rec := h.request(http.MethodGet, h.defaultName(testProcessName), "/", nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("a container the runtime calls %s answered %d, want 503", state, rec.Code)
		}
	}
}

// ------------------------------------------------------ the name points here

// What makes a declared name the member's is that it points at this host.
func TestADeclaredHostNameIsProvedAgainstTheRecord(t *testing.T) {
	cases := []struct {
		what   string
		stage  func(h *harness, z *zone)
		served bool
	}{
		{"an A record of this host", func(h *harness, z *zone) {
			z.points("status.example.org", testPublicAddress)
		}, true},
		{"one of several addresses", func(h *harness, z *zone) {
			z.points("status.example.org", "198.51.100.7", testPublicAddress)
		}, true},
		{"a CNAME to the name kitbash derived", func(h *harness, z *zone) {
			z.aliases("status.example.org", h.defaultName(testProcessName))
			z.points("status.example.org", "198.51.100.7")
		}, true},
		{"a record pointing somewhere else", func(h *harness, z *zone) {
			z.points("status.example.org", "198.51.100.7")
		}, false},
		{"a CNAME to somebody else's name", func(h *harness, z *zone) {
			z.aliases("status.example.org", "app.bob."+testDomain)
			z.points("status.example.org", "198.51.100.7")
		}, false},
		{"a name that does not resolve", func(h *harness, z *zone) {
			z.unknown("status.example.org")
		}, false},
	}
	for _, c := range cases {
		h, fake, names := serveProxying(t, testDomain, TLSGateway)
		up := newUpstream(t)
		c.stage(h, names)
		req := processRequest{
			ID: uuid.V7(), Package: "/org/sensorium", Name: testProcessName,
			Container: "kitbash-app-proof", Digest: testDigest,
			Expose: ExposeHTTP, Endpoint: up.server.URL, Hostname: "status.example.org",
		}
		fake.Publish(req.Container, up.port(t))
		fake.SetState(req.Container, podman.StateRunning)
		_, res, body := h.register(req)
		if c.served {
			if res.StatusCode != http.StatusOK {
				t.Errorf("%s: status = %d, want it registered, body %s", c.what, res.StatusCode, body)
				continue
			}
			if rec := h.request(http.MethodGet, "status.example.org", "/", nil); rec.Code != http.StatusOK {
				t.Errorf("%s: the name answered %d, want it served", c.what, rec.Code)
			}
			continue
		}
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403, body %s", c.what, res.StatusCode, body)
			continue
		}
		if p := h.problemOf(res, body); !strings.Contains(p.Detail, "status.example.org") {
			t.Errorf("%s: the problem does not name the hostname: %s", c.what, p.Detail)
		}
		if _, found, err := h.store.Process(context.Background(), req.ID); err != nil || found {
			t.Errorf("%s: the refused registration was stored", c.what)
		}
	}
}

// A record that moved while the Process was registered is caught at the next
// start: the name would otherwise be served, and asked for at a certificate
// authority, for whoever holds it now.
func TestAStartProvesTheDeclaredHostNameAgain(t *testing.T) {
	h, fake, names := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	names.points("status.example.org", testPublicAddress)
	req := h.registerServed(fake, up, testProcessName, "status.example.org")

	names.points("status.example.org", "198.51.100.7")
	res, body := h.postJSON(http.MethodPost, processesPath+"/"+req.ID+"/start", startRequest{
		Container: req.Container, Image: testDigest,
	})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("start status = %d, body %s, want 403", res.StatusCode, body)
	}
	if p := h.problemOf(res, body); !strings.Contains(p.Detail, "198.51.100.7") {
		t.Errorf("the problem does not say what the name resolves to: %s", p.Detail)
	}
}

// And a table built at boot serves only the names that still point here, which
// is what the certificate policy answers from.
func TestTheTableDropsANameThatStoppedPointingHere(t *testing.T) {
	h, fake, names := serveProxying(t, testDomain, TLSACME)
	up := newUpstream(t)
	names.points("status.example.org", testPublicAddress)
	h.registerServed(fake, up, testProcessName, "status.example.org")
	if !h.server.proxy.holds("status.example.org") {
		t.Fatal("the name was not served after its registration")
	}

	names.points("status.example.org", "198.51.100.7")
	h.server.LoadRoutes(context.Background())

	if h.server.proxy.holds("status.example.org") {
		t.Error("a name that stopped pointing here is still served")
	}
	if err := h.server.proxy.certs.HostPolicy(context.Background(), "status.example.org"); err == nil {
		t.Error("a certificate would still be obtained for a name that points somewhere else")
	}
}

// In gateway mode this host's own addresses are behind the gateway, so without
// the public address the operator names there is nothing to prove a name
// against, and no declared name is served.
func TestGatewayModeWithoutAPublicAddressServesNoDeclaredName(t *testing.T) {
	fake := sysusers.NewFake()
	names := newZone()
	h := serveWith(t, Options{
		Admin:    func(*user.User) (bool, error) { return false, nil },
		Users:    fake,
		Runner:   fake,
		Domain:   testDomain,
		TLS:      TLSGateway,
		CertDir:  filepath.Join(t.TempDir(), "certs"),
		Resolver: names,
	})
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid()})
	up := newUpstream(t)
	names.points("status.example.org", testPublicAddress)

	req := processRequest{
		ID: uuid.V7(), Package: "/org/sensorium", Name: testProcessName,
		Container: "kitbash-app-proof", Digest: testDigest,
		Expose: ExposeHTTP, Endpoint: up.server.URL, Hostname: "status.example.org",
	}
	fake.Publish(req.Container, up.port(t))
	fake.SetState(req.Container, podman.StateRunning)
	_, res, body := h.register(req)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body %s", res.StatusCode, body)
	}
	if p := h.problemOf(res, body); !strings.Contains(p.Fix, PublicAddressEnv) {
		t.Errorf("the fix does not name %s: %s", PublicAddressEnv, p.Fix)
	}
	// The derived name is unaffected: it is this host's own by construction.
	req.Hostname = ""
	req.ID = uuid.V7()
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("a derived name was refused too: %d %s", res.StatusCode, body)
	}
	if names.lookups != 0 {
		t.Errorf("the resolver was asked %d times on a host that can prove nothing, want none", names.lookups)
	}
}

// A Process that declares no name is not looked up at all: the derived name is
// this host's own by construction, so there is nothing to prove and no
// registration pays for a resolution.
func TestADerivedNameIsNotResolved(t *testing.T) {
	h, fake, names := serveProxying(t, testDomain, TLSACME)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")
	h.server.LoadRoutes(context.Background())
	if names.lookups != 0 {
		t.Errorf("the resolver was asked %d times for a Process that declared no name", names.lookups)
	}
}

// ------------------------------------------------------- the fan out receivers

// A unit that declares provides.subscriptions is exposed over HTTP for one
// reason, that kitbashd has a port to POST records to, so it is not a service
// and it is not published: no name is answered for it, nothing is routed to
// it, and the Process beside it that is a service still gets its own.
func TestASubscriberIsNotPublished(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	req := h.servedRequest(fake, up, "observe-count")
	req.Subscriptions = []string{store.SubscriptionTelemetry}

	if host := h.registeredHost(req); host != "" {
		t.Errorf("the registration answered the name %q, want none: a subscriber has no url", host)
	}
	name := h.defaultName("observe-count")
	if h.server.proxy.holds(name) {
		t.Errorf("%s is on the routing table, want a receiver nobody is forwarded to", name)
	}
	if rec := h.request(http.MethodGet, name, "/v1/traces", nil); rec.Code != http.StatusNotFound {
		t.Errorf("the name of a subscriber answered %d, want 404", rec.Code)
	}
	if up.received() != nil {
		t.Error("a request off the proxy reached a fan out receiver")
	}

	// The regression beside it: an ordinary http Process is still served
	// under the name it derives.
	service := newUpstream(t)
	if host := h.registeredHost(h.servedRequest(fake, service, testProcessName)); host != h.defaultName(testProcessName) {
		t.Fatalf("an http Process was answered %q, want %s", host, h.defaultName(testProcessName))
	}
	if rec := h.request(http.MethodGet, h.defaultName(testProcessName), "/", nil); rec.Code != http.StatusOK {
		t.Errorf("an http Process answered %d on its own name, want it served", rec.Code)
	}
}

// And a receiver may not declare a name of its own: there is nothing to serve
// it at, so the manifest is what has to change.
func TestASubscriberMayNotDeclareAHostName(t *testing.T) {
	h, fake, names := serveProxying(t, testDomain, TLSACME)
	up := newUpstream(t)
	// The member pointed the name here, so what refuses this is the
	// subscription and not the proof.
	names.points("records.example.org", testPublicAddress)
	req := h.servedRequest(fake, up, "observe-count")
	req.Hostname = "records.example.org"
	req.Subscriptions = []string{store.SubscriptionTelemetry}

	_, res, body := h.register(req)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body %s, want 422", res.StatusCode, body)
	}
	p := h.problemOf(res, body)
	if want := problem.Base + problem.SlugInvalidManifest; p.Type != want {
		t.Errorf("type = %q, want %q", p.Type, want)
	}
	if !strings.Contains(p.Detail, "a subscriber is a receiver for kitbashd and is not published") {
		t.Errorf("the problem does not say why: %s", p.Detail)
	}
	if h.server.proxy.holds("records.example.org") {
		t.Error("a refused registration put its name on the routing table")
	}
}

// A registration written before this rule existed is not published either: the
// table is rebuilt from the registrations at every start, so a subscriber a
// release that served one left behind leaves the table on the restart that
// reads it, whether it took the derived name or declared one.
func TestTheTableDoesNotPublishASubscriberFromAnOlderRelease(t *testing.T) {
	h, fake, names := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	names.points("records.example.org", testPublicAddress)

	derived := h.defaultName("observe-count")
	for _, row := range []store.Process{
		{Name: "observe-count", Container: "kitbash-observe-count-derived"},
		{Name: "evaluate-latency", Container: "kitbash-evaluate-latency-declared", Hostname: "records.example.org"},
	} {
		fake.Publish(row.Container, up.port(t))
		fake.SetState(row.Container, podman.StateRunning)
		_, hash, err := store.NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		row.ID = uuid.V7()
		row.Owner = h.user
		row.Package = "/org/" + row.Name
		row.Digest = testDigest
		row.Expose = ExposeHTTP
		row.Endpoint = up.server.URL
		row.Subscriptions = []string{store.SubscriptionTelemetry}
		row.RegisteredAt = time.Now().UTC()
		if err := h.store.RegisterProcess(context.Background(), row, hash, store.Quota{}); err != nil {
			t.Fatalf("RegisterProcess: %v", err)
		}
	}

	h.server.LoadRoutes(context.Background())

	if count := h.server.proxy.count(); count != 0 {
		t.Errorf("the table holds %d name(s), want none: both rows are receivers", count)
	}
	for _, name := range []string{derived, "records.example.org"} {
		if h.server.proxy.holds(name) {
			t.Errorf("%s is served after a restart, want a receiver nobody is forwarded to", name)
		}
	}
}

// ---------------------------------------------------------- what a client may spend

// serveProxyingBudget is serveProxying with a budget small enough for a test
// to spend, and the proxy served over a real listener, which is the only place
// a connection cap can be read.
func serveProxyingBudget(t *testing.T, mode string, total, perAddress int) (*harness, *sysusers.Fake, string) {
	t.Helper()
	fake := sysusers.NewFake()
	names := newZone()
	h := serveWith(t, Options{
		Admin:               func(*user.User) (bool, error) { return false, nil },
		Users:               fake,
		Runner:              fake,
		Domain:              testDomain,
		TLS:                 mode,
		CertDir:             filepath.Join(t.TempDir(), "certs"),
		PublicAddress:       testPublicAddress,
		Resolver:            names,
		ProxyMaxConnections: total,
		ProxyPerAddress:     perAddress,
	})
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid()})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.server.ServeProxy(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if _, returned := recvWithin(done); !returned {
			t.Errorf("waited %s for ServeProxy to return", waitBudget)
		}
	})
	return h, fake, ln.Addr().String()
}

// dialFrom opens a connection from one of this host's loopback addresses, so a
// test has two clients rather than two ports of one.
func dialFrom(t *testing.T, source, address string) (net.Conn, error) {
	t.Helper()
	dialer := net.Dialer{
		Timeout:   waitBudget,
		LocalAddr: &net.TCPAddr{IP: net.ParseIP(source)},
	}
	return dialer.Dial("tcp", address)
}

// In acme mode the peer is the client, so what one client may hold is a
// connection cap on the listener: the one over its share is closed, and
// another client is served while the first holds everything it may.
func TestOneClientCannotHoldTheWholeProxyInACMEMode(t *testing.T) {
	const perAddress = 4
	h, fake, address := serveProxyingBudget(t, TLSACME, 32, perAddress)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")

	var held []net.Conn
	t.Cleanup(func() {
		for _, conn := range held {
			conn.Close()
		}
	})
	for i := range perAddress {
		conn, err := dialFrom(t, "127.0.0.1", address)
		if err != nil {
			t.Fatalf("connection %d of this client was refused: %v", i+1, err)
		}
		// The head is sent and the body never finishes, which is what a
		// connection held open costs the host.
		fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: %s\r\nContent-Length: 1000000\r\n\r\nx", h.defaultName(testProcessName))
		held = append(held, conn)
	}

	// The one over the cap is accepted by the kernel and closed by the
	// listener, which the client reads as a connection that answers nothing.
	over, err := dialFrom(t, "127.0.0.1", address)
	if err == nil {
		defer over.Close()
		over.SetDeadline(time.Now().Add(waitBudget))
		fmt.Fprintf(over, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", h.defaultName(testProcessName))
		if line, err := bufio.NewReader(over).ReadString('\n'); err == nil {
			t.Errorf("the connection over this client's share was answered %q", strings.TrimSpace(line))
		}
	}

	// And another client is served while the first holds everything it may.
	other, err := dialFrom(t, "127.0.0.2", address)
	if err != nil {
		t.Fatalf("another client could not connect while one held its share: %v", err)
	}
	defer other.Close()
	other.SetDeadline(time.Now().Add(waitBudget))
	fmt.Fprintf(other, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", h.defaultName(testProcessName))
	line, err := bufio.NewReader(other).ReadString('\n')
	if err != nil {
		t.Fatalf("another client's request was not answered: %v", err)
	}
	// Plain 80 in acme mode is the redirect to https, which is the answer this
	// listener gives: what the test asks is whether it answered at all while
	// one client held everything it may.
	if !strings.Contains(line, "301") {
		t.Errorf("another client was answered %q, want the redirect this listener serves", strings.TrimSpace(line))
	}
}

// In gateway mode every connection is the gateway's, so capping the peer would
// cap the gateway itself: what is counted is requests in flight per the
// address the gateway wrote, and one client over its share is answered 429
// while another client of the same gateway is served.
func TestOneClientCannotHoldTheWholeProxyInGatewayMode(t *testing.T) {
	const perAddress = 4
	h, fake, address := serveProxyingBudget(t, TLSGateway, 32, perAddress)
	slow := newBlockingStub(t)
	port, _ := endpointPort(slow.server.URL)
	container := "kitbash-app-slow"
	fake.Publish(container, port)
	fake.SetState(container, podman.StateRunning)
	req := processRequest{
		ID: uuid.V7(), Package: "/org/sensorium", Name: testProcessName,
		Container: container, Digest: testDigest, Expose: ExposeHTTP, Endpoint: slow.server.URL,
	}
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	name := h.defaultName(testProcessName)

	// Four requests from one client, each held by the Process.
	client := &http.Client{Timeout: waitBudget}
	var wg sync.WaitGroup
	for range perAddress {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request, _ := http.NewRequest(http.MethodGet, "http://"+address+"/", nil)
			request.Host = name
			request.Header.Set("X-Forwarded-For", "203.0.113.9")
			if res, err := client.Do(request); err == nil {
				res.Body.Close()
			}
		}()
	}
	waitFor(t, "the Process to be holding this client's requests", func() bool {
		return slow.arrived.Load() >= perAddress
	})
	t.Cleanup(func() {
		slow.release()
		wg.Wait()
	})

	// The fifth from the same client is refused without reaching the Process.
	before := slow.arrived.Load()
	over, _ := http.NewRequest(http.MethodGet, "http://"+address+"/", nil)
	over.Host = name
	over.Header.Set("X-Forwarded-For", "203.0.113.9")
	res, err := client.Do(over)
	if err != nil {
		t.Fatalf("the request over this client's share was not answered at all: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 for a client over its share", res.StatusCode)
	}
	if slow.arrived.Load() != before {
		t.Error("the refused request reached the Process anyway")
	}

	// Another client behind the same gateway is served, which is the whole
	// point of counting the header rather than the peer.
	slow.release()
	honest, _ := http.NewRequest(http.MethodGet, "http://"+address+"/", nil)
	honest.Host = name
	honest.Header.Set("X-Forwarded-For", "198.51.100.5")
	answer, err := client.Do(honest)
	if err != nil {
		t.Fatalf("another client of the same gateway was not served: %v", err)
	}
	defer answer.Body.Close()
	if answer.StatusCode != http.StatusOK {
		t.Errorf("another client was answered %d, want the Process", answer.StatusCode)
	}
}

// ------------------------------------------------------- what a client claims

// The three X-Forwarded headers are not the only thing a client writes about
// itself. Every other header a Process behind a proxy is likely to believe is
// dropped, in both modes.
func TestTheClientsOwnIdentityHeadersDoNotReachTheProcess(t *testing.T) {
	for _, mode := range []string{TLSACME, TLSGateway} {
		h, fake, _ := serveProxying(t, testDomain, mode)
		up := newUpstream(t)
		h.registerServed(fake, up, testProcessName, "")
		sent := map[string]string{
			"Forwarded":        "for=203.0.113.9;proto=https;host=bank.example.org",
			"X-Real-Ip":        "203.0.113.9",
			"True-Client-Ip":   "203.0.113.9",
			"Cf-Connecting-Ip": "203.0.113.9",
			"X-Forwarded-Port": "443",
			"X-Original-Url":   "/admin",
			"X-Rewrite-Url":    "/admin",
		}
		if rec := h.request(http.MethodGet, h.defaultName(testProcessName), "/", sent); rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", mode, rec.Code)
		}
		got := up.received()
		for header := range sent {
			if have := got.Header.Get(header); have != "" {
				t.Errorf("%s: the Process received the client's own %s: %q", mode, header, have)
			}
		}
	}
}

// ------------------------------------------------------------ the acme wiring

// In acme mode the plain listener is the certificate client's own handler: it
// answers the challenge path and redirects everything else. A Process is not
// reachable over plain HTTP at all, which is the point of holding the
// certificates.
func TestTheACMEListenerAnswersTheChallengeAndRedirects(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSACME)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.server.ServeProxy(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if _, returned := recvWithin(done); !returned {
			t.Errorf("waited %s for ServeProxy to return", waitBudget)
		}
	})

	client := &http.Client{
		Timeout:       waitBudget,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	request, _ := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/status", nil)
	request.Host = name
	res, err := client.Do(request)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMovedPermanently {
		t.Errorf("plain HTTP answered %d, want a redirect to https", res.StatusCode)
	}
	if location := res.Header.Get("Location"); location != "https://"+name+"/status" {
		t.Errorf("Location = %q, want the same request over https", location)
	}
	if up.received() != nil {
		t.Error("a plain HTTP request reached a Process in acme mode")
	}

	// The challenge path is the client's own and is not redirected, or no
	// certificate is ever obtained.
	challenge, _ := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/.well-known/acme-challenge/token", nil)
	challenge.Host = name
	answer, err := client.Do(challenge)
	if err != nil {
		t.Fatalf("challenge request: %v", err)
	}
	defer answer.Body.Close()
	if answer.StatusCode == http.StatusMovedPermanently {
		t.Error("the ACME challenge path was redirected, so no certificate could ever be obtained")
	}
}

// The certificates are a private key each, so the directory is the daemon's
// alone, made at start rather than at the first handshake.
func TestTheCertificateDirectoryIsRootOwnedAndNarrow(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "certs")
	fake := sysusers.NewFake()
	serveWith(t, Options{
		Admin:    func(*user.User) (bool, error) { return false, nil },
		Users:    fake,
		Runner:   fake,
		Domain:   testDomain,
		TLS:      TLSACME,
		CertDir:  dir,
		Resolver: newZone(),
	})
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("the certificate directory was not made at start: %v", err)
	}
	if mode := info.Mode().Perm(); mode != CertDirMode {
		t.Errorf("the certificate directory is %o, want %o", mode, CertDirMode)
	}
}

// A name is one Process's whichever kind it is, which is what the conflict
// check reads. A declared name cannot be under this host's domain, so the
// derived half of this answers one case: a host whose domain was changed under
// registrations written before it.
func TestTheNamesOneRegistrationHolds(t *testing.T) {
	declared := store.Process{
		Owner: "loki", Name: "app", Expose: ExposeHTTP, Hostname: "status.example.org",
	}
	got := heldNames(declared, testDomain)
	if len(got) != 2 || got[0] != "status.example.org" || got[1] != "app.loki."+testDomain {
		t.Errorf("a Process with a declared name holds %v, want the declared and the derived one", got)
	}
	if got := heldNames(store.Process{Owner: "loki", Name: "app", Expose: ExposeHTTP}, testDomain); len(got) != 1 {
		t.Errorf("a Process that declared none holds %v, want the derived name alone", got)
	}
	if got := heldNames(store.Process{Owner: "loki", Name: "app", Expose: ExposeMCP}, testDomain); len(got) != 0 {
		t.Errorf("an mcp Process holds %v, want no name", got)
	}
	if got := heldNames(store.Process{
		Owner: "loki", Name: "app", Expose: ExposeHTTP, Runner: "/org/pve-runner",
	}, testDomain); len(got) != 0 {
		t.Errorf("a Process a run kit owns holds %v, want no name here", got)
	}
	if got := heldNames(store.Process{
		Owner: "loki", Name: "observe-count", Expose: ExposeHTTP,
		Subscriptions: []string{store.SubscriptionTelemetry},
	}, testDomain); len(got) != 0 {
		t.Errorf("a fan out receiver holds %v, want no name: it is served under none", got)
	}
}
