// Package daemon serves the kitbashd socket API: OTLP/HTTP on the standard
// paths and the small JSON API under /kitbash/v1, both defined by
// spec/kitbashd-api.yaml. It holds no MCP types; kitbash-mcp is a client of
// this HTTP surface, not a caller of these functions.
//
// It serves two listeners: the unix socket, where identity is the peer
// credentials, and the Process receiver on TCP, where identity is a bearer
// token minted at registration. Nothing a producer sends decides who it is,
// see PLAN.md section 2.4.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"sync"
	"time"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
)

// AdminGroup is the group whose members read everyone's Telemetry and set
// retention, see PLAN.md section 4.5.
const AdminGroup = "kitbash-admin"

// SweepEvery is how often retention runs after the sweep on start.
const SweepEvery = time.Hour

// ShutdownTimeout is how long in flight requests get when the daemon stops.
const ShutdownTimeout = 5 * time.Second

// QueryWindow is how far back a query reaches when it names no since.
const QueryWindow = 24 * time.Hour

// ProblemContentType is the media type of every error this API returns.
const ProblemContentType = "application/problem+json"

// MaxConnections is how many connections the daemon serves at once. Every
// member session holds one, so the cap is far above what a host with fifty
// members needs, and it stops one caller from spending every file descriptor
// the daemon has.
const MaxConnections = 256

// Timeouts bound how long one connection may hold a file descriptor. Without
// them an idle keep alive connection lives forever and a body dribbled a byte
// at a time keeps a handler running as long as the caller likes.
type Timeouts struct {
	// ReadHeader is how long the request line and headers may take.
	ReadHeader time.Duration
	// Read is how long the whole request, body included, may take.
	Read time.Duration
	// Write is how long a response may take.
	Write time.Duration
	// Idle is how long a kept alive connection may sit between requests.
	Idle time.Duration
}

// DefaultTimeouts are what kitbashd serves with. An export of 4 MiB over a
// unix socket takes milliseconds, so thirty seconds is generous.
func DefaultTimeouts() Timeouts {
	return Timeouts{
		ReadHeader: 5 * time.Second,
		Read:       30 * time.Second,
		Write:      30 * time.Second,
		Idle:       60 * time.Second,
	}
}

// withDefaults fills in whatever the caller left at zero.
func (t Timeouts) withDefaults() Timeouts {
	d := DefaultTimeouts()
	if t.ReadHeader <= 0 {
		t.ReadHeader = d.ReadHeader
	}
	if t.Read <= 0 {
		t.Read = d.Read
	}
	if t.Write <= 0 {
		t.Write = d.Write
	}
	if t.Idle <= 0 {
		t.Idle = d.Idle
	}
	return t
}

// logger writes where the operator reads, the same shape internal/problem uses.
var logger = log.New(os.Stderr, "kitbashd: ", log.LstdFlags)

// AdminFunc decides whether a caller is an administrator. The default reads
// the system groups; a test injects its own rather than creating a group.
type AdminFunc func(u *user.User) (bool, error)

// Options are what the daemon needs beyond its store.
type Options struct {
	// Version is what health reports. It is the binary's build version.
	Version string
	// Admin overrides the system group lookup. Nil means the real one.
	Admin AdminFunc
	// Now overrides the clock, for tests. Nil means time.Now.
	Now func() time.Time
	// Timeouts bound one connection. A zero field takes its default.
	Timeouts Timeouts
	// MaxConnections caps concurrent connections. Zero means the default.
	MaxConnections int
}

// Caller is the identity behind one request.
type Caller struct {
	Peer  Peer
	User  string
	Admin bool
}

// Server answers the socket API for every caller, and the OTLP paths of the
// Process receiver for every Process with a token.
type Server struct {
	store    *store.Store
	version  string
	admin    AdminFunc
	now      func() time.Time
	started  time.Time
	mux      *http.ServeMux
	otlpMux  *http.ServeMux
	timeouts Timeouts
	maxConns int
	fanout   *fanout

	// bound is what health reports as its listeners, written when a listener
	// starts serving and read by every health request.
	boundMu sync.Mutex
	bound   listeners
}

// New builds the server. The store is not owned by it: whoever opened the file
// closes it.
func New(st *store.Store, opts Options) *Server {
	s := &Server{
		store:    st,
		version:  opts.Version,
		admin:    opts.Admin,
		now:      opts.Now,
		timeouts: opts.Timeouts.withDefaults(),
		maxConns: opts.MaxConnections,
		fanout:   newFanout(),
	}
	if s.maxConns <= 0 {
		s.maxConns = MaxConnections
	}
	if s.version == "" {
		s.version = "dev"
	}
	if s.admin == nil {
		s.admin = systemAdmin
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.started = s.now()
	s.routes()
	return s
}

// routes registers every path spec/kitbashd-api.yaml declares. A path is
// registered without its method so a wrong method answers with problem details
// like everything else, rather than the mux's plain text 405.
//
// There are two muxes because there are two listeners. The socket carries the
// whole API and identifies its caller by peer credentials; the Process
// receiver carries the three OTLP paths and identifies its caller by token.
// Nothing under /kitbash/v1 is reachable over TCP.
func (s *Server) routes() {
	s.mux = http.NewServeMux()
	s.otlpPaths(s.mux, s.socketIdentity)
	s.mux.HandleFunc(queryPath, s.method(http.MethodPost, s.query))
	s.mux.HandleFunc(retentionPath, s.retention)
	s.mux.HandleFunc(processesPath, s.processes)
	s.mux.HandleFunc(processesPath+"/", s.unregisterProcess)
	s.mux.HandleFunc(healthPath, s.method(http.MethodGet, s.health))
	s.mux.HandleFunc("/", s.notFound)

	s.otlpMux = http.NewServeMux()
	s.otlpPaths(s.otlpMux, s.tokenIdentity)
	s.otlpMux.HandleFunc("/", s.notServedOverTCP)
}

// The paths of the JSON API, as spec/kitbashd-api.yaml names them.
const (
	queryPath     = "/kitbash/v1/query"
	retentionPath = "/kitbash/v1/retention"
	processesPath = "/kitbash/v1/processes"
	healthPath    = "/kitbash/v1/health"
)

// otlpPaths registers the three OTLP paths on one mux, with the identity that
// listener reads.
func (s *Server) otlpPaths(mux *http.ServeMux, ident identifier) {
	mux.HandleFunc(pathTraces, s.method(http.MethodPost, s.export(ident, decodeTraces)))
	mux.HandleFunc(pathLogs, s.method(http.MethodPost, s.export(ident, decodeLogs)))
	mux.HandleFunc(pathMetrics, s.method(http.MethodPost, s.export(ident, decodeMetrics)))
}

// Handler is the whole API, for a test that serves it over its own listener.
func (s *Server) Handler() http.Handler {
	return recovered(s.mux)
}

// TCPHandler is the Process receiver, for a test that serves it over its own
// listener.
func (s *Server) TCPHandler() http.Handler {
	return recovered(s.otlpMux)
}

// Close stops the fan out workers. The store is not owned by the server, so
// nothing here closes it.
func (s *Server) Close() {
	s.fanout.close()
}

// listeners is what health reports about where the daemon is bound.
func (s *Server) listeners() listeners {
	s.boundMu.Lock()
	defer s.boundMu.Unlock()
	return s.bound
}

// bind records a listener's address for health and clears it when that
// listener stops.
func (s *Server) bind(kind, address string) {
	s.boundMu.Lock()
	defer s.boundMu.Unlock()
	switch kind {
	case listenerSocket:
		s.bound.Socket = address
	case listenerTCP:
		s.bound.TCP = address
	}
}

// The two listeners health names.
const (
	listenerSocket = "socket"
	listenerTCP    = "tcp"
)

// Serve answers requests on ln until ctx is done, then drains in flight
// requests and returns. The listener is closed either way.
//
// Every connection is bounded in two ways: the listener accepts no more than
// MaxConnections at once, and each one is subject to the timeouts. A daemon
// that runs for months cannot afford a connection that never ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	return s.serve(ctx, ln, listenerSocket, s.Handler())
}

// ServeTCP answers the Process receiver on ln until ctx is done. It is the
// same OTLP path as the socket with the same limits, and the token in the
// Authorization header is the whole identity, see PLAN.md section 4.5.
//
// The host firewall decides who may reach this port. kitbashd installs none:
// an unknown token is refused here, and that is the only claim this listener
// makes.
func (s *Server) ServeTCP(ctx context.Context, ln net.Listener) error {
	return s.serve(ctx, ln, listenerTCP, s.TCPHandler())
}

// serve runs one listener with the timeouts and the connection cap both
// listeners share.
func (s *Server) serve(ctx context.Context, ln net.Listener, kind string, handler http.Handler) error {
	s.bind(kind, ln.Addr().String())
	defer s.bind(kind, "")
	srv := &http.Server{
		Handler:           handler,
		ConnContext:       withPeer,
		ReadHeaderTimeout: s.timeouts.ReadHeader,
		ReadTimeout:       s.timeouts.Read,
		WriteTimeout:      s.timeouts.Write,
		IdleTimeout:       s.timeouts.Idle,
		ErrorLog:          logger,
	}
	errs := make(chan error, 1)
	go func() { errs <- srv.Serve(limit(ln, s.maxConns)) }()

	select {
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("daemon: serve: %w", err)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			srv.Close()
			return fmt.Errorf("daemon: shutdown: %w", err)
		}
		return nil
	}
}

// Sweep enforces retention once and logs what it deleted. It runs on start and
// then on the ticker, see PLAN.md section 2.4.
func (s *Server) Sweep(ctx context.Context) (store.SweepCounts, error) {
	counts, err := s.store.Sweep(ctx, s.now())
	if err != nil {
		return counts, err
	}
	if counts.Total() > 0 {
		logger.Printf("retention swept %d traces, %d logs, %d metrics",
			counts.Traces, counts.Logs, counts.Metrics)
	}
	return counts, nil
}

// SweepLoop runs Sweep on the interval until ctx is done. The caller sweeps
// once before serving; this is the hourly one.
func (s *Server) SweepLoop(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = SweepEvery
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				logger.Printf("retention sweep failed: %v", err)
			}
		}
	}
}

// method wraps a handler so any other verb on that path is not-found rather
// than a response shape this API does not define.
func (s *Server) method(verb string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != verb {
			writeProblem(w, problem.NotFoundFix(r.URL.Path,
				fmt.Sprintf("%s %s is not part of the kitbashd API", r.Method, r.URL.Path),
				fmt.Sprintf("Call %s %s instead.", verb, r.URL.Path)))
			return
		}
		h(w, r)
	}
}

// notFound answers every path the API does not define.
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, problem.NotFoundFix(r.URL.Path,
		fmt.Sprintf("%s is not part of the kitbashd API", r.URL.Path),
		"See spec/kitbashd-api.yaml for the paths kitbashd serves."))
}

// recovered turns a panic into problem details rather than a dropped
// connection, so a client always reads a shape it understands.
func recovered(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				writeProblem(w, problem.Internal(r.URL.Path, fmt.Sprintf("panic: %v", v), ""))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// caller resolves who is on the other end of the socket. An uid with no user
// on this host is refused: kitbash has no identity outside /etc/passwd.
func (s *Server) caller(r *http.Request) (Caller, *problem.Problem) {
	peer, err := peerFrom(r.Context())
	if err != nil {
		return Caller{}, problem.NotPermitted(r.URL.Path,
			"kitbashd could not read the peer credentials of this connection",
			"Connect to the kitbashd unix socket, not to a TCP port.")
	}
	u, err := user.LookupId(strconv.FormatUint(uint64(peer.UID), 10))
	if err != nil {
		return Caller{}, problem.NotPermitted(r.URL.Path,
			fmt.Sprintf("uid %d is not a user on this host", peer.UID),
			"Ask an administrator to create a member for this uid.")
	}
	admin, err := s.admin(u)
	if err != nil {
		return Caller{}, problem.Internal(r.URL.Path,
			fmt.Sprintf("group lookup for %s: %v", u.Username, err), "")
	}
	return Caller{Peer: peer, User: u.Username, Admin: admin}, nil
}

// systemAdmin reads the caller's groups from the host.
func systemAdmin(u *user.User) (bool, error) {
	ids, err := u.GroupIds()
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		g, err := user.LookupGroupId(id)
		if err != nil {
			continue
		}
		if g.Name == AdminGroup {
			return true, nil
		}
	}
	return false, nil
}
