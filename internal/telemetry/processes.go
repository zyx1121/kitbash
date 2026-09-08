package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/zyx1121/kitbash/internal/problem"
)

// ProcessesPath is the Process registry of spec/kitbashd-api.yaml. Registering
// a Process is what mints its Telemetry token, so a Process that was never
// registered is a producer of nothing.
const ProcessesPath = "/kitbash/v1/processes"

// The environment every Process is started with, see the
// environment_given_to_every_process block of spec/kitbashd-api.yaml. The
// manifest cannot override these: they are the Process's identity, not its
// configuration.
const (
	EnvEndpoint    = "KITBASH_TELEMETRY_ENDPOINT"
	EnvToken       = "KITBASH_TELEMETRY_TOKEN"
	EnvProcess     = "KITBASH_PROCESS"
	EnvPackage     = "KITBASH_PACKAGE"
	EnvUser        = "KITBASH_USER"
	EnvMCPEndpoint = "KITBASH_MCP_ENDPOINT"
	// EnvFanoutSecret is the bearer every fan out request to this Process
	// carries. A subscriber compares it with the header and takes records from
	// nothing else, see fan_out.authentication in spec/kitbashd-api.yaml. It
	// is set only for a daemon that mints one; an older one answers a
	// registration without it and the Process runs as it did before.
	EnvFanoutSecret = "KITBASH_FANOUT_SECRET"
)

// ProcessEndpoint is the address a rootless container reaches kitbashd's OTLP
// receiver on. It is not the unix socket: rootless networking delivers
// host.containers.internal to the host's primary address, so a Process exports
// over TCP with a token while a session exports over the socket.
const ProcessEndpoint = "http://host.containers.internal:4318"

// ProcessEndpointEnv overrides that address. It exists for tests, so it is
// honoured only when the process is not serving an SSH session, the same rule
// as SocketEnv and fs.RootsEnv.
const ProcessEndpointEnv = "KITBASH_TELEMETRY_ENDPOINT_FOR_PROCESSES"

// MCPPath is where kitbashd serves MCP over streamable HTTP on the Process
// receiver, so every Process can reach the surface as its owner, see PLAN.md
// section 2.3 and mcp_for_processes in spec/kitbashd-api.yaml.
const MCPPath = "/mcp"

// MCPEndpointForProcesses is the MCP endpoint this host gives its Processes.
// It is the Process receiver's own address with the MCP path on it, so the
// test override of the OTLP endpoint moves both at once: they are one
// listener, and a test that redirects one and not the other is testing a host
// that does not exist.
func MCPEndpointForProcesses() string {
	return strings.TrimSuffix(EndpointForProcesses(), "/") + MCPPath
}

// EndpointForProcesses is the OTLP endpoint this host gives its Processes.
func EndpointForProcesses() string {
	if env := os.Getenv(ProcessEndpointEnv); env != "" && os.Getenv(sshEnv) == "" {
		return env
	}
	return ProcessEndpoint
}

// Registration is one Process as kitbashd records it. It is the request body
// of processes_register in spec/kitbashd-api.yaml; the owner is not in it,
// because kitbashd reads that from the socket's peer credentials.
// Container and Digest are what boot restore needs: the container to start
// and the image it was started from, so kitbashd restores a Process without
// reading its manifest, see registration_fields in spec/kitbashd-api.yaml.
// Both are omitted when the caller does not know them.
type Registration struct {
	ID            string   `json:"id"`
	Package       string   `json:"package"`
	Name          string   `json:"name"`
	Container     string   `json:"container,omitempty"`
	Digest        string   `json:"digest,omitempty"`
	Expose        string   `json:"expose"`
	Endpoint      string   `json:"endpoint,omitempty"`
	Subscriptions []string `json:"subscriptions,omitempty"`
}

// The shapes the two restore fields must have. kitbashd starts what the
// container field names, as the owner, at boot: a name that is not a kitbash
// container name, or a digest that is not an OCI image id, is a registration
// this process got wrong and must not send.
var (
	containerName = regexp.MustCompile(`^kitbash-[a-z0-9-]+$`)
	imageDigest   = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// check holds the registration to those shapes. The other fields are checked
// by kitbashd, which is the one that has to trust them; these two are checked
// here as well because a wrong value is not a caller's mistake but this
// program's, and it would be found at the next boot rather than now.
func (r Registration) check() *problem.Problem {
	if r.Container != "" && !containerName.MatchString(r.Container) {
		return problem.BadRequest(r.ID,
			fmt.Sprintf("%q is not a kitbash container name", r.Container), "")
	}
	if r.Digest != "" && !imageDigest.MatchString(r.Digest) {
		return problem.BadRequest(r.ID,
			fmt.Sprintf("%q is not an OCI image digest", r.Digest), "")
	}
	return nil
}

// Registered is one Process kitbashd knows about, as processes_list returns
// it. Tokens are returned once, at registration, and are never listed.
//
// Owner is the member kitbashd recorded from the socket's peer credentials at
// registration. A member's list is their own, so it is an admin's list that
// carries Processes with an owner other than the caller.
type Registered struct {
	ID            string   `json:"id"`
	Package       string   `json:"package"`
	Name          string   `json:"name"`
	Expose        string   `json:"expose,omitempty"`
	Endpoint      string   `json:"endpoint,omitempty"`
	Subscriptions []string `json:"subscriptions,omitempty"`
	Owner         string   `json:"owner,omitempty"`
	Admin         bool     `json:"admin,omitempty"`
	RegisteredAt  string   `json:"registeredAt,omitempty"`
}

// UnmarshalJSON reads a listed Process, accepting user as a spelling of owner.
// The daemon answers owner; the older spelling costs one line to keep and the
// alternative is an owner filter that silently sees nothing.
func (r *Registered) UnmarshalJSON(data []byte) error {
	type listed Registered
	var raw struct {
		listed
		User string `json:"user"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*r = Registered(raw.listed)
	if r.Owner == "" {
		r.Owner = raw.User
	}
	return nil
}

// registerResponse is what processes_register answers. The fan out secret is
// returned beside the token and, like it, only here: processes_list carries
// neither. A daemon of an earlier release answers without one, and a Process
// started from that answer runs with an unauthenticated fan out.
type registerResponse struct {
	ID           string `json:"id"`
	Token        string `json:"token"`
	FanoutSecret string `json:"fanoutSecret"`
}

// processList is the object form of a Process list. The array form is accepted
// too, so this client holds no opinion on which one kitbashd answers with.
type processList struct {
	Processes []Registered `json:"processes"`
}

// RegisterProcess registers one Process and returns its Telemetry token and
// its fan out secret. Both are returned once: the token is the only proof the
// Process has that it may export, the secret the only proof it has that a
// delivery came from kitbashd. The caller injects them into the container and
// keeps no copy.
//
// An empty secret is not a failure. A daemon of an earlier release answers a
// registration without one, and a Process is not worth refusing to start over
// a fan out that is authenticated the way it was last week.
//
// A host without kitbashd is not a reason to refuse to start a container: the
// container runtime does not depend on the daemon. The caller logs the problem
// and starts the Process untraced.
func (c *Client) RegisterProcess(ctx context.Context, reg Registration) (token, fanoutSecret string, prob *problem.Problem) {
	if c == nil {
		return "", "", problem.Internal(reg.ID, "telemetry is not configured for this session", NotRunningFix)
	}
	if prob := reg.check(); prob != nil {
		return "", "", prob
	}
	body, err := json.Marshal(reg)
	if err != nil {
		return "", "", problem.Internal(reg.ID, err.Error(), "")
	}
	status, payload, prob := c.send(ctx, http.MethodPost, ProcessesPath, body, reg.ID)
	if prob != nil {
		return "", "", prob
	}
	if status >= 300 {
		return "", "", c.failure(reg.ID, statusText(status), payload)
	}
	var answer registerResponse
	if err := json.Unmarshal(payload, &answer); err != nil {
		return "", "", problem.Internal(reg.ID,
			fmt.Sprintf("%s answered with a body that is not a registration: %v", ProcessesPath, err), "")
	}
	if answer.Token == "" {
		return "", "", problem.Internal(reg.ID,
			fmt.Sprintf("%s answered without a token", ProcessesPath), "")
	}
	return answer.Token, answer.FanoutSecret, nil
}

// UnregisterProcess revokes a Process's token. A Process kitbashd does not
// know is already unregistered, so 404 is not a failure for the caller: a stop
// after a daemon restart has to succeed.
func (c *Client) UnregisterProcess(ctx context.Context, id string) *problem.Problem {
	if c == nil {
		return problem.Internal(id, "telemetry is not configured for this session", NotRunningFix)
	}
	if id == "" {
		return problem.Internal(id, "no Process id was given to unregister", "")
	}
	status, payload, prob := c.send(ctx, http.MethodDelete, ProcessesPath+"/"+id, nil, id)
	if prob != nil {
		return prob
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status >= 300 {
		return c.failure(id, statusText(status), payload)
	}
	return nil
}

// ListProcesses is every Process kitbashd has registered for the caller. The
// session compares it with the containers that are actually running, see
// client_behaviour.processes in spec/kitbashd-api.yaml.
func (c *Client) ListProcesses(ctx context.Context) ([]Registered, *problem.Problem) {
	if c == nil {
		return nil, problem.Internal("processes", "telemetry is not configured for this session", NotRunningFix)
	}
	status, payload, prob := c.send(ctx, http.MethodGet, ProcessesPath, nil, "processes")
	if prob != nil {
		return nil, prob
	}
	if status >= 300 {
		return nil, c.failure("processes", statusText(status), payload)
	}
	var wrapped processList
	if err := json.Unmarshal(payload, &wrapped); err == nil {
		return wrapped.Processes, nil
	}
	var bare []Registered
	if err := json.Unmarshal(payload, &bare); err != nil {
		return nil, problem.Internal("processes",
			fmt.Sprintf("%s answered with a body that is not a Process list: %v", ProcessesPath, err), "")
	}
	return bare, nil
}
