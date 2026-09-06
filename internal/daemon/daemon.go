// Package daemon serves the kitbashd socket API: OTLP/HTTP on the standard
// paths and the small JSON API under /kitbash/v1, both defined by
// spec/kitbashd-api.yaml. It holds no MCP types; kitbash-mcp is a client of
// this HTTP surface, not a caller of these functions.
//
// Identity is the socket's peer credentials. Nothing a producer sends decides
// who it is, see PLAN.md section 2.4.
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
}

// Caller is the identity behind one request.
type Caller struct {
	Peer  Peer
	User  string
	Admin bool
}

// Server answers the socket API for every caller.
type Server struct {
	store   *store.Store
	version string
	admin   AdminFunc
	now     func() time.Time
	started time.Time
	mux     *http.ServeMux
}

// New builds the server. The store is not owned by it: whoever opened the file
// closes it.
func New(st *store.Store, opts Options) *Server {
	s := &Server{
		store:   st,
		version: opts.Version,
		admin:   opts.Admin,
		now:     opts.Now,
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
func (s *Server) routes() {
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/v1/traces", s.method(http.MethodPost, s.exportTraces))
	s.mux.HandleFunc("/v1/logs", s.method(http.MethodPost, s.exportLogs))
	s.mux.HandleFunc("/v1/metrics", s.method(http.MethodPost, s.exportMetrics))
	s.mux.HandleFunc("/kitbash/v1/query", s.method(http.MethodPost, s.query))
	s.mux.HandleFunc("/kitbash/v1/retention", s.retention)
	s.mux.HandleFunc("/kitbash/v1/health", s.method(http.MethodGet, s.health))
	s.mux.HandleFunc("/", s.notFound)
}

// Handler is the whole API, for a test that serves it over its own listener.
func (s *Server) Handler() http.Handler {
	return recovered(s.mux)
}

// Serve answers requests on ln until ctx is done, then drains in flight
// requests and returns. The listener is closed either way.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ConnContext:       withPeer,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          logger,
	}
	errs := make(chan error, 1)
	go func() { errs <- srv.Serve(ln) }()

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
