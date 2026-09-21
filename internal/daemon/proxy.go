package daemon

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// kitbashd is the reverse proxy of every Process with expose: http, see
// PLAN.md section 2.3. It is not a third built in: it is how the built in
// runner exposes what it runs, decided by the manifest and by one setting the
// operator gives the host, see PLAN.md section 3.
//
// A host with no domain routes nothing, which is what every kitbash host did
// before this existed: an http Process keeps its internal port and is reached
// over loopback. With a domain every such Process is <name>.<member>.<domain>,
// or the name its unit declared, and a request for that name is forwarded to
// the port that Process's own container publishes.
//
// Two rules hold the whole thing up. A name that is not in the routing table
// never reaches a container, so the proxy is not a way to ask this daemon to
// request an arbitrary address. And the port is never the member's word: it is
// read off the container as its owner, the same discovery the health probe is
// held to, so a registration can never point the proxy at a neighbour's
// Process or at a port nobody published, see routePort.

// The two ways a kitbash host terminates TLS, as KITBASH_TLS spells them.
// acme is the default with a domain: kitbashd listens on 80 and 443 and
// obtains one certificate per host name. gateway listens on 80 alone and
// trusts the Host a gateway that already terminated TLS forwards, which is how
// a host behind one public address is put on the internet.
const (
	TLSACME    = "acme"
	TLSGateway = "gateway"
)

// Where the proxy listens when the operator names no address. Both are every
// interface: a host with a domain is a host something reaches from outside.
const (
	DefaultProxyListen    = ":80"
	DefaultProxyTLSListen = ":443"
)

// PublicAddressEnv is the name of the setting that tells this host the address
// it is reached on from outside, which is what a unit's declared hostname is
// proved against. It is named here because the problem a member reads says how
// to fix it and the fix is in /etc/conf.d/kitbashd.
const PublicAddressEnv = "KITBASH_PUBLIC_ADDRESS"

// DefaultCertDir is where autocert keeps the certificates it obtains. It is
// root owned and 0700 like the rest of /var/lib/kitbash: a private key is the
// proof this host is the name it serves.
const DefaultCertDir = "/var/lib/kitbash/certs"

// CertDirMode is the mode of that directory.
const CertDirMode = 0o700

// ProxyResponseHeaderTimeout is how long a Process has to begin answering one
// forwarded request. There is no timeout on the body after that and no idle
// timeout on the connection: a Process that streams is the point of an http
// Process, and a download of an hour is not a failure.
const ProxyResponseHeaderTimeout = 60 * time.Second

// ProxyReadHeaderTimeout bounds the request line and headers of a request off
// the wire, which is what stops a connection that dribbles a header forever.
const ProxyReadHeaderTimeout = 10 * time.Second

// ProxyShutdownTimeout is how long a forwarded request gets when the daemon
// stops. It is shorter than a streaming response on purpose: a daemon that is
// stopping does not wait out a download.
const ProxyShutdownTimeout = 5 * time.Second

// ReResolveWindow is how long the answer of one re-resolve stands. A container
// that is up and refusing connections is a 502 for as long as that lasts, and
// what arrives is the internet's to decide: without a window a caller holding
// the URL would set the rate this daemon inspects containers at, which is one
// podman child per request. One inspect per name per window is the whole cost,
// whatever the request rate, see forwardFailed.
const ReResolveWindow = 5 * time.Second

// ProxySpan is the name of the span one forwarded request writes.
const ProxySpan = "proxy"

// The attributes that span carries beyond the four, as the OpenTelemetry
// conventions spell them. kitbash.host is this daemon's own: which name the
// request arrived under is the question a member asks of these records.
const (
	AttrRequestMethod = "http.request.method"
	AttrResponseCode  = "http.response.status_code"
	AttrURLPath       = "url.path"
	AttrProxyHost     = "kitbash.host"
	// AttrProxyAsk is true when the gateway's question about a name was
	// answered yes, false when it was answered no, see serveAsk.
	AttrProxyAsk = "kitbash.ask"
	// AttrClientTraceparent is the trace context the client claimed, recorded
	// as a claim and never adopted as this span's parent, see recordForward.
	AttrClientTraceparent = "kitbash.client.traceparent"
)

// The path kitbashd answers on every name it serves, and the prefix that path
// lives under. In gateway mode the prefix is reserved: a request for it is
// answered here and never forwarded, whichever served name it arrived under,
// so no Process can answer the gateway's question about a name it does not
// own, see serveAsk.
const (
	AskPrefix = "/.kitbash/"
	AskPath   = AskPrefix + "ask"
)

// AskDomainParam is the query parameter Caddy's on demand TLS asks with. The
// name is Caddy's and not kitbash's: this endpoint exists to be the ask of an
// on_demand_tls block.
const AskDomainParam = "domain"

// MaxClientTraceparent is how much of that header is kept. A traceparent is 55
// characters; what is over that is not one, and a record is not a place to put
// a client's bytes by the kilobyte.
const MaxClientTraceparent = 256

// The two listeners the proxy binds, as health names them.
const (
	listenerProxy    = "proxy"
	listenerProxyTLS = "proxy-tls"
)

// route is one name the proxy serves: which Process answers it, and on which
// port of the loopback address. A port of zero is a Process that is registered
// and has no container publishing anything, which is a name that answers 503
// rather than one that is not served at all.
type route struct {
	host      string
	id        string
	owner     string
	pkg       string
	container string
	port      int
}

// reResolver is what bounds a failed forward. The routing table alone cannot
// say whether a container is still there, and the answer costs a runtime call,
// so it is asked once per route at a time and the answer stands for
// ReResolveWindow: concurrent refusals wait for the call already going rather
// than starting one each, and everything that arrives after it reads what that
// call found.
type reResolver struct {
	mu      sync.Mutex
	answers map[string]*reResolveAnswer
}

// reResolveAnswer is what one re-resolve of one route found, and whether the
// call it is about has finished.
type reResolveAnswer struct {
	// ready is closed when the call has answered, and nil after that, which
	// is what tells a caller that has to wait from one that can read.
	ready   chan struct{}
	dropped bool
	until   time.Time
}

// resolve answers whether this route's port was dropped, asking at most once
// per route per window. ask is the question, called by one caller at a time
// and not at all while an answer is fresh.
func (r *reResolver) resolve(id string, now time.Time, ask func() bool) bool {
	r.mu.Lock()
	if held := r.answers[id]; held != nil {
		if held.ready != nil {
			// A call is going. Its answer is this caller's answer: two
			// requests that arrive together are one inspect.
			ready := held.ready
			r.mu.Unlock()
			<-ready
			r.mu.Lock()
			dropped := false
			if after := r.answers[id]; after != nil {
				dropped = after.dropped
			}
			r.mu.Unlock()
			return dropped
		}
		if now.Before(held.until) {
			dropped := held.dropped
			r.mu.Unlock()
			return dropped
		}
	}
	mine := &reResolveAnswer{ready: make(chan struct{})}
	r.answers[id] = mine
	r.mu.Unlock()

	dropped := ask()

	r.mu.Lock()
	ready := mine.ready
	mine.dropped, mine.until, mine.ready = dropped, now.Add(ReResolveWindow), nil
	r.mu.Unlock()
	close(ready)
	return dropped
}

// forget drops what is remembered about one route, which is what building
// that route again does: the table has just been told what the runtime says,
// so the answer of a call made before it is not about this route any more.
func (r *reResolver) forget(id string) {
	r.mu.Lock()
	delete(r.answers, id)
	r.mu.Unlock()
}

// forgetAll drops the lot, which a reload does: the table it was about is gone.
func (r *reResolver) forgetAll() {
	r.mu.Lock()
	r.answers = map[string]*reResolveAnswer{}
	r.mu.Unlock()
}

// proxy holds the routing table and the two things built from it: the client
// that forwards a request and, in acme mode, the certificates. Everything here
// is read on the request path, so the lock is a read write one and the table is
// replaced rather than walked.
type proxy struct {
	// domain is what the operator gave this host, empty for a host that has
	// none, which is a proxy that routes nothing.
	domain string
	// mode is TLSACME or TLSGateway.
	mode string
	// public is the address the proxy is reached on from outside, as the
	// operator named it, invalid when they named none. It is what a declared
	// host name is proved against, see hostnames.go.
	public netip.Addr
	// interfaces reads this host's own addresses. It is a field so a test
	// answers for a host it does not have.
	interfaces func() []netip.Addr

	// maxConns is how many connections this listener serves at once and
	// perAddress how many one client may hold. They are the proxy's own and
	// not the API's: the socket carries a session per member and this carries
	// the internet, so one budget spent is not the other.
	maxConns   int
	perAddress int

	mu     sync.RWMutex
	routes map[string]route

	// resolves bounds what a forward that failed costs, see reResolver.
	resolves *reResolver

	forwarder *httputil.ReverseProxy
	// certs is the ACME client, nil in gateway mode and on a host with no
	// domain.
	certs *autocert.Manager
	// inflight counts the requests one client has in flight, which is how
	// gateway mode caps a client whose connections all belong to the gateway,
	// see budget.go.
	inflight *addressCounter
}

// routeKey carries the route one request was matched to from the handler into
// the rewrite, so the forwarder is built once rather than per request.
type routeKey struct{}

// newProxy builds the proxy for one domain. An empty domain is a host that
// routes nothing: every method still works and the table stays empty, so
// nothing else in the daemon has to ask whether there is a proxy.
func newProxy(opts Options) *proxy {
	mode := opts.TLS
	if mode == "" {
		mode = TLSACME
	}
	p := &proxy{
		domain:     opts.Domain,
		mode:       mode,
		interfaces: hostInterfaceAddrs,
		maxConns:   opts.ProxyMaxConnections,
		perAddress: opts.ProxyPerAddress,
		routes:     map[string]route{},
		resolves:   &reResolver{answers: map[string]*reResolveAnswer{}},
	}
	if p.maxConns <= 0 {
		p.maxConns = ProxyMaxConnections
	}
	if p.perAddress <= 0 {
		p.perAddress = ProxyPerAddress
	}
	p.inflight = newAddressCounter(p.perAddress)
	if opts.PublicAddress != "" {
		addr, err := netip.ParseAddr(opts.PublicAddress)
		if err != nil {
			logger.Printf("proxy: %s is %q, which is not an address; no declared hostname can be proved against it",
				PublicAddressEnv, opts.PublicAddress)
		} else {
			p.public = addr.Unmap()
		}
	}
	p.forwarder = &httputil.ReverseProxy{
		Rewrite:      p.rewrite,
		Transport:    proxyTransport(),
		ErrorLog:     logger,
		ErrorHandler: p.upstreamFailed,
	}
	return p
}

// proxyTransport is how the proxy reaches a Process: over loopback and nowhere
// else. The dialler is the fan out's, which re-checks at connect time what the
// routing table checked when it was built, so a name that somehow reached this
// transport with an address in it still cannot take the daemon off the host.
func proxyTransport() *http.Transport {
	return &http.Transport{
		DialContext:           dialLoopback,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       IdleSubscriberTimeout,
		ResponseHeaderTimeout: ProxyResponseHeaderTimeout,
		// No HTTP/2 to the Process: a WebSocket upgrade is an HTTP/1.1
		// connection the proxy hands over whole, and a Process that speaks
		// HTTP/2 is not something kitbash negotiates for a member.
		ForceAttemptHTTP2: false,
	}
}

// enabled reports whether this host routes anything at all.
func (p *proxy) enabled() bool { return p != nil && p.domain != "" }

// acme reports whether this host obtains its own certificates.
func (p *proxy) acme() bool { return p.enabled() && p.mode == TLSACME }

// track puts one name on the table, replacing whatever the Process held
// before. A Process that moved from one name to another leaves no entry
// behind: the old name is dropped by id before the new one is written.
func (p *proxy) track(r route) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropID(r.id)
	p.routes[r.host] = r
}

// untrack takes one Process off the table, which is what stopping,
// unregistering and removing it do.
func (p *proxy) untrack(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropID(id)
}

// dropID removes every name one Process holds. The caller holds the lock.
//
// What a re-resolve found about this Process goes with them: a route written
// again is the runtime's answer of a moment ago, so a call made before it has
// nothing to say about the route that is there now.
func (p *proxy) dropID(id string) {
	for host, held := range p.routes {
		if held.id == id {
			delete(p.routes, host)
		}
	}
	p.resolves.forget(id)
}

// portOf is the port one Process's name forwards to and whether the table
// holds a name for it at all. A port of zero is a name that answers 503.
func (p *proxy) portOf(id string) (int, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, held := range p.routes {
		if held.id == id {
			return held.port, true
		}
	}
	return 0, false
}

// dropPort takes the port off every name one Process holds and leaves the
// names, which is what a container that is not running any more leaves behind:
// the registration stands, so the name stands and answers 503, and nothing is
// forwarded to a port the container has freed.
func (p *proxy) dropPort(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for host, held := range p.routes {
		if held.id != id || held.port == 0 {
			continue
		}
		held.port = 0
		p.routes[host] = held
	}
}

// reload replaces the whole table, which is what a restore does: the
// registrations are the source of truth and a name nothing is registered under
// any more is not served.
func (p *proxy) reload(routes []route) {
	table := make(map[string]route, len(routes))
	for _, r := range routes {
		table[r.host] = r
	}
	p.mu.Lock()
	p.routes = table
	p.mu.Unlock()
	p.resolves.forgetAll()
}

// lookup answers which Process serves one name.
func (p *proxy) lookup(host string) (route, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r, held := p.routes[host]
	return r, held
}

// holds reports whether this host serves a name at all, which is what the
// certificate policy asks: kitbashd obtains a certificate for the names it
// routes and for no other, so a request for a name nobody registered costs the
// host nothing at the certificate authority.
func (p *proxy) holds(host string) bool {
	_, held := p.lookup(host)
	return held
}

// count is how many names are served, which health reports.
func (p *proxy) count() int {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.routes)
}

// hostFor is the name one registration is served under: the one its unit
// declared, or <name>.<member>.<domain>. It answers an empty string for every
// Process that is not served at all, which is a host with no domain, a Process
// that is not exposed over HTTP, one a run kit owns, because there is no
// container of it here, a fan out receiver, because nothing about it is meant
// for a browser, and one whose name does not make a DNS name.
//
// A member's account name and a Process name are both held to patterns the
// surface enforces, so the derived name is almost always valid; almost is not
// always, because an account name may end in a hyphen and a DNS label may not.
// Such a Process is not routed rather than routed under a name no resolver
// answers.
func hostFor(p store.Process, domain string) string {
	if domain == "" || p.Expose != ExposeHTTP || p.Runner != "" || p.Subscribes() {
		return ""
	}
	if p.Hostname != "" {
		if !manifest.ValidHostname(p.Hostname) {
			return ""
		}
		return p.Hostname
	}
	return defaultHost(p.Name, p.Owner, domain)
}

// defaultHost is the name a Process that declares none is served under. The
// member's label is the attribution: whose work a service is, is visible in
// its address, see PLAN.md section 2.3.
func defaultHost(name, owner, domain string) string {
	if name == "" || owner == "" || domain == "" {
		return ""
	}
	host := name + "." + owner + "." + domain
	if !manifest.ValidHostname(host) {
		return ""
	}
	return host
}

// heldNames is every name one registration claims: the declared one, whether
// or not this host has a domain, and the derived one, which needs a domain to
// exist at all and which a fan out receiver never holds, because it is served
// under no name. It is what a registration declaring a host name is checked
// against, so two Processes never claim one name even on a host that routes
// nothing yet.
func heldNames(p store.Process, domain string) []string {
	var names []string
	if p.Hostname != "" {
		names = append(names, p.Hostname)
	}
	if p.Expose == ExposeHTTP && p.Runner == "" && !p.Subscribes() {
		if derived := defaultHost(p.Name, p.Owner, domain); derived != "" && derived != p.Hostname {
			names = append(names, derived)
		}
	}
	return names
}

// urlFor is the address a member is given for one Process, which proc_run
// answers and proc_list shows. It is https whichever mode the host is in: in
// acme mode kitbashd holds the certificate, in gateway mode the gateway in
// front of it does, and either way the address a member hands to somebody is
// the one with TLS on it.
func urlFor(host string) string {
	if host == "" {
		return ""
	}
	return "https://" + host
}

// hostOf reads the name a request names itself with: the Host header without
// its port, folded to lower case, held to what a DNS name may carry.
//
// This is the one place a client's own bytes decide anything, so it decides as
// little as possible. A Host with a path, an at sign, a space or a control
// character in it is not a name and is refused here rather than being looked
// up, matched against a certificate policy or written into a span. An address
// is refused as well, written out in either family: kitbash routes names, and
// four numeric labels are a name by shape alone.
func hostOf(raw string) (string, bool) {
	host := raw
	if strings.Contains(host, ":") {
		split, _, err := net.SplitHostPort(host)
		if err != nil {
			return "", false
		}
		host = split
	}
	host = strings.ToLower(host)
	// One trailing dot is the root, which is the same name written out in
	// full: a client that sends it is naming the Process, not something else.
	host = strings.TrimSuffix(host, ".")
	if !manifest.ValidHostname(host) || isAddressLiteral(host) {
		return "", false
	}
	return host, true
}

// ProxyHandler is the reverse proxy, for the listener and for a test that
// serves it over its own.
func (s *Server) ProxyHandler() http.Handler {
	return recovered(http.HandlerFunc(s.serveProxy))
}

// serveProxy answers one request off the wire: it matches the Host against the
// routing table and forwards, or it refuses.
func (s *Server) serveProxy(w http.ResponseWriter, r *http.Request) {
	// In gateway mode every connection belongs to the gateway, so the
	// listener's per peer cap would cap the gateway itself: what one client
	// may hold is counted here instead, by the address the gateway wrote into
	// X-Forwarded-For, see budget.go. In acme mode the peer is the client and
	// the listener has already counted it.
	if s.proxy.mode == TLSGateway {
		key := s.proxy.clientKey(r)
		if !s.proxy.inflight.take(key) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Retry-After", RetryAfterSeconds)
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintln(w, "This client has too many requests in flight on this host.")
			return
		}
		defer s.proxy.inflight.release(key)
	}
	// The gateway's own question, answered before anything is read out of the
	// Host. The gateway reaches this host at whatever address it was given,
	// which is as often as not an address literal and never a served name, and
	// the name it is asking about is in the query. The whole prefix is
	// reserved rather than the one path: a Process that could answer under
	// /.kitbash/ on its own name would be answering the gateway about a name
	// it does not own.
	if s.proxy.mode == TLSGateway && strings.HasPrefix(r.URL.Path, AskPrefix) {
		if r.URL.Path == AskPath {
			s.serveAsk(w, r)
			return
		}
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			"this path is kitbashd's own and is not served by any Process",
			"Request a path of your own; kitbashd answers "+AskPrefix+" on every name it serves."))
		return
	}
	host, ok := hostOf(r.Host)
	if !ok {
		// The Host itself is not repeated: it is a client's bytes and this is
		// the answer to a request that carried something that is not a name.
		writeProblem(w, problem.BadRequest(r.URL.Path,
			"this request does not name a host kitbash could read",
			"Request the address kitbash answered with, which is https://<name>.<member>.<domain>."))
		return
	}
	target, held := s.proxy.lookup(host)
	if !held {
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("no Process on this host is served at %s", host),
			"Call proc_list to see which address each of your Processes answers on."))
		return
	}
	if target.port == 0 {
		// The name is served and the Process behind it is not running: its
		// container publishes nothing, so there is nowhere to forward to. That
		// is a different answer from a name nobody holds, and it is the one a
		// member reads when they stopped their own Process.
		writeStopped(w, host)
		s.recordForward(r, target, host, http.StatusServiceUnavailable, s.now(), s.now())
		return
	}
	s.forward(w, r, target, host)
}

// writeStopped is the answer for a name this host serves whose Process is not
// running. It is one body and one status wherever that is found: at the table,
// where the port is already zero, and at a forward that found the container
// gone since, see forwardFailed.
func writeStopped(w http.ResponseWriter, host string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, "%s is registered on this host and its Process is not running.\n", host)
}

// serveAsk answers the one question a gateway asks before it obtains a
// certificate: is this a name kitbashd serves? Caddy's on_demand_tls asks it
// with GET /.kitbash/ask?domain=<name> and obtains a certificate only on 200,
// so a gateway in front of this host gets one certificate per served name and
// asks the certificate authority for nothing else. It is the policy acme mode
// enforces in certManager, asked over HTTP instead: the routing table decides
// and nothing else does. A wildcard would not do: it covers one label and the
// default names have two, see PLAN.md section 2.3.
//
// The table is the whole of the answer, whatever state the Process is in. A
// member who stopped their Process still holds its name, and a certificate
// left to lapse while it was down would make starting it again a wait for the
// gateway rather than a start.
//
// Nothing is kept for a name nobody serves beyond reading it, no name is
// resolved and no header is read: this is one parse and one map lookup, and
// the gateway may ask as often as it likes.
func (s *Server) serveAsk(w http.ResponseWriter, r *http.Request) {
	start := s.now()
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprintln(w, "This path answers GET.")
		return
	}
	name, ok := hostOf(r.URL.Query().Get(AskDomainParam))
	if !ok {
		// Neither recorded nor repeated: what arrived is not a name, so it is
		// not something this host has an answer about.
		writeProblem(w, problem.BadRequest(r.URL.Path,
			"this request asks about something that is not a host name",
			"Ask with "+AskPath+"?"+AskDomainParam+"=<name>."))
		return
	}
	held := s.proxy.holds(name)
	if !held {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "%s is not a name this host serves.\n", name)
		s.recordAsk(r, name, false, http.StatusNotFound, start, s.now())
		return
	}
	// The answer is the status, so the gateway obtains the certificate and
	// reads nothing else.
	w.WriteHeader(http.StatusOK)
	s.recordAsk(r, name, true, http.StatusOK, start, s.now())
}

// forward hands one request to the Process and records what happened.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, target route, host string) {
	start := s.now()
	recorder := &recordingWriter{ResponseWriter: w}
	s.proxy.forwarder.ServeHTTP(recorder, r.WithContext(context.WithValue(r.Context(), routeKey{}, target)))
	s.recordForward(r, target, host, recorder.code(), start, s.now())
}

// rewrite builds the request the Process receives. Three things decide it and
// none of them is a header the client sent.
//
// The address is the routing table's, which is the loopback port the runtime
// says this Process's own container publishes. The Host the Process sees is
// the one the request arrived under, because an application writes its own
// addresses from it. And the three X-Forwarded headers are set here, which
// replaces whatever the client sent: SetXForwarded writes the peer's address
// into X-Forwarded-For rather than appending to a chain a client can write, so
// a request cannot arrive claiming to come from somewhere else.
//
// X-Forwarded-Proto is the one value a gateway is believed about, and only in
// gateway mode: the gateway is what terminated TLS, so it is the only thing
// that knows whether the member out there was on https. In acme mode it is
// https, because this daemon terminated TLS itself and plain 80 is redirected
// before it reaches here.
//
// The three X-Forwarded headers are not the only thing a client writes about
// who it is, so every other header a Process is likely to believe is dropped
// rather than passed on: a proxy that replaced three names and forwarded five
// others would be telling the Process what the client wanted it to hear.
// kitbash speaks for the client's address in one header and in no other.
func (p *proxy) rewrite(r *httputil.ProxyRequest) {
	target, ok := r.In.Context().Value(routeKey{}).(route)
	if !ok {
		return
	}
	r.SetURL(&url.URL{Scheme: "http", Host: net.JoinHostPort(LoopbackHost, strconv.Itoa(target.port))})
	r.Out.Host = r.In.Host
	r.SetXForwarded()
	r.Out.Header.Set("X-Forwarded-Proto", p.forwardedProto(r.In))
	for _, header := range clientClaims {
		r.Out.Header.Del(header)
	}
}

// clientClaims are the headers a client writes about itself that a Process
// behind a proxy would otherwise read as the proxy's word. Forwarded is the
// standard one, the rest are what the large proxies made de facto; a Process
// that reads any of them has to read what kitbash says and not what the
// internet said, so none of them survives the forward. They are dropped in
// both modes, because a gateway that means one of them can write
// X-Forwarded-Proto, which is the one value this proxy takes from it.
var clientClaims = []string{
	"Forwarded",
	"X-Real-Ip",
	"True-Client-Ip",
	"Cf-Connecting-Ip",
	"X-Forwarded-Port",
	"X-Original-Url",
	"X-Rewrite-Url",
}

// forwardedProto is what the Process is told the member reached this host over.
func (p *proxy) forwardedProto(in *http.Request) string {
	if p.mode != TLSGateway {
		return "https"
	}
	// Only the two spellings, and only in gateway mode. Anything else is a
	// value this daemon does not pass on: what reaches the Process is what
	// kitbash is prepared to say about the request.
	switch strings.ToLower(strings.TrimSpace(in.Header.Get("X-Forwarded-Proto"))) {
	case "http":
		return "http"
	case "https":
		return "https"
	}
	// A gateway that says nothing has said nothing: what reached this daemon
	// is plain HTTP over the wire between them, and a Process told https would
	// be told something no part of this host observed. The url a member is
	// given is still https, because that is what the gateway holds the
	// certificate for; what the Process is told is what arrived.
	return "http"
}

// forwardFailed answers a forward that did not reach the Process. It is the
// proxy's error handler, set on the forwarder once the Server exists, because
// the one thing it does beyond answering is ask the runtime a question.
//
// A container that died outside proc_stop keeps the port it had: the state is
// read when the route is built, at a registration, a start and a restore, and
// nothing rebuilds it when a container is killed, crashes or is stopped by its
// owner with podman. The port is then a port nothing is bound to, so the
// connection is refused by the kernel and the honest answer is not 502.
//
// So a forward nothing accepted the connection for is re-resolved: the runtime
// is asked what it calls that container, and a container that is not running,
// or one the runtime does not have at all, loses its port here and answers
// 503, the same answer a Process stopped with proc_stop gives. One that is
// running keeps its 502, because a Process that is up and not answering is
// what 502 is for.
//
// Two things bound it. Only a dial failure asks: a connection reset while the
// response was being read is a container that was there and answered, and a
// timeout is a Process that is slow. And the answer is asked once per route
// per ReResolveWindow, so a 502 loop costs one inspect per window per name
// whatever arrives, see reResolver and issue #149.
func (s *Server) forwardFailed(w http.ResponseWriter, r *http.Request, err error) {
	// A forward that had begun answering is neither re-resolved nor answered
	// again: the status is on the wire already, and a second WriteHeader is a
	// log line and nothing more.
	if recorder, ok := w.(*recordingWriter); ok && recorder.wrote {
		logger.Printf("proxy: the Process serving %s stopped answering mid response: %v", r.Host, err)
		return
	}
	target, held := r.Context().Value(routeKey{}).(route)
	if held && dialRefused(err) && s.dropDeadRoute(r.Context(), target) {
		writeStopped(w, target.host)
		return
	}
	s.proxy.upstreamFailed(w, r, err)
}

// dropDeadRoute asks the runtime whether one route's container is still
// running and takes the port off the table when it is not. It reports whether
// the route is one this request answers 503 for.
//
// The question goes through the re-resolver, so what it costs is bounded by
// the window and not by the request rate.
func (s *Server) dropDeadRoute(ctx context.Context, target route) bool {
	if target.container == "" {
		return false
	}
	return s.proxy.resolves.resolve(target.id, s.now(), func() bool {
		return s.containerStopped(ctx, target)
	})
}

// containerStopped is the one runtime call a re-resolve makes, and what it
// does with the answer.
//
// A container the runtime does not have is a container that is not running,
// which is what podman rm by the owner leaves behind, and it is the answer
// routePort gives no port for either. A runtime that would not answer at all
// leaves the table as it is: what the port says is the last thing this daemon
// knew, and 502 is the honest answer to a forward that failed for a reason
// nothing here could read.
func (s *Server) containerStopped(ctx context.Context, target route) bool {
	m, found, err := s.users.Lookup(ctx, target.owner)
	if err != nil || !found {
		return false
	}
	config, err := s.runner.ContainerConfig(ctx, m, target.container)
	switch {
	case errors.Is(err, sysusers.ErrNoContainer):
	case err != nil:
		return false
	case config.State == podman.StateRunning:
		return false
	}
	logger.Printf("proxy: the container of %s is not running any more, %s answers 503 until it is started again",
		target.id, target.host)
	s.proxy.dropPort(target.id)
	return true
}

// dialRefused reports whether a forward failed because nothing accepted the
// connection, which is the one failure a container that is gone produces and
// the only one worth a runtime call.
func dialRefused(err error) bool {
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "dial" {
		return false
	}
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET)
}

// upstreamFailed answers a Process that could not be reached or that closed
// the connection mid answer. The runtime's own words stay in the daemon log,
// which is the operator's.
func (p *proxy) upstreamFailed(w http.ResponseWriter, r *http.Request, err error) {
	logger.Printf("proxy: forwarding a request for %s: %v", r.Host, err)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	fmt.Fprintln(w, "The Process serving this address did not answer.")
}

// recordingWriter is the response writer the forwarder is given, which
// remembers the status for the span and hands everything else through. A
// WebSocket upgrade takes the connection over, so Hijack and Flush are passed
// on as well: a wrapper that swallowed either would break streaming and
// upgrades, which are the two things an http Process does that a JSON API does
// not.
type recordingWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *recordingWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap is what http.ResponseController follows to find the writer's own
// Flush and Hijack.
func (w *recordingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *recordingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *recordingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("daemon: this connection cannot be handed over to a Process")
	}
	if !w.wrote {
		// The forwarder writes the 101 to the connection itself once it has
		// it, so this is where an upgrade is recorded.
		w.status = http.StatusSwitchingProtocols
		w.wrote = true
	}
	return hijacker.Hijack()
}

// code is the status of the response, or 502 for a forward that wrote nothing
// at all, which is a Process that dropped the connection before the header.
func (w *recordingWriter) code() int {
	if !w.wrote {
		return http.StatusBadGateway
	}
	return w.status
}

// recordForward writes the one span a forwarded request produces. It carries
// the four attributes of PLAN.md section 2.4, so a member's query by user
// answers what was requested of their Processes, plus the method, the status,
// the path and the name it arrived under. No body is read and no header is
// copied: a span per request has to stay cheap.
//
// The span begins a trace of its own. A request off the internet may carry a
// traceparent and whoever sent it is nobody this host knows: adopting it would
// let a stranger write their own trace id into a member's Telemetry, join
// their requests to somebody else's trace and, with one id repeated, make
// every member's records answer one query. The header is recorded as what it
// is, a claim, in kitbash.client.traceparent, so a member whose own client
// sent one can still follow it.
func (s *Server) recordForward(r *http.Request, target route, host string, status int, start, end time.Time) {
	s.recordProxySpan(r.Context(), host, status, store.Attributes{
		User:     target.owner,
		Package:  target.pkg,
		Process:  target.id,
		Path:     r.URL.Path,
		Producer: InternalProducer,
		Other:    proxyAttributes(r, host, status),
	}, start, end)
}

// recordAsk writes the span the gateway's question produces, which is one per
// question like one per forwarded request. It is kitbashd's own record and not
// a member's: no Process served it, and the name asked about is as likely as
// not a name nobody here holds. What it carries beyond the forwarded request's
// attributes is kitbash.ask, the answer, so an operator reads how often the
// gateway asks and about what.
//
// Nothing of the asker is recorded. A forwarded request carries the client's
// traceparent as a claim because a member may want to follow their own client;
// this is a gateway asking a yes or no about a name, and the headers it asked
// with are not part of the answer.
func (s *Server) recordAsk(r *http.Request, name string, held bool, status int, start, end time.Time) {
	s.recordProxySpan(r.Context(), name, status, store.Attributes{
		User:     InternalProducer,
		Path:     r.URL.Path,
		Producer: InternalProducer,
		Other: map[string]any{
			AttrRequestMethod: r.Method,
			AttrResponseCode:  status,
			AttrURLPath:       r.URL.Path,
			AttrProxyHost:     name,
			AttrProxyAsk:      held,
		},
	}, start, end)
}

// recordProxySpan mints the identity of one proxy span, writes it and fans it
// out. It is what a forwarded request and an answered question have in common:
// everything about either is already in the attributes, and what is left is a
// trace of its own and the one write.
func (s *Server) recordProxySpan(ctx context.Context, host string, status int,
	attrs store.Attributes, start, end time.Time) {
	traceID, err := randomID(traceIDBytes)
	if err != nil {
		logger.Printf("proxy: could not record a request for %s: %v", host, err)
		return
	}
	spanID, err := randomID(spanIDBytes)
	if err != nil {
		logger.Printf("proxy: could not record a request for %s: %v", host, err)
		return
	}
	export := store.Export{Spans: []store.Span{{
		TraceID:    traceID,
		SpanID:     spanID,
		Name:       ProxySpan,
		StartNS:    start.UnixNano(),
		EndNS:      end.UnixNano(),
		Status:     spanStatus(status),
		Attributes: attrs,
	}}}
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), HealthWriteTimeout)
	defer cancel()
	if err := s.store.Insert(write, export); err != nil {
		logger.Printf("proxy: could not record a request for %s: %v", host, err)
		return
	}
	s.fanout.dispatch(export, InternalProducer)
}

// spanStatus is what the store records about one forwarded request. A status
// the Process answered is the Process's business, so only a request kitbash
// could not forward is an error.
func spanStatus(status int) string {
	if status == http.StatusBadGateway || status == http.StatusServiceUnavailable {
		return "error"
	}
	return "ok"
}

// The widths of a trace id and a span id, in bytes, as W3C trace context and
// the query surface spell them.
const (
	traceIDBytes = 16
	spanIDBytes  = 8
)

// proxyAttributes is what one forwarded request records beyond the four.
func proxyAttributes(r *http.Request, host string, status int) map[string]any {
	other := map[string]any{
		AttrRequestMethod: r.Method,
		AttrResponseCode:  status,
		AttrURLPath:       r.URL.Path,
		AttrProxyHost:     host,
	}
	if claimed := clientTraceparent(r.Header.Get("traceparent")); claimed != "" {
		other[AttrClientTraceparent] = claimed
	}
	return other
}

// clientTraceparent is the trace context a client claimed, bounded, for the
// one attribute that records it. It is not parsed and not believed: what it is
// for is a member following their own client's header, so it is carried as it
// arrived and cut to a length a header this daemon does not act on is worth.
func clientTraceparent(header string) string {
	header = strings.TrimSpace(header)
	if len(header) > MaxClientTraceparent {
		header = header[:MaxClientTraceparent]
	}
	return header
}

// randomID mints one trace or span id.
func randomID(width int) (string, error) {
	b := make([]byte, width)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// routePort is the port one Process is forwarded to: the port its own
// container publishes on the host, read off the runtime as the owner.
//
// The registration's endpoint is where the port is read from and it is never
// what decides: kitbash-mcp runs as the member, so the endpoint it sent is a
// claim, and the claim is held to what the runtime says that container
// publishes. This is the discovery the health probe is held to, for the same
// reason, see verifyProbe: a port the member chose would turn a registration
// into a way to have kitbashd forward the internet at a neighbour's Process.
//
// A Process whose container is not there yet, or whose container publishes
// nothing, has no port. That is not a refusal: the name stays on the table and
// answers 503 until the start that creates the container puts the port on it.
func (s *Server) routePort(ctx context.Context, m sysusers.Member, p store.Process) int {
	if p.Container == "" || p.Runner != "" {
		return 0
	}
	port, ok := endpointPort(p.Endpoint)
	if !ok {
		return 0
	}
	config, err := s.runner.ContainerConfig(ctx, m, p.Container)
	if err != nil {
		return 0
	}
	if !publishes(config, port) {
		return 0
	}
	// And the container has to be running. What a container publishes is the
	// configuration it was created with and it survives a stop, an exit and
	// the prepared state the four step start pauses in, so the published port
	// alone would have this daemon forward a name to a host port nothing is
	// bound to any more, which is the port the next member's container may be
	// given. The state is what restore reads for the same reason, see
	// restoreOne.
	if config.State != podman.StateRunning {
		return 0
	}
	return port
}

// publishes reports whether one container publishes a host port, which is the
// one question both the health probe and the proxy ask of the runtime.
func publishes(config sysusers.ContainerConfig, port int) bool {
	for _, published := range config.Publish {
		if published.HostPort == port {
			return true
		}
	}
	return false
}

// trackRoute puts one registration on the routing table, or takes it off. It
// is what a registration, a start and a restore each call for one Process.
func (s *Server) trackRoute(ctx context.Context, m sysusers.Member, p store.Process) {
	if !s.proxy.enabled() {
		return
	}
	host := hostFor(p, s.proxy.domain)
	if host == "" {
		s.proxy.untrack(p.ID)
		return
	}
	s.proxy.track(route{
		host:      host,
		id:        p.ID,
		owner:     p.Owner,
		pkg:       p.Package,
		container: p.Container,
		port:      s.routePort(ctx, m, p),
	})
}

// trackRouteFor is trackRoute for a caller that has not looked the owner up. A
// member this daemon cannot look up is a Process it cannot read a port for, so
// the name is not served.
func (s *Server) trackRouteFor(ctx context.Context, p store.Process) {
	if !s.proxy.enabled() {
		return
	}
	m, found, err := s.users.Lookup(ctx, p.Owner)
	if err != nil || !found {
		s.proxy.untrack(p.ID)
		return
	}
	s.trackRoute(ctx, m, p)
}

// LoadRoutes builds the routing table from the registrations kitbashd holds,
// which is what makes a name survive a reboot: the names travel with the
// registration, so a restore rebuilds the table rather than waiting for every
// member to run their Processes again.
//
// It replaces the table rather than adding to it, so a name whose registration
// is gone stops being served.
func (s *Server) LoadRoutes(ctx context.Context) int {
	if !s.proxy.enabled() {
		return 0
	}
	list, err := s.store.Processes(ctx, "")
	if err != nil {
		logger.Printf("proxy: could not read the registered Processes: %v", err)
		return 0
	}
	members := map[string]sysusers.Member{}
	var routes []route
	held := map[string]string{}
	for _, p := range list {
		host := hostFor(p, s.proxy.domain)
		if host == "" {
			continue
		}
		if other, taken := held[host]; taken {
			// Two registrations claiming one name is not something a request
			// can be answered from, so the first one keeps it. Registration
			// refuses this, so a table that finds it is reading rows written
			// before the check existed or under another domain.
			logger.Printf("proxy: %s is claimed by the Processes %s and %s; %s keeps it",
				host, other, p.ID, other)
			continue
		}
		// A declared name is proved again here, because this is the table the
		// certificate policy answers from: a name whose record moved while
		// this host was down must not be served and must not be asked for at
		// a certificate authority, see hostnames.go. A derived name is this
		// host's own by construction and is not looked up.
		if p.Hostname != "" {
			if prob := s.proveHostname(ctx, "", p); prob != nil {
				logger.Printf("proxy: not serving %s: %s", host, prob.Detail)
				continue
			}
		}
		m, known := members[p.Owner]
		if !known {
			looked, found, err := s.users.Lookup(ctx, p.Owner)
			if err != nil || !found {
				logger.Printf("proxy: not serving %s: %s owns it and could not be looked up: %v",
					host, p.Owner, err)
				continue
			}
			m = looked
			members[p.Owner] = m
		}
		held[host] = p.ID
		routes = append(routes, route{
			host:      host,
			id:        p.ID,
			owner:     p.Owner,
			pkg:       p.Package,
			container: p.Container,
			port:      s.routePort(ctx, m, p),
		})
	}
	s.proxy.reload(routes)
	if len(routes) > 0 {
		logger.Printf("proxy: serving %d name(s) under %s", len(routes), s.proxy.domain)
	}
	return len(routes)
}

// hostConflict refuses a registration that declares a name another Process on
// this host already holds, which is conflict at proc_run, see PLAN.md section
// 2.3. A name is one Process's: two of them would be a request nobody can
// answer, and the member who declared it second is the one who can change it.
//
// Only a declared name is checked, because a derived name is the Process's own
// by construction: it carries the owner's account name and the Process name,
// which is unique for that member. What a declared name is checked against is
// both kinds, the declared names of other Processes and their derived ones. A
// declared name can no longer be under this host's domain, so the derived half
// answers one case: a host whose domain was changed under registrations that
// were written before it, where a name that was outside the old domain is a
// derived name under the new one.
func (s *Server) hostConflict(ctx context.Context, instance string, p store.Process) *problem.Problem {
	if p.Hostname == "" {
		return nil
	}
	list, err := s.store.Processes(ctx, "")
	if err != nil {
		return problem.Internal(instance, err.Error(), "")
	}
	for _, other := range list {
		if other.ID == p.ID {
			// A re-registration of the same Process keeps its own name.
			continue
		}
		for _, name := range heldNames(other, s.proxy.domain) {
			if name != p.Hostname {
				continue
			}
			return problem.ConflictFix(instance,
				fmt.Sprintf("the host name %s is already served by another Process on this host", p.Hostname),
				"Declare another deploy.units[].hostname, or stop the Process that holds this one.")
		}
	}
	return nil
}

// certManager is the ACME client of a host in acme mode: one certificate per
// host name, cached under the daemon's own directory, and obtained for the
// names in the routing table and for no others.
//
// There is no wildcard certificate. A wildcard covers one label and the
// default names have two, see PLAN.md section 2.3.
func (s *Server) certManager(dir string) (*autocert.Manager, error) {
	if dir == "" {
		dir = DefaultCertDir
	}
	if err := os.MkdirAll(dir, CertDirMode); err != nil {
		return nil, fmt.Errorf("daemon: the certificate directory %s: %w", dir, err)
	}
	if err := os.Chmod(dir, CertDirMode); err != nil {
		return nil, fmt.Errorf("daemon: the certificate directory %s: %w", dir, err)
	}
	return &autocert.Manager{
		Cache:  autocert.DirCache(dir),
		Prompt: autocert.AcceptTOS,
		HostPolicy: func(_ context.Context, host string) error {
			name, ok := hostOf(host)
			if !ok || !s.proxy.holds(name) {
				return fmt.Errorf("daemon: this host serves no Process at %q", host)
			}
			return nil
		},
	}, nil
}

// ServeProxy answers the plain HTTP listener until ctx is done.
//
// What it serves depends on the mode. In gateway mode this is the whole proxy:
// a gateway in front of the host terminated TLS and forwards here. In acme
// mode it is the ACME client's own handler, which answers the HTTP-01
// challenge on its one path and redirects everything else to https, because
// this daemon holds the certificates and plain HTTP is not how a Process is
// reached.
//
// Nothing after the bind needs root. The listener is opened by the caller,
// before or after it drops anything it likes; this handler reads the routing
// table, opens loopback connections and writes the store, all of which the
// daemon does as itself.
func (s *Server) ServeProxy(ctx context.Context, ln net.Listener) error {
	handler := s.ProxyHandler()
	if s.proxy.acme() && s.proxy.certs != nil {
		handler = s.proxy.certs.HTTPHandler(http.HandlerFunc(redirectToHTTPS))
	}
	return s.serveProxyListener(ctx, ln, listenerProxy, handler, nil)
}

// ServeProxyTLS answers the HTTPS listener until ctx is done. It exists in
// acme mode alone: in gateway mode the gateway holds the certificate.
func (s *Server) ServeProxyTLS(ctx context.Context, ln net.Listener) error {
	if !s.proxy.acme() || s.proxy.certs == nil {
		return fmt.Errorf("daemon: this host does not terminate TLS itself, so there is no listener for 443")
	}
	return s.serveProxyListener(ctx, ln, listenerProxyTLS, s.ProxyHandler(), s.proxy.certs)
}

// serveProxyListener runs one of the two listeners. The timeouts are the
// proxy's own and not the API's: a Process that streams holds a response open
// for as long as it likes, so there is no write timeout and no idle timeout
// here, and the header timeout is what bounds a connection that says nothing.
func (s *Server) serveProxyListener(ctx context.Context, ln net.Listener, kind string,
	handler http.Handler, certs *autocert.Manager) error {
	s.bind(kind, ln.Addr().String())
	defer s.bind(kind, "")
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: ProxyReadHeaderTimeout,
		IdleTimeout:       ProxyIdleTimeout,
		MaxHeaderBytes:    ProxyMaxHeaderBytes,
		ErrorLog:          logger,
	}
	// The proxy's budget is its own and not the API's: the socket carries a
	// session per member, and what the internet holds against 80 must never be
	// the reason a member cannot reach the daemon. Inside it one peer is capped
	// as well, except in gateway mode, where every connection is the gateway's
	// and the cap is counted per request instead, see budget.go.
	perAddress := s.proxy.perAddress
	if s.proxy.mode == TLSGateway {
		perAddress = 0
	}
	bounded := limitProxy(ln, s.proxy.maxConns, perAddress)
	errs := make(chan error, 1)
	go func() {
		if certs != nil {
			srv.TLSConfig = certs.TLSConfig()
			errs <- srv.ServeTLS(bounded, "", "")
			return
		}
		errs <- srv.Serve(bounded)
	}()
	select {
	case err := <-errs:
		if isServerClosed(err) {
			return nil
		}
		return fmt.Errorf("daemon: serve the proxy: %w", err)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), ProxyShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			srv.Close()
		}
		return nil
	}
}

// isServerClosed reports the error a listener that was shut down answers with.
func isServerClosed(err error) bool {
	return err == nil || errors.Is(err, http.ErrServerClosed)
}

// redirectToHTTPS is what plain HTTP gets in acme mode, everything but the
// ACME challenge path the client's own handler answers first.
func redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	host, ok := hostOf(r.Host)
	if !ok {
		writeProblem(w, problem.BadRequest(r.URL.Path,
			"this request does not name a host kitbash could read",
			"Request the address kitbash answered with, which is https://<name>.<member>.<domain>."))
		return
	}
	target := url.URL{Scheme: "https", Host: host, Path: r.URL.Path, RawQuery: r.URL.RawQuery}
	http.Redirect(w, r, target.String(), http.StatusMovedPermanently)
}
