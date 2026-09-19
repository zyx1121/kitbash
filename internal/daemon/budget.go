package daemon

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// What one client may spend of the proxy, see PLAN.md section 2.3.
//
// The proxy listener carries the internet and the socket carries a session per
// member, so they do not share a budget: the connections a stranger holds
// against 80 must never be the reason a member's session cannot reach the
// daemon. And inside the proxy's own budget one client is bounded too, because
// a budget one caller can spend whole is a budget that decides who is served.

// ProxyMaxConnections is how many connections the proxy serves at once. It is
// far above what a host with fifty members needs and far below what a file
// descriptor limit of 1024 would refuse, because a connection here is a socket
// and nothing else until a request arrives.
const ProxyMaxConnections = 4096

// ProxyPerAddress is how many of them one client may hold at once. Sixty four
// is above what a browser opens for one page, six per name, and above what a
// member's own tooling needs, and it is a small enough share of the whole that
// a client spending all of it leaves the rest of the budget for everyone else.
const ProxyPerAddress = 64

// ProxyIdleTimeout is how long a kept alive connection may sit between
// requests. Without one a client that sends nothing keeps a slot for as long
// as it likes, which is the cheapest way there is to spend a budget.
const ProxyIdleTimeout = 120 * time.Second

// ProxyMaxHeaderBytes bounds the head of one request. It is Go's own default
// spelled out, because a proxy that forwards to a member's Process is a place
// where the default matters enough to be read.
const ProxyMaxHeaderBytes = 64 << 10

// addressCounter counts what one client holds, by address. It is used two
// ways: as the connection cap of a listener whose peers are clients, and as
// the in flight request cap where the peer is a gateway and the client is a
// header it wrote.
type addressCounter struct {
	limit int
	mu    sync.Mutex
	held  map[string]int
}

func newAddressCounter(limit int) *addressCounter {
	return &addressCounter{limit: limit, held: map[string]int{}}
}

// take reserves one for a client and reports whether it was under the cap. An
// empty key is not counted: a request with no address to charge is charged to
// the budget as a whole and to nobody in particular.
func (c *addressCounter) take(key string) bool {
	if c == nil || c.limit <= 0 || key == "" {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held[key] >= c.limit {
		return false
	}
	c.held[key]++
	return true
}

// release gives one back.
func (c *addressCounter) release(key string) {
	if c == nil || c.limit <= 0 || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held[key] <= 1 {
		delete(c.held, key)
		return
	}
	c.held[key]--
}

// count is what one client holds, for a test and for nothing else.
func (c *addressCounter) count(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held[key]
}

// proxyListener is the proxy's own accept loop: a total budget, and a cap per
// peer address where the peer is the client.
//
// The per peer cap is acme mode's. In gateway mode every connection comes from
// the gateway, so capping the peer would cap the whole gateway to one client's
// share; there the cap is on requests in flight per X-Forwarded-For address,
// see clientKey.
type proxyListener struct {
	net.Listener
	slots chan struct{}
	peers *addressCounter
}

// limitProxy wraps ln with the proxy's budget. perAddress of zero caps no
// single peer, which is what gateway mode passes.
func limitProxy(ln net.Listener, total, perAddress int) net.Listener {
	slots := make(chan struct{}, total)
	for range total {
		slots <- struct{}{}
	}
	return &proxyListener{
		Listener: ln,
		slots:    slots,
		peers:    newAddressCounter(perAddress),
	}
}

func (l *proxyListener) Accept() (net.Conn, error) {
	for {
		<-l.slots
		conn, err := l.Listener.Accept()
		if err != nil {
			l.slots <- struct{}{}
			return nil, err
		}
		key := addressOf(conn.RemoteAddr())
		if !l.peers.take(key) {
			// This client holds its share already. The connection is closed
			// rather than queued: a client that is over its cap is told now,
			// and the slot goes back for whoever is next.
			conn.Close()
			l.slots <- struct{}{}
			continue
		}
		var once sync.Once
		release := func() {
			once.Do(func() {
				l.peers.release(key)
				l.slots <- struct{}{}
			})
		}
		return &countedConn{Conn: conn, release: release}, nil
	}
}

// countedConn gives its slot and its client's place back when it is closed.
type countedConn struct {
	net.Conn
	release func()
}

func (c *countedConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}

// addressOf is the address half of a peer, without its port: one client is one
// address and not one connection.
func addressOf(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

// clientKey is who one request is charged to. In acme mode the peer is the
// client and what it says about itself is not read at all. In gateway mode the
// peer is the gateway, which writes the client's address into
// X-Forwarded-For, so the first entry of that header is the client; a request
// that arrived without one is charged to the gateway, which is the honest
// answer when the only thing this host knows is who handed it over.
func (p *proxy) clientKey(r *http.Request) string {
	peer := addressOf(remoteAddr(r))
	if p.mode != TLSGateway {
		return peer
	}
	if first := firstForwarded(r.Header.Get("X-Forwarded-For")); first != "" {
		return first
	}
	return peer
}

// remoteAddr is the peer of one request as an address.
func remoteAddr(r *http.Request) net.Addr {
	if r.RemoteAddr == "" {
		return nil
	}
	return addrString(r.RemoteAddr)
}

// addrString carries a string as a net.Addr, which is what a request's
// RemoteAddr is.
type addrString string

func (a addrString) Network() string { return "tcp" }
func (a addrString) String() string  { return string(a) }

// firstForwarded is the first entry of an X-Forwarded-For, which is the client
// the gateway saw. Anything that is not an address is not a key: a client
// cannot pick its own bucket by writing a word in the header, because in
// gateway mode the gateway rewrites this header and a request that reached
// here with something else in it is charged to the gateway instead.
func firstForwarded(header string) string {
	if header == "" {
		return ""
	}
	first, _, _ := strings.Cut(header, ",")
	first = strings.TrimSpace(first)
	if first == "" {
		return ""
	}
	if !isAddressLiteral(first) {
		return ""
	}
	return first
}
