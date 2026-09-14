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

	"github.com/zyx1121/kitbash/internal/cgroups"
	"github.com/zyx1121/kitbash/internal/mounts"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
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

// RetryAfterSeconds is the Retry-After a 429 carries, in seconds.
const RetryAfterSeconds = "60"

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
	// Users is what the users family does to the host. Nil means the real
	// one, which runs useradd and its neighbours as root.
	Users sysusers.System
	// Runner starts and removes containers as their owner, which is what
	// every Process start, restore and removing a member need. Nil means the
	// real one.
	Runner sysusers.Runner
	// Cgroups is the delegated cgroup tree Processes are placed in, which is
	// what makes the manifest's limits an enforcement. Nil means the real
	// one, which is Linux only and turns itself off on a host whose cgroup
	// filesystem it cannot write.
	Cgroups cgroups.Cgroups
	// CgroupRoot is where the unified hierarchy is mounted. Empty means
	// cgroups.DefaultRoot.
	CgroupRoot string
	// EnvDir is where the environment file of one Process is written before
	// its container is started. Empty means DefaultEnvDir.
	EnvDir string
	// ProcessEndpoint is the address this host gives its Processes, which
	// they export Telemetry to and reach the MCP surface on. Empty means
	// DefaultProcessEndpoint. It exists for tests, the same way
	// KITBASH_TELEMETRY_ENDPOINT_FOR_PROCESSES does for a session.
	ProcessEndpoint string
	// Sessions starts one kitbash-mcp as the owner of a Process, which is
	// what an MCP session on the Process receiver is. Nil means the runner
	// when it starts one, and the real one otherwise.
	Sessions sysusers.Sessions
	// MCPBinary is the kitbash-mcp a session runs. Empty means the one
	// KITBASH_MCP_BINARY names, and then the path of the API spec.
	MCPBinary string
	// MCPIdle is how long an MCP session may go without a request. Zero means
	// MCPIdle, which is what spec/kitbashd-api.yaml declares.
	MCPIdle time.Duration
	// MCPCallerGrace is how long after a session ends its caller credential
	// still resolves, so the records its child flushed on the way out are not
	// stored without the Process that recorded them. Zero means CallerGrace.
	MCPCallerGrace time.Duration
	// HealthMinInterval is the floor every declared probe interval is held
	// to. Zero means MinHealthInterval. It exists for tests, which cannot
	// wait five seconds to see a second probe.
	HealthMinInterval time.Duration
	// OrgRoot is the shared root a mount source may name, /org on a kitbash
	// host. Empty means mounts.OrgRoot. It exists for tests, which have no
	// /org of their own to put a folder in.
	OrgRoot string
	// ProcRoot is the process table kitbashd reads a container's own mount
	// namespace through, /proc on a kitbash host. Empty means
	// DefaultProcRoot. It exists for tests, which run no containers and stage
	// a tree of their own in its shape.
	ProcRoot string
	// NoRestore stops the daemon from starting the registered Processes,
	// which an operator sets with KITBASH_NO_RESTORE to bring a host up
	// without its Processes, and a test sets to keep the runtime out of it.
	NoRestore bool
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
	users    sysusers.System
	runner   sysusers.Runner
	sessions sysusers.Sessions
	cgroups  cgroups.Cgroups
	envDir   string
	endpoint string
	// orgRoot is the shared root a mount source may name, see mounts in
	// spec/kitbashd-api.yaml.
	orgRoot string
	// procRoot is where this host's process table is mounted, which is how
	// kitbashd reads what a container has in its own mount namespace, see
	// mounts.go.
	procRoot string
	// actions serialises the start, stop and remove of one Process, see
	// run.go. Two of them at once on one id race on its environment file and
	// on its cgroup.
	actions *actionLock
	// fetches holds one image copy per member and digest, see images.go. The
	// second caller is refused rather than queued: a copy takes minutes.
	fetches *fetchLock
	// probes holds the Processes whose declared health path kitbashd
	// requests, see health.go. The loop that requests them is started by the
	// caller after restore.
	probes *prober

	// The MCP endpoint of the Process receiver: the handler of the SDK, the
	// live sessions, the binary each one runs and how long one may sit idle,
	// see mcp_for_processes in spec/kitbashd-api.yaml.
	mcpHandler  http.Handler
	mcpSessions *mcpRegistry
	mcpBinary   string
	mcpIdle     time.Duration
	callerGrace time.Duration

	// stopped ends the workers the daemon starts for itself, which is the
	// idle sweep of the MCP sessions. Close closes it exactly once.
	stopped   chan struct{}
	closeOnce sync.Once
	// teardown counts the session children that are being closed, which Close
	// waits for so a daemon that has stopped leaves none behind.
	teardown sync.WaitGroup

	noRestore bool

	// restoreProblems is why a Process is not running, by Process id: the
	// restore that could not bring it back writes one here and the next start
	// that works clears it. It is what processes_list answers as problem, so
	// proc_list can say failed with a reason its owner can act on rather than
	// leaving them to read the daemon log, which is the operator's.
	//
	// It is memory and not a column: it says what happened to this daemon, so
	// a daemon that starts again works it out again rather than reading a
	// verdict from a boot that is over.
	restoreMu       sync.Mutex
	restoreProblems map[string]restoreProblem

	// bound is what health reports as its listeners, written when a listener
	// starts serving and read by every health request.
	boundMu sync.Mutex
	bound   listeners

	// backups is what health reports about the nightly copy of the store,
	// written by the backup loop, see PLAN.md section 4.7.
	backups backups

	// internalCauses is the queue of causes waiting to be written, and
	// internalRate what bounds how many one peer may record, see
	// PLAN.md section 2.4.
	internalCauses chan internalCause
	internalRate   *rateLimiter
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
		users:    opts.Users,
		runner:   opts.Runner,
		sessions: opts.Sessions,
		cgroups:  opts.Cgroups,
		envDir:   opts.EnvDir,
		endpoint: opts.ProcessEndpoint,
		orgRoot:  opts.OrgRoot,
		procRoot: opts.ProcRoot,
		actions:  newActionLock(),
		fetches:  newFetchLock(),
		probes:   newProber(opts.HealthMinInterval),

		mcpSessions: newMCPRegistry(),
		mcpBinary:   mcpBinaryPath(opts.MCPBinary),
		mcpIdle:     opts.MCPIdle,
		callerGrace: opts.MCPCallerGrace,
		stopped:     make(chan struct{}),

		internalCauses: make(chan internalCause, InternalQueue),
		internalRate:   newRateLimiter(InternalRate, InternalRateWindow),

		noRestore:       opts.NoRestore,
		restoreProblems: map[string]restoreProblem{},
	}
	if s.runner == nil {
		s.runner = sysusers.NewPodman()
	}
	if s.cgroups == nil {
		s.cgroups = cgroups.New(opts.CgroupRoot)
	}
	if s.envDir == "" {
		s.envDir = DefaultEnvDir
	}
	if s.orgRoot == "" {
		s.orgRoot = mounts.OrgRoot
	}
	if s.procRoot == "" {
		s.procRoot = DefaultProcRoot
	}
	if s.sessions == nil {
		s.sessions = mcpSessionsOf(s.runner)
	}
	if s.mcpIdle <= 0 {
		s.mcpIdle = MCPIdle
	}
	if s.callerGrace <= 0 {
		s.callerGrace = CallerGrace
	}
	if s.users == nil {
		s.users = sysusers.NewHost(s.runner)
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
	go s.mcpSweepLoop()
	go s.internalWriter()
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
	s.mux.HandleFunc(processesPath+"/", s.process)
	s.mux.HandleFunc(buildsPath, s.builds)
	s.mux.HandleFunc(imagesPath+"/", s.image)
	s.mux.HandleFunc(usersPath, s.usersFamily)
	s.mux.HandleFunc(usersPath+"/", s.user)
	s.mux.HandleFunc(approvalsPath, s.approvalsFamily)
	s.mux.HandleFunc(approvalsPath+"/", s.approval)
	s.mux.HandleFunc(sessionsJoinPath, s.method(http.MethodPost, s.joinSession))
	s.mux.HandleFunc(healthPath, s.method(http.MethodGet, s.health))
	s.mux.HandleFunc(internalPath, s.method(http.MethodPost, s.internal))
	s.mux.HandleFunc("/", s.notFound)

	s.otlpMux = http.NewServeMux()
	s.otlpPaths(s.otlpMux, s.tokenIdentity)
	s.routesMCP(s.otlpMux)
	s.otlpMux.HandleFunc("/", s.notServedOverTCP)
}

// The paths of the JSON API, as spec/kitbashd-api.yaml names them.
const (
	queryPath     = "/kitbash/v1/query"
	retentionPath = "/kitbash/v1/retention"
	processesPath = "/kitbash/v1/processes"
	// buildsPath is the build record of every Package, and imagesPath the
	// copy of one image between two members' stores, see builds.go and
	// images.go.
	buildsPath    = "/kitbash/v1/builds"
	imagesPath    = "/kitbash/v1/images"
	usersPath     = "/kitbash/v1/users"
	approvalsPath = "/kitbash/v1/approvals"
	// sessionsJoinPath is where a session asks to be placed in its member's
	// cgroup, see run.go.
	sessionsJoinPath = "/kitbash/v1/sessions/join"
	healthPath       = "/kitbash/v1/health"
	internalPath     = "/kitbash/v1/internal"
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

// Close stops the fan out workers and ends every MCP session, which is what
// takes the kitbash-mcp children with it. The store is not owned by the
// server, so nothing here closes it.
func (s *Server) Close() {
	s.closeOnce.Do(func() { close(s.stopped) })
	s.closeMCPSessions()
	s.waitForChildren()
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
		if kind == listenerTCP {
			// The MCP sessions of this listener hold an event stream open
			// each, which is a request in flight that Shutdown would wait
			// out. Ending them first closes those streams and takes every
			// kitbash-mcp with them, which is what stopping the receiver
			// means: a session nobody serves any more is over.
			s.closeMCPSessions()
		}
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
