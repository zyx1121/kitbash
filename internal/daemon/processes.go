package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/mounts"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
)

// Exposures a Process can declare, see PLAN.md section 2.3.
const (
	ExposeMCP  = "mcp"
	ExposeHTTP = "http"
	ExposeNone = "none"
)

// LoopbackHost is the only host a fan out endpoint may name. A Process
// publishes its port on the host's loopback address, so an endpoint anywhere
// else would point kitbashd at the network and turn the fan out into a way to
// make the daemon talk to arbitrary hosts.
const LoopbackHost = "127.0.0.1"

// Bounds on what a registration may carry. They are far above what a real
// Process needs and stop one caller from filling the store with a field.
const (
	MaxPackageBytes   = 4096
	MaxNameBytes      = 256
	MaxEndpointBytes  = 256
	MaxSubscriptions  = 8
	MaxContainerBytes = 128
	// MaxHealthPathBytes bounds the path a probe requests. It is the path of
	// a URL kitbashd builds, and a health endpoint is named in a word.
	MaxHealthPathBytes = 256
)

// MaxProcessesPerMember is how many Processes one member may hold registered.
// Every one of them is a live token and, if it subscribes, a queue of up to
// QueueBytes, so the count is what bounds what one member costs the daemon. A
// member running more than a few dozen Processes on one machine has a
// different problem than this limit.
const MaxProcessesPerMember = 64

// uuidV7 is the identifier shape spec/kitbashd-api.yaml declares for a Process
// and for an approval, the same one internal/uuid writes.
// containerName and imageDigest are the two fields a registration gained in
// M5. Restore starts a container by the name the runtime holds it under, so
// the name is checked against the shape the runner writes and nothing else: it
// reaches a command line as an argument.
var (
	containerName = regexp.MustCompile(`^kitbash-[a-z0-9-]+$`)
	imageDigest   = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// healthInterval is how spec/manifest.schema.json spells an interval, which is
// what a registration carries through unchanged.
var healthInterval = regexp.MustCompile(`^[0-9]+(ms|s|m)$`)

var uuidV7 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// processRequest is the processes_register input of spec/kitbashd-api.yaml.
// Container and Digest are optional: a kitbash-mcp of the previous release
// registers without them, and a registration that carries no container is one
// restore leaves alone.
// Permits is what this Process may call back over /mcp, as the Package's
// manifest declares it. A registration without one permits nothing: an older
// kitbash-mcp sends no block, and the surface it would otherwise be given is
// its owner's whole one.
type processRequest struct {
	ID        string `json:"id"`
	Package   string `json:"package"`
	Name      string `json:"name,omitempty"`
	Container string `json:"container,omitempty"`
	Digest    string `json:"digest,omitempty"`
	Expose    string `json:"expose"`
	Endpoint  string `json:"endpoint,omitempty"`
	// Hostname is the one name deploy.units[0].hostname declared, which
	// kitbashd serves this Process under in place of the name it derives. A
	// name another Process on this host already holds is a conflict, see
	// proxy.go.
	Hostname      string           `json:"hostname,omitempty"`
	Subscriptions []string         `json:"subscriptions,omitempty"`
	Permits       manifest.Permits `json:"permits,omitempty"`
	// Runner is the Package path of the run kit that owns this Process, sent
	// by a session whose manifest named a runner. It carries no container,
	// because the Process runs wherever the kit put it: kitbashd registers it,
	// mints its token, and neither starts nor restores it, see PLAN.md
	// section 3.
	Runner string         `json:"runner,omitempty"`
	Health *healthRequest `json:"health,omitempty"`
	// Mounts are the folders of Files this unit declared, as the manifest
	// wrote them. They arrive unresolved and are resolved here, as root: the
	// session that sent them runs as the member, so what it says about a path
	// is a claim, see mounts.go.
	Mounts []mounts.Declared `json:"mounts,omitempty"`
	// Secrets are the names the unit declared, and only the names: the values
	// are the owner's and are read from root owned files at every start, so
	// none of them is ever in this body, see secrets.go.
	Secrets []string `json:"secrets,omitempty"`
}

// healthRequest is the probe a registration declares, which is the part of
// deploy.units[0].health kitbashd implements: the path it requests and how
// often. A registration that carries no block declares no probe, which is
// every registration written before probing existed and every Package that
// asks for none.
type healthRequest struct {
	HTTP     string `json:"http,omitempty"`
	Interval string `json:"interval,omitempty"`
}

// processResponse is what a registration answers. The token is returned once
// and never again: the store holds only its hash. The fan out secret is
// returned once as well, though the store keeps it: the caller has to put it in
// the container, and processes_list never carries it, see
// fan_out.authentication in spec/kitbashd-api.yaml.
type processResponse struct {
	ID           string `json:"id"`
	Token        string `json:"token"`
	FanoutSecret string `json:"fanoutSecret"`
	// Host is the name the reverse proxy serves this Process under, absent on
	// a host with no domain and for a Process that is not exposed over HTTP.
	// It is the daemon's answer rather than the declaration: the derived name
	// for a Process that declared none, the declared one for a Process that
	// did, see proxy.go.
	Host string `json:"host,omitempty"`
}

// processList is what processes_list answers, without a token anywhere in it.
type processList struct {
	Processes []listedProcess `json:"processes"`
}

// listedProcess is one registration as processes_list answers it: the record
// as it is stored, plus why this Process is not running if the last restore
// could not bring it back. The reason is the daemon's and not a column, see
// Server.restoreProblems; it is here so proc_list can report a Process that
// did not come back as failed with something its owner can act on.
type listedProcess struct {
	store.Process
	Problem string `json:"problem,omitempty"`
	Fix     string `json:"problemFix,omitempty"`
	// Host is the name the reverse proxy serves this Process under. Like the
	// problem it is not a column: it is derived from the registration and the
	// domain this host was given, so a host that gains a domain serves every
	// Process that was already registered under it, see proxy.go.
	Host string `json:"host,omitempty"`
}

// processes answers POST and GET on /kitbash/v1/processes.
func (s *Server) processes(w http.ResponseWriter, r *http.Request) {
	caller, prob := s.caller(r)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.registerProcess(w, r, caller)
	case http.MethodGet:
		s.listProcesses(w, r, caller)
	default:
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s %s is not part of the kitbashd API", r.Method, r.URL.Path),
			"Call POST to register a Process, GET to list them, or DELETE on the Process id to unregister one."))
	}
}

// registerProcess mints the token a Process exports with. The owner is the
// peer, and whether that member is an admin is captured here rather than read
// at export time: a Process keeps the privileges of the session that ran it,
// so removing a member from kitbash-admin does not silently change what their
// running kits may claim until they are run again, see PLAN.md section 2.4.
func (s *Server) registerProcess(w http.ResponseWriter, r *http.Request, caller Caller) {
	var req processRequest
	if prob := decodeBody(w, r, &req); prob != nil {
		writeProblem(w, prob)
		return
	}
	if prob := validateProcess(r.URL.Path, req); prob != nil {
		writeProblem(w, prob)
		return
	}

	token, hash, err := store.NewToken()
	if err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	// The secret is minted with the token and replaces whatever the previous
	// registration of this id held, so a container that lost its token lost
	// its secret too and a neighbour holding an old one is not believed.
	secret, err := store.NewFanoutSecret()
	if err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	// The mounts are resolved before the row is written, so a registration
	// that names a folder this member may not see is refused rather than
	// stored: a Process never holds a registration naming a folder the daemon
	// would refuse to mount, see mounts.go.
	resolved, prob := s.resolveMounts(r.Context(), r.URL.Path, caller.User, req.Mounts)
	if prob != nil {
		writeProblem(w, prob)
		return
	}

	p := store.Process{
		ID:            req.ID,
		Owner:         caller.User,
		Admin:         caller.Admin,
		Package:       req.Package,
		Name:          req.Name,
		Container:     req.Container,
		Digest:        req.Digest,
		Expose:        req.Expose,
		Endpoint:      req.Endpoint,
		Hostname:      req.Hostname,
		Subscriptions: req.Subscriptions,
		Runner:        req.Runner,
		Permits:       req.Permits,
		Health:        declaredHealth(req.Health),
		Mounts:        resolved,
		Secrets:       req.Secrets,
		FanoutSecret:  secret,
		RegisteredAt:  s.now().UTC(),
	}
	// A declared probe is checked against the container before the row is
	// written, so a registration kitbashd would not probe is refused rather
	// than stored as a declaration nothing acts on, see verifyProbe.
	verified, prob := s.checkedProbe(r, caller, p)

	if prob != nil {
		writeProblem(w, prob)
		return
	}
	// A declared host name is checked before the row is written. First the two
	// rules that do not need the network: a name under this host's own domain
	// is not a member's to declare, because under the domain there are derived
	// names and nothing else. Then the names the other Processes hold: one
	// name is one Process's, and the member who declared it second is the one
	// who can change it. Then the record itself, because a name that does not
	// point here is a name this host answers for nobody, see hostnames.go.
	if prob := s.declaredHostname(r.URL.Path, p); prob != nil {
		writeProblem(w, prob)
		return
	}
	if prob := s.hostConflict(r.Context(), r.URL.Path, p); prob != nil {
		writeProblem(w, prob)
		return
	}
	if prob := s.proveHostname(r.Context(), r.URL.Path, p); prob != nil {
		writeProblem(w, prob)
		return
	}
	if err := s.store.RegisterProcess(r.Context(), p, hash, MaxProcessesPerMember); err != nil {
		if errors.Is(err, store.ErrProcessOwned) {
			writeProblem(w, problem.ConflictFix(r.URL.Path,
				fmt.Sprintf("the Process %s belongs to another member", req.ID),
				"Register the Process under a new id."))
			return
		}
		if errors.Is(err, store.ErrTooManyProcesses) {
			writeProblem(w, problem.ConflictFix(r.URL.Path,
				fmt.Sprintf("%s already has %d Processes registered", caller.User, MaxProcessesPerMember),
				"Stop a Process you are no longer using, which unregisters it, then run this one."))
			return
		}
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	s.fanout.track(p)
	// The probe starts with the registration when the container already
	// publishes the endpoint, and its first request goes out at once: the
	// reading its owner is waiting for is the first one. A registration whose
	// container does not exist yet, which is what proc_run sends, is probed
	// from the start that creates it.
	if verified {
		s.probes.track(p, s.now())
	} else {
		s.probes.untrack(p.ID)
	}
	// The name is served from the registration, whether or not the container
	// exists yet: a Process that is registered and not running answers 503 on
	// its own name rather than looking like a name nobody holds.
	s.trackRouteFor(r.Context(), p)
	writeJSON(w, r.URL.Path, processResponse{
		ID: p.ID, Token: token, FanoutSecret: secret, Host: hostFor(p, s.proxy.domain),
	})
}

// checkedProbe holds one registration's declared probe to the ports its
// container publishes and reports whether kitbashd may probe it. The caller's
// own member record is what the runtime is asked as, because a member's
// containers are in their own store and nowhere else.
func (s *Server) checkedProbe(r *http.Request, caller Caller, p store.Process) (bool, *problem.Problem) {
	if !p.Health.Declared() {
		return false, nil
	}
	m, found, err := s.users.Lookup(r.Context(), caller.User)
	if err != nil {
		return false, problem.Internal(r.URL.Path, err.Error(), "")
	}
	if !found {
		return false, problem.NotPermitted(r.URL.Path,
			fmt.Sprintf("%s is not a member of this host, so kitbashd cannot check what their containers publish", caller.User),
			"Ask an administrator to create a member for this account.")
	}
	return s.verifyProbe(r.Context(), m, p, r.URL.Path)
}

// declaredHealth reads the probe of one registration. A block naming no
// path declares nothing, which is what a caller sending an interval alone
// asked for and is refused by validateProcess before it reaches here.
func declaredHealth(req *healthRequest) store.Health {
	if req == nil {
		return store.Health{}
	}
	return store.Health{HTTP: req.HTTP, Interval: req.Interval}
}

// listProcesses answers with the caller's Processes, or every member's for an
// admin, the same rule tel_query follows.
func (s *Server) listProcesses(w http.ResponseWriter, r *http.Request, caller Caller) {
	owner := caller.User
	if caller.Admin {
		owner = ""
	}
	list, err := s.store.Processes(r.Context(), owner)
	if err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	// One listing carries both of the things kitbashd knows and the store
	// does not: why a Process did not come back, and what its last health
	// probe saw.
	listed := make([]listedProcess, 0, len(list))
	for _, p := range s.withReadings(list) {
		entry := listedProcess{Process: p, Host: hostFor(p, s.proxy.domain)}
		if prob := s.processProblem(p.ID); prob.Detail != "" {
			entry.Problem = prob.Detail
			entry.Fix = prob.Fix
		}
		listed = append(listed, entry)
	}
	writeJSON(w, r.URL.Path, processList{Processes: listed})
}

// process answers everything under /kitbash/v1/processes/{id}: the DELETE that
// unregisters one, and the three actions that run the container runtime as its
// owner, see run.go.
func (s *Server) process(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, processesPath+"/")
	id, action, _ := strings.Cut(rest, "/")
	switch action {
	case actionStart, actionStop, actionRemove:
		s.processAction(w, r, id, action)
		return
	case "":
		s.unregisterProcess(w, r, id)
		return
	}
	writeProblem(w, problem.NotFoundFix(r.URL.Path,
		fmt.Sprintf("%s is not part of the kitbashd API", r.URL.Path),
		"Call DELETE on the Process id to unregister it, or POST start, stop or remove on it."))
}

// unregisterProcess answers DELETE /kitbash/v1/processes/{id}. Removing the
// record revokes the token, so a container that keeps exporting after its
// Process is gone is answered 401 rather than writing records nobody owns.
func (s *Server) unregisterProcess(w http.ResponseWriter, r *http.Request, id string) {
	caller, prob := s.caller(r)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	if r.Method != http.MethodDelete {
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s %s is not part of the kitbashd API", r.Method, r.URL.Path),
			"Call DELETE to unregister a Process."))
		return
	}
	if id == "" {
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s is not part of the kitbashd API", r.URL.Path),
			"Call DELETE on the Process id, without a further path."))
		return
	}

	p, found, err := s.store.Process(r.Context(), id)
	if err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	if !found {
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("kitbashd has no Process %s registered", id),
			"List the Processes to see which ids are registered."))
		return
	}
	// Owner only, including for an admin: spec/kitbashd-api.yaml gives this
	// path one rule and unregistering revokes a token the container is still
	// using, which is not a thing to do to another member by accident.
	if p.Owner != caller.User {
		writeProblem(w, problem.NotPermitted(r.URL.Path,
			fmt.Sprintf("the Process %s belongs to another member", id),
			"Unregister your own Processes; the member who ran this one unregisters it."))
		return
	}
	if _, err := s.store.DeleteProcess(r.Context(), id); err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	s.fanout.untrack(id)
	s.clearProcessProblem(id)
	// Nothing is probed for a Process nobody runs. A request already in
	// flight for it is answered to nobody, see probeOnce.
	s.probes.untrack(id)
	// And nothing is routed to it: the name a Process held is served while it
	// is registered and not a moment longer.
	s.proxy.untrack(id)
	// The token that opened them is revoked, so the sessions it opened are
	// over and the kitbash-mcp of each one exits.
	s.endMCPSessions(id)
	w.WriteHeader(http.StatusNoContent)
}

// LoadSubscribers reads the registered Processes and points the fan out at the
// ones that asked for Telemetry. The daemon calls it before it serves, so a
// restart picks up the subscribers of the Processes that survived it.
func (s *Server) LoadSubscribers(ctx context.Context) error {
	list, err := s.store.Processes(ctx, "")
	if err != nil {
		return err
	}
	s.fanout.reload(list)
	return nil
}

// validateProcess checks a registration before anything is written. Every rule
// here is one spec/kitbashd-api.yaml states; the endpoint rule is the one that
// matters for safety, see LoopbackHost.
func validateProcess(instance string, req processRequest) *problem.Problem {
	if !uuidV7.MatchString(req.ID) {
		return problem.BadRequest(instance,
			fmt.Sprintf("%q is not a Process id", req.ID),
			"Send the id as a version 7 UUID in its canonical 36 character form.")
	}
	if req.Package == "" || !strings.HasPrefix(req.Package, "/") || len(req.Package) > MaxPackageBytes {
		return problem.BadRequest(instance,
			fmt.Sprintf("%q is not a package path", req.Package),
			"Send the package as the absolute path of the folder it was built from.")
	}
	for _, segment := range strings.Split(req.Package, "/") {
		if segment == ".." {
			return problem.BadRequest(instance,
				"the package path contains a parent reference",
				"Send the package as an absolute path without any .. segment.")
		}
	}
	if len(req.Name) > MaxNameBytes {
		return problem.BadRequest(instance,
			fmt.Sprintf("the name is %d bytes, over the %d this API carries", len(req.Name), MaxNameBytes),
			"Send a shorter Process name.")
	}
	if req.Container != "" {
		if len(req.Container) > MaxContainerBytes || !containerName.MatchString(req.Container) {
			return problem.BadRequest(instance,
				fmt.Sprintf("%q is not a container name kitbash writes", req.Container),
				"Send the container as the name the runtime holds it under, which starts with kitbash- and carries lower case letters, digits and hyphens.")
		}
	}
	if req.Runner != "" {
		if !strings.HasPrefix(req.Runner, "/") || len(req.Runner) > MaxPackageBytes ||
			strings.Contains(req.Runner, "/../") || strings.HasSuffix(req.Runner, "/..") {
			return problem.BadRequest(instance,
				fmt.Sprintf("%q is not a package path", req.Runner),
				"Send the runner as the absolute path of the run kit's Package folder.")
		}
	}
	if req.Digest != "" && !imageDigest.MatchString(req.Digest) {
		return problem.BadRequest(instance,
			fmt.Sprintf("%q is not an image digest", req.Digest),
			"Send the digest as sha256: followed by 64 hexadecimal characters.")
	}
	switch req.Expose {
	case ExposeMCP, ExposeHTTP, ExposeNone:
	default:
		return problem.BadRequest(instance,
			fmt.Sprintf("%q is not an exposure", req.Expose),
			"Send expose as one of mcp, http or none.")
	}
	if len(req.Subscriptions) > MaxSubscriptions {
		return problem.BadRequest(instance,
			fmt.Sprintf("a Process may declare at most %d subscriptions", MaxSubscriptions),
			"Declare subscriptions: [telemetry], which is the only one that exists.")
	}
	for _, s := range req.Subscriptions {
		if s != store.SubscriptionTelemetry {
			return problem.BadRequest(instance,
				fmt.Sprintf("%q is not a subscription kitbashd offers", s),
				"Declare subscriptions: [telemetry], which is the only one that exists.")
		}
	}
	if prob := validateHealth(instance, req.Health); prob != nil {
		return prob
	}
	if prob := validateHostname(instance, req); prob != nil {
		return prob
	}
	// The count is held here rather than in the resolution, so a registration
	// declaring a hundred mounts is one refusal and not a hundred opens.
	if len(req.Mounts) > mounts.Max {
		return problem.BadRequest(instance,
			fmt.Sprintf("a Process may mount at most %d folders, not %d", mounts.Max, len(req.Mounts)),
			fmt.Sprintf("Declare at most %d entries in deploy.units[0].mounts.", mounts.Max))
	}
	// The declared secret names are held to their shape here, so a
	// registration kitbashd could never resolve is refused rather than stored
	// as a Process that fails every start. Whether the owner holds a value for
	// each name is not asked: a name set between this call and the start is a
	// value the start reads, and the start is what refuses, see secrets.go.
	if prob := validateSecrets(instance, req.Secrets); prob != nil {
		return prob
	}
	// A permits block kitbashd cannot honour is refused here rather than
	// stored: a glob nobody can read would otherwise sit in the registry
	// permitting nothing, and the member would look for the mistake in the
	// session instead of in the manifest.
	if err := req.Permits.Validate(); err != nil {
		return problem.BadRequest(instance, err.Error(),
			"Declare provides.permits.tools as tool name globs and provides.permits.paths as absolute path prefixes in the Package's kitbash.yaml.")
	}
	return validateEndpoint(instance, req.Endpoint)
}

// validateSecrets holds the declared names to the shape a manifest may carry:
// at most manifest.MaxSecrets of them, each an environment variable name, none
// of them one kitbashd speaks for and none of them declared twice. It is the
// same rule spec/manifest.schema.json and internal/manifest read, checked again
// here because a registration is a request and not a manifest: kitbash-mcp
// runs as the member, so what it sends about a unit is a claim.
func validateSecrets(instance string, names []string) *problem.Problem {
	refuse := func(detail string) *problem.Problem {
		return problem.BadRequest(instance, detail,
			fmt.Sprintf("Declare at most %d names in deploy.units[0].secrets, each matching ^[A-Z][A-Z0-9_]{0,63}$ and none of them a %s name.",
				manifest.MaxSecrets, manifest.OwnedEnvPrefix))
	}
	if len(names) > manifest.MaxSecrets {
		return refuse(fmt.Sprintf("a Process may declare at most %d secrets, not %d",
			manifest.MaxSecrets, len(names)))
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !manifest.ValidSecretName(name) {
			return refuse(fmt.Sprintf("%q is not a secret name", name))
		}
		if strings.HasPrefix(name, manifest.OwnedEnvPrefix) {
			return refuse(fmt.Sprintf("%s is a name kitbashd speaks for, so it is not one a member sets", name))
		}
		if seen[name] {
			return refuse(fmt.Sprintf("%s is declared twice", name))
		}
		seen[name] = true
	}
	return nil
}

// validateHostname checks the one name a unit declares for itself. It is held
// to the same shape spec/manifest.schema.json holds it to, checked again here
// because a registration is a request and not a manifest: kitbash-mcp runs as
// the member, so what it sends about a unit is a claim.
//
// Two rules beyond the shape. A name is served for a Process that is exposed
// over HTTP, because that is the Process a name leads to; declaring one on any
// other exposure is asking for something that does not exist. And a Process a
// run kit owns is not served by kitbashd at all: there is no container of it
// here, so the daemon has nothing to forward to, the same reason it probes
// none, see verifyProbe.
func validateHostname(instance string, req processRequest) *problem.Problem {
	if req.Hostname == "" {
		return nil
	}
	if !manifest.ValidHostname(req.Hostname) {
		return problem.BadRequest(instance,
			fmt.Sprintf("%q is not a host name kitbashd serves", req.Hostname),
			fmt.Sprintf("Declare deploy.units[0].hostname as a lower case DNS name of at least two labels and at most %d bytes, such as app.example.org.", manifest.MaxHostname))
	}
	if req.Expose != ExposeHTTP {
		return problem.BadRequest(instance,
			fmt.Sprintf("this unit declares the host name %s and is exposed as %s, so there is nothing at that name to reach",
				req.Hostname, req.Expose),
			"Declare deploy.units[0].expose: http beside the hostname, or remove the hostname.")
	}
	if req.Runner != "" {
		return problem.NotPermitted(instance,
			"a Process a run kit owns is not served by kitbashd, so it has no host name here",
			fmt.Sprintf("Remove deploy.units[0].hostname from this Package, or have %s publish the Process it runs.", req.Runner))
	}
	return nil
}

// validateHealth checks the declared probe. kitbashd requests this path on the
// Process's own endpoint, so what it has to be is a path: a registration that
// named a whole URL would be asking the daemon to request something else.
func validateHealth(instance string, req *healthRequest) *problem.Problem {
	if req == nil {
		return nil
	}
	refuse := func(detail string) *problem.Problem {
		return problem.BadRequest(instance, detail, healthFix)
	}
	if req.HTTP == "" {
		return refuse("the health block names no path to request")
	}
	if len(req.HTTP) > MaxHealthPathBytes {
		return refuse(fmt.Sprintf("the health path is %d bytes, over the %d this API carries",
			len(req.HTTP), MaxHealthPathBytes))
	}
	if !strings.HasPrefix(req.HTTP, "/") {
		return refuse(fmt.Sprintf("%q is not a path on the Process's endpoint", req.HTTP))
	}
	for _, r := range req.HTTP {
		if r <= ' ' || r == 0x7f {
			return refuse(fmt.Sprintf("%q carries a character a request line cannot", req.HTTP))
		}
	}
	if req.Interval != "" && !healthInterval.MatchString(req.Interval) {
		return refuse(fmt.Sprintf("%q is not an interval", req.Interval))
	}
	return nil
}

// validateEndpoint refuses anything but a loopback HTTP endpoint. The fan out
// POSTs to whatever this names, so it must never be pointed at the network.
func validateEndpoint(instance, endpoint string) *problem.Problem {
	if endpoint == "" {
		return nil
	}
	refuse := func(detail string) *problem.Problem {
		return problem.BadRequest(instance, detail,
			fmt.Sprintf("Send the endpoint as http://%s:PORT, the address the Process publishes on the host.", LoopbackHost))
	}
	if len(endpoint) > MaxEndpointBytes {
		return refuse(fmt.Sprintf("the endpoint is %d bytes, over the %d this API carries", len(endpoint), MaxEndpointBytes))
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return refuse(fmt.Sprintf("%q is not a URL", endpoint))
	}
	if u.Scheme != "http" {
		return refuse(fmt.Sprintf("%q is not an http endpoint", endpoint))
	}
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return refuse(fmt.Sprintf("%q carries more than a host and a port", endpoint))
	}
	host, port, err := splitHostPort(u.Host)
	if err != nil || host != LoopbackHost {
		return refuse(fmt.Sprintf("%q does not name the loopback address; kitbashd delivers Telemetry to %s only",
			endpoint, LoopbackHost))
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return refuse(fmt.Sprintf("%q does not name a port", endpoint))
	}
	return nil
}

// splitHostPort is net.SplitHostPort with an error rather than a panic for a
// host that carries no port at all.
func splitHostPort(hostport string) (string, string, error) {
	i := strings.LastIndex(hostport, ":")
	if i < 0 {
		return "", "", fmt.Errorf("daemon: %q carries no port", hostport)
	}
	return hostport[:i], hostport[i+1:], nil
}
