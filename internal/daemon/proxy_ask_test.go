package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// testGatewayHost is where the gateway reaches this host: the address it was
// given, which is never one of the names it is asking about. Half the point of
// these tests is that the question is answered before anything is read out of
// the Host.
const testGatewayHost = "10.0.0.5:80"

// ask sends the gateway's question the way Caddy's on_demand_tls asks it.
func (h *harness) ask(method, domain string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(method, testGatewayHost, AskPath+"?"+AskDomainParam+"="+url.QueryEscape(domain), nil)
}

// ------------------------------------------------------------- the answer

// The sentence of this change: a gateway asking about a name this host serves
// is told yes, and that is what makes it obtain the certificate.
func TestTheGatewayIsToldAboutAServedName(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)

	rec := h.ask(http.MethodGet, name)
	if rec.Code != http.StatusOK {
		t.Fatalf("the gateway asked about %s and was answered %d, want 200: the name is served", name, rec.Code)
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("body = %q, want nothing: the answer is the status", body)
	}
	if up.received() != nil {
		t.Error("the gateway's question reached a Process")
	}
}

// A name nobody registered is answered no, so the gateway never asks a
// certificate authority for a name somebody merely pointed at this host.
func TestTheGatewayIsToldAboutANameNobodyServes(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")

	for _, name := range []string{
		"nobody." + h.user + "." + testDomain,
		testProcessName + ".stranger." + testDomain,
		testDomain,
		"bank.example.org",
	} {
		if rec := h.ask(http.MethodGet, name); rec.Code != http.StatusNotFound {
			t.Errorf("the gateway asked about %s and was answered %d, want 404", name, rec.Code)
		}
	}
}

// The name is read the way a Host is read, so the gateway asking in upper case
// or with the root written out is asking about the same Process.
func TestTheAskedNameIsReadLikeAHost(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)

	for _, asked := range []string{name, strings.ToUpper(name), name + ".", strings.ToUpper(name) + "."} {
		if rec := h.ask(http.MethodGet, asked); rec.Code != http.StatusOK {
			t.Errorf("the gateway asked about %q and was answered %d, want 200: it is the same name", asked, rec.Code)
		}
	}
}

// Something that is not a name is refused rather than looked up, which is the
// rule the Host of a request is held to.
func TestAnAskAboutSomethingThatIsNotANameIsRefused(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")

	for _, asked := range []string{
		"",
		"not a name",
		h.defaultName(testProcessName) + "/../etc/passwd",
		"evil.example@" + h.defaultName(testProcessName),
		"203.0.113.9",
		"[::1]",
	} {
		rec := h.ask(http.MethodGet, asked)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("the gateway asked about %q and was answered %d, want 400", asked, rec.Code)
		}
		if body := rec.Body.String(); strings.Contains(body, "@") || strings.Contains(body, "passwd") {
			t.Errorf("%q was repeated back in %q; a refused name is not echoed", asked, body)
		}
	}
	if up.received() != nil {
		t.Error("a question about something that is not a name reached a Process")
	}
}

// The question is a GET. Anything else is a caller that is not the gateway's
// on demand client, and it is refused with what this path answers.
func TestAnAskIsAGet(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
		rec := h.ask(method, name)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s answered %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Errorf("%s was answered with Allow %q, want GET", method, allow)
		}
	}
	if up.received() != nil {
		t.Error("a question asked with another method reached a Process")
	}
}

// --------------------------------------------------------- the reserved path

// The path is kitbashd's own on every name it serves. A Process that could
// answer it would be answering the gateway about whichever name the gateway
// asked, so the request is answered here and never forwarded.
func TestTheAskPathIsNotForwardedToAProcess(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)

	// The Host is the Process's own served name, which is the request a
	// Process would have to receive to impersonate the answer.
	rec := h.request(http.MethodGet, name, AskPath+"?"+AskDomainParam+"=bank.example.org", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: kitbashd answered, and it serves no bank.example.org", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "the Process answered") {
		t.Errorf("body = %q, want kitbashd's own answer", body)
	}
	if up.received() != nil {
		t.Fatal("the ask path was forwarded to a Process, which could then tell the gateway to obtain any certificate")
	}

	// The whole prefix, not the one path: a Process answering anything under
	// it is a Process answering for kitbashd on a name it does not own.
	for _, path := range []string{AskPrefix, AskPrefix + "ask/", AskPrefix + "anything"} {
		rec := h.request(http.MethodGet, name, path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s answered %d, want 404", path, rec.Code)
		}
		if up.received() != nil {
			t.Errorf("%s reached a Process", path)
		}
	}
}

// In acme mode this daemon holds the certificates and the routing table is the
// host policy, so there is no question to answer: the path is a path like any
// other.
func TestThereIsNoAskInACMEMode(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSACME)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)

	if rec := h.ask(http.MethodGet, name); rec.Code == http.StatusOK {
		t.Error("a host that holds its own certificates answered the gateway's question")
	}
	// The Host of that question is the gateway's address, which is not a name
	// this daemon routes at all.
	if rec := h.ask(http.MethodGet, name); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want the answer a request whose Host is an address gets", rec.Code)
	}
	if rec := h.request(http.MethodGet, "nobody."+h.user+"."+testDomain, AskPath, nil); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 like any unknown name", rec.Code)
	}
	// Nothing is reserved here either: the path belongs to the Process.
	rec := h.request(http.MethodGet, name, AskPath, nil)
	if rec.Code != http.StatusOK || up.received() == nil {
		t.Errorf("status = %d, want the Process's own answer in acme mode", rec.Code)
	}
}

// ---------------------------------------------------------- the table alone

// A member who stopped their Process still holds its name. A certificate left
// to lapse while it was down would make starting it again a wait for the
// gateway, so the question is answered from the table and not from the state.
func TestAStoppedProcessStillHoldsItsCertificate(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	req := h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)

	res, body := h.do(http.MethodPost, processesPath+"/"+req.ID+"/stop", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d, body %s", res.StatusCode, body)
	}
	// The name answers 503 to a request, which is what says the Process is
	// down rather than gone.
	if rec := h.request(http.MethodGet, name, "/", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("the stopped Process answered %d, want 503", rec.Code)
	}
	if rec := h.ask(http.MethodGet, name); rec.Code != http.StatusOK {
		t.Errorf("the gateway was answered %d about a stopped Process, want 200: its owner still holds the name", rec.Code)
	}
}

// A name that left the table leaves with the member: nothing is served there
// any more, so nothing should hold a certificate for it either.
func TestARemovedMembersNameIsNoLongerAsked(t *testing.T) {
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
	}, hash, 0); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}
	h.server.LoadRoutes(context.Background())
	name := testProcessName + ".alice." + testDomain
	if rec := h.ask(http.MethodGet, name); rec.Code != http.StatusOK {
		t.Fatalf("before the removal the gateway was answered %d about %s, want 200", rec.Code, name)
	}

	res, body := h.do(http.MethodDelete, usersPath+"/alice", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("users_remove status = %d, body %s", res.StatusCode, body)
	}
	if rec := h.ask(http.MethodGet, name); rec.Code != http.StatusNotFound {
		t.Errorf("the gateway was answered %d about %s once the member was gone, want 404", rec.Code, name)
	}
}

// ------------------------------------------------------------- the record

// One question is one span, carrying the name it was about and the answer it
// was given, so an operator reads how often the gateway asks and about what.
// It is kitbashd's own record: no Process served it and none is named.
func TestAnAskWritesOneSpan(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")
	name := h.defaultName(testProcessName)

	cases := []struct {
		asked  string
		want   bool
		status int
	}{
		{name, true, http.StatusOK},
		{"bank.example.org", false, http.StatusNotFound},
	}
	for _, c := range cases {
		h.request(http.MethodGet, testGatewayHost, AskPath+"?"+AskDomainParam+"="+url.QueryEscape(c.asked), map[string]string{
			"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		})
	}
	spans := h.proxySpans()
	if len(spans) != len(cases) {
		t.Fatalf("the store holds %d proxy spans, want one per question", len(spans))
	}
	for _, c := range cases {
		span, found := spanAbout(spans, c.asked)
		if !found {
			t.Errorf("no span was written about %s", c.asked)
			continue
		}
		if got := span.Attributes.Other[AttrProxyAsk]; got != c.want {
			t.Errorf("%s = %v, want %v", AttrProxyAsk, got, c.want)
		}
		if got := fmtStatus(span.Attributes.Other[AttrResponseCode]); got != c.status {
			t.Errorf("%s = %v, want %d", AttrResponseCode, got, c.status)
		}
		if span.Attributes.Process != "" || span.Attributes.Package != "" {
			t.Errorf("the span names the Process %q of %q; a question is nobody's Process",
				span.Attributes.Process, span.Attributes.Package)
		}
		if span.Attributes.User != InternalProducer {
			t.Errorf("user = %q, want %q: the gateway asked this daemon", span.Attributes.User, InternalProducer)
		}
		if _, claimed := span.Attributes.Other[AttrClientTraceparent]; claimed {
			t.Error("the asker's headers were recorded; a question is not a trace of the client's")
		}
	}
}

// A question this daemon refuses is not recorded at all: what arrived is not a
// name, and a record is not a place to put a caller's bytes.
func TestARefusedAskIsNotRecorded(t *testing.T) {
	h, fake, _ := serveProxying(t, testDomain, TLSGateway)
	up := newUpstream(t)
	h.registerServed(fake, up, testProcessName, "")

	h.ask(http.MethodGet, "not a name")
	h.ask(http.MethodPost, h.defaultName(testProcessName))
	if spans := h.proxySpans(); len(spans) != 0 {
		t.Errorf("the store holds %d proxy spans after two refusals, want none", len(spans))
	}
}

// spanAbout is the span one question wrote, found by the name it asked about:
// what order the store answers in is the store's business.
func spanAbout(spans []store.Span, name string) (store.Span, bool) {
	for _, span := range spans {
		if span.Attributes.Other[AttrProxyHost] == name {
			return span, true
		}
	}
	return store.Span{}, false
}

// fmtStatus reads the status back out of an attribute, which JSON gives back
// as a number.
func fmtStatus(value any) int {
	switch n := value.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}
