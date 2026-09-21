package teltest

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/zyx1121/kitbash/internal/podman"
)

// kitbashd runs the container of a Process, so the fake does too: a session
// registers a Process and then asks for it to be started, and what comes back
// has to look like a container in the member's own runtime, see
// spec/kitbashd-api.yaml processes_start.

// The environment the real daemon puts in every Process. The names are spelled
// here rather than imported so this fake stays a fake of the wire and not a
// second caller of the client it stands in for.
const (
	envEndpoint     = "KITBASH_TELEMETRY_ENDPOINT"
	envToken        = "KITBASH_TELEMETRY_TOKEN"
	envProcess      = "KITBASH_PROCESS"
	envPackage      = "KITBASH_PACKAGE"
	envUser         = "KITBASH_USER"
	envMCPEndpoint  = "KITBASH_MCP_ENDPOINT"
	envFanoutSecret = "KITBASH_FANOUT_SECRET"
)

// ownedEnv is what the daemon speaks for and a manifest cannot set.
var ownedEnv = []string{
	envEndpoint, envToken, envProcess, envPackage, envUser, envMCPEndpoint, envFanoutSecret,
}

// DefaultProcessEndpoint is the address the fake gives its Processes, the same
// one a kitbash host does.
const DefaultProcessEndpoint = "http://host.containers.internal:4318"

// StartOptions is the processes_start body, as the daemon reads it.
type StartOptions struct {
	Container string            `json:"container,omitempty"`
	Image     string            `json:"image,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Restart   string            `json:"restart,omitempty"`
	CPU       string            `json:"cpu,omitempty"`
	Memory    string            `json:"memory,omitempty"`
	PidsLimit int               `json:"pidsLimit,omitempty"`
	Publish   []PortMapping     `json:"publish,omitempty"`
	// Command is what the unit declared its container runs in place of the
	// command of its image, which podman is given after the image.
	Command []string `json:"command,omitempty"`
}

// PortMapping is one published port of a start request.
type PortMapping struct {
	HostPort      int `json:"hostPort,omitempty"`
	ContainerPort int `json:"containerPort"`
}

// StartCall is one recorded start, stop or remove.
type StartCall struct {
	Action    string
	ID        string
	Container string
	Options   StartOptions
	// Env is the environment the fake would have started the container with,
	// which is the request's own plus everything kitbashd speaks for.
	Env map[string]string
}

// MirrorRuns points the fake at a container runtime: every start it serves
// creates the container there, the way the daemon's podman run shows up in the
// member's own store, and every stop and remove is applied to it. Without one
// the fake only records.
func (d *Daemon) MirrorRuns(runner *podman.Fake) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runtime = runner
}

// SetProcessEndpoint replaces the address the fake gives its Processes.
func (d *Daemon) SetProcessEndpoint(endpoint string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.endpoint = endpoint
}

// AnswerStart replaces what the three actions return, which is how a daemon
// that refuses to run a Process is exercised. The zero Response restores the
// fake's own behaviour.
func (d *Daemon) AnswerStart(r Response) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.starts = r
}

// Starts are the start, stop and remove calls the fake served, in order.
func (d *Daemon) Starts() []StartCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]StartCall(nil), d.startCalls...)
}

// Started is the one start of a Process id, and false when there is not
// exactly one.
func (d *Daemon) Started(id string) (StartCall, bool) {
	var found StartCall
	seen := 0
	for _, call := range d.Starts() {
		if call.Action == startAction && call.ID == id {
			found = call
			seen++
		}
	}
	return found, seen == 1
}

// The three actions, as the path names them.
const (
	startAction  = "start"
	stopAction   = "stop"
	removeAction = "remove"
)

// start answers processes_start: it records the call, mirrors the container
// into the runtime if there is one, and answers the id that runtime gave it.
func (d *Daemon) start(w http.ResponseWriter, r *http.Request) {
	d.wait()
	body, err := readAll(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var opts StartOptions
	if len(body) > 0 {
		if err := json.Unmarshal(body, &opts); err != nil {
			write(w, Problem(http.StatusBadRequest, "bad-request", "Bad request",
				"the body is not a start request", ""))
			return
		}
	}
	d.action(w, r, startAction, opts, string(body))
}

// stop and remove answer the two actions that take no body.
func (d *Daemon) stop(w http.ResponseWriter, r *http.Request) {
	d.wait()
	d.action(w, r, stopAction, StartOptions{}, "")
}

func (d *Daemon) remove(w http.ResponseWriter, r *http.Request) {
	d.wait()
	d.action(w, r, removeAction, StartOptions{}, "")
}

// action is the body the three share: record, refuse an id nobody registered,
// then do it to the runtime the fake mirrors into.
func (d *Daemon) action(w http.ResponseWriter, r *http.Request, name string, opts StartOptions, body string) {
	id := r.PathValue("id")
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path, Body: body})
	reg, known := d.registrations[id]
	call := StartCall{Action: name, ID: id, Container: reg.Container, Options: opts}
	if name == startAction {
		call.Env = d.processEnv(reg, opts.Env)
	}
	d.startCalls = append(d.startCalls, call)
	override := d.starts
	runtime := d.runtime
	d.mu.Unlock()

	if override.Status != 0 {
		write(w, override)
		return
	}
	if !known {
		write(w, Problem(http.StatusNotFound, "not-found", "Not found",
			"kitbashd has no Process "+id+" registered", ""))
		return
	}
	container := reg.Container
	answer := map[string]string{"id": id, "container": container}
	if runtime != nil {
		switch name {
		case startAction:
			run := podman.RunOptions{
				Name:        container,
				Image:       opts.Image,
				Labels:      opts.Labels,
				Env:         call.Env,
				Restart:     opts.Restart,
				CPUs:        opts.CPU,
				Memory:      opts.Memory,
				PidsLimit:   opts.PidsLimit,
				Detach:      true,
				Interactive: true,
			}
			for _, port := range opts.Publish {
				run.Publish = append(run.Publish, podman.PortMapping{
					HostPort: port.HostPort, ContainerPort: port.ContainerPort,
				})
			}
			runtimeID, err := runtime.Run(r.Context(), run)
			if err != nil {
				write(w, Problem(http.StatusInternalServerError, "internal", "Internal error",
					err.Error(), ""))
				return
			}
			answer["containerId"] = runtimeID
		case stopAction:
			if err := runtime.Stop(r.Context(), container, 10); err != nil {
				write(w, Problem(http.StatusInternalServerError, "internal", "Internal error",
					err.Error(), ""))
				return
			}
		case removeAction:
			if err := runtime.Remove(r.Context(), container, true); err != nil {
				write(w, Problem(http.StatusInternalServerError, "internal", "Internal error",
					err.Error(), ""))
				return
			}
		}
	}
	encoded, _ := json.Marshal(answer)
	write(w, Response{Status: http.StatusOK, ContentType: "application/json", Body: string(encoded)})
}

// processEnv is the environment the daemon would start this container with:
// the request's own minus everything kitbashd speaks for, plus the Process's
// identity and the two credentials this fake minted. The caller holds the
// lock.
func (d *Daemon) processEnv(reg Registration, env map[string]string) map[string]string {
	merged := map[string]string{}
	for k, v := range env {
		merged[k] = v
	}
	for _, key := range ownedEnv {
		delete(merged, key)
	}
	endpoint := d.endpoint
	if endpoint == "" {
		endpoint = DefaultProcessEndpoint
	}
	merged[envEndpoint] = endpoint
	merged[envToken] = d.minted[reg.ID]
	merged[envProcess] = reg.ID
	merged[envPackage] = reg.Package
	merged[envUser] = d.identity.User
	merged[envMCPEndpoint] = strings.TrimSuffix(endpoint, "/") + "/mcp"
	if secret := d.secrets[reg.ID]; secret != "" {
		merged[envFanoutSecret] = secret
	}
	return merged
}
