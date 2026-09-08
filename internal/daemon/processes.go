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

var uuidV7 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// processRequest is the processes_register input of spec/kitbashd-api.yaml.
// Container and Digest are optional: a kitbash-mcp of the previous release
// registers without them, and a registration that carries no container is one
// restore leaves alone.
type processRequest struct {
	ID            string   `json:"id"`
	Package       string   `json:"package"`
	Name          string   `json:"name,omitempty"`
	Container     string   `json:"container,omitempty"`
	Digest        string   `json:"digest,omitempty"`
	Expose        string   `json:"expose"`
	Endpoint      string   `json:"endpoint,omitempty"`
	Subscriptions []string `json:"subscriptions,omitempty"`
}

// processResponse is what a registration answers. The token is returned once
// and never again: the store holds only its hash.
type processResponse struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

// processList is what processes_list answers, without a token anywhere in it.
type processList struct {
	Processes []store.Process `json:"processes"`
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
		Subscriptions: req.Subscriptions,
		RegisteredAt:  s.now().UTC(),
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
	writeJSON(w, r.URL.Path, processResponse{ID: p.ID, Token: token})
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
	writeJSON(w, r.URL.Path, processList{Processes: list})
}

// unregisterProcess answers DELETE /kitbash/v1/processes/{id}. Removing the
// record revokes the token, so a container that keeps exporting after its
// Process is gone is answered 401 rather than writing records nobody owns.
func (s *Server) unregisterProcess(w http.ResponseWriter, r *http.Request) {
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
	id := strings.TrimPrefix(r.URL.Path, processesPath+"/")
	if id == "" || strings.Contains(id, "/") {
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
	return validateEndpoint(instance, req.Endpoint)
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
