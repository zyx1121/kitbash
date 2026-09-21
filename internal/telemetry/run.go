package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/zyx1121/kitbash/internal/problem"
)

// kitbashd runs the container of a Process, not the session: a member cannot
// place their own container in a delegated cgroup, so the limits a manifest
// declares are only enforced when root starts it. These three calls are the
// client half of processes_start, processes_stop and processes_remove in
// spec/kitbashd-api.yaml, see PLAN.md section 2.3.

// The three actions, as the last segment of a Process's path.
const (
	startAction  = "start"
	stopAction   = "stop"
	removeAction = "remove"
)

// What one action is given to come back. kitbashd is not answering out of its
// own store here: it is waiting for podman to create a container, or to let a
// PID 1 that ignores SIGTERM run out its whole stop grace. Both are wider than
// the budget the daemon gives the child, so the answer arrives rather than the
// deadline, see internal/sysusers.
const (
	startTimeout  = 150 * time.Second
	actionTimeout = 60 * time.Second
)

// SessionsJoinPath is where a session asks kitbashd to place it in its
// member's cgroup, see sessions_join in spec/kitbashd-api.yaml. Without it a
// session cannot exec into a container kitbashd started: moving a process
// between cgroups needs write access to the common ancestor's cgroup.procs,
// and a session sshd started shares only the root cgroup with anything of
// kitbash's.
const SessionsJoinPath = "/kitbash/v1/sessions/join"

// JoinSession asks kitbashd to place this process in its member's cgroup. It
// is called once, at startup, and carries no body: the process it places is
// the peer of the socket, which the kernel says and a caller cannot claim.
//
// A session that is not placed still works. Everything but exec into a
// container kitbashd started is unaffected, and that says so when it is tried,
// so this is one line in the log rather than a session that will not start.
func (c *Client) JoinSession(ctx context.Context) *problem.Problem {
	if c == nil {
		return problem.Internal("sessions_join", "telemetry is not configured for this session", NotRunningFix)
	}
	status, payload, prob := c.send(ctx, http.MethodPost, SessionsJoinPath, nil, "sessions_join")
	if prob != nil {
		return prob
	}
	if status >= 300 {
		return c.failure("sessions_join", statusText(status), payload)
	}
	return nil
}

// StartOptions is the command line the session would have run, as
// processes_start carries it. The environment here is the manifest's own:
// kitbashd writes the Telemetry token, the fan out secret and the Process's
// identity itself, so no credential is ever in this body.
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
	// command of its image. It is the one thing in this body that lands on
	// podman's command line, which is where the words after an image go.
	Command []string `json:"command,omitempty"`
}

// PortMapping is one published port. A host port of zero leaves the choice to
// the runtime.
type PortMapping struct {
	HostPort      int `json:"hostPort,omitempty"`
	ContainerPort int `json:"containerPort"`
}

// startResponse is what the three actions answer.
type startResponse struct {
	ID          string `json:"id"`
	Container   string `json:"container"`
	ContainerID string `json:"containerId"`
}

// StartProcess asks kitbashd to run the container of a registered Process as
// its owner, and answers the id the runtime gave it. The Process must be
// registered first: kitbashd reads the owner, the container name, the image
// and the fan out secret from that registration, and refuses an id it does not
// know.
//
// A daemon that is not there is not a Process that starts anyway. Version 1
// has no untraced path, so the caller reports this and starts nothing, see
// PLAN.md section 2.6.
func (c *Client) StartProcess(ctx context.Context, id string, opts StartOptions) (string, *problem.Problem) {
	body, prob := encode(id, opts)
	if prob != nil {
		return "", prob
	}
	answer, prob := c.action(ctx, id, startAction, body)
	if prob != nil {
		return "", prob
	}
	return answer.ContainerID, nil
}

// StopProcess asks kitbashd to stop the container of a Process as its owner.
// The registration is untouched: the caller unregisters, which is what revokes
// the token.
func (c *Client) StopProcess(ctx context.Context, id string) *problem.Problem {
	_, prob := c.action(ctx, id, stopAction, nil)
	return prob
}

// RemoveProcess asks kitbashd to remove the container of a Process as its
// owner, which is what a run that replaces a Process does to the one it takes
// the place of.
func (c *Client) RemoveProcess(ctx context.Context, id string) *problem.Problem {
	_, prob := c.action(ctx, id, removeAction, nil)
	return prob
}

// action performs one of the three, mapping kitbashd's answer onto the problem
// the agent reads. A daemon that cannot be reached is internal with the fix
// only an operator can act on.
func (c *Client) action(ctx context.Context, id, action string, body []byte) (startResponse, *problem.Problem) {
	if c == nil {
		return startResponse{}, problem.Internal(id, "kitbashd is not reachable from this session", NotRunningFix)
	}
	if id == "" {
		return startResponse{}, problem.Internal(id, "no Process id was given to "+action, "")
	}
	ctx, cancel := context.WithTimeout(ctx, budgetOf(action))
	defer cancel()
	status, payload, prob := c.sendWith(ctx, c.supervisor,
		http.MethodPost, ProcessesPath+"/"+id+"/"+action, body, id)
	if prob != nil {
		return startResponse{}, prob
	}
	if status >= 300 {
		return startResponse{}, c.failure(id, statusText(status), payload)
	}
	var answer startResponse
	if err := json.Unmarshal(payload, &answer); err != nil {
		return startResponse{}, problem.Internal(id,
			fmt.Sprintf("%s answered %s with a body that is not a Process: %v", ProcessesPath, action, err), "")
	}
	return answer, nil
}

// budgetOf is how long one action may take.
func budgetOf(action string) time.Duration {
	if action == startAction {
		return startTimeout
	}
	return actionTimeout
}

// encode renders one request body.
func encode(id string, opts StartOptions) ([]byte, *problem.Problem) {
	body, err := json.Marshal(opts)
	if err != nil {
		return nil, problem.Internal(id, err.Error(), "")
	}
	return body, nil
}
