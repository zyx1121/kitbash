package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/zyx1121/kitbash/internal/problem"
)

// The JSON API of spec/kitbashd-api.yaml. OTLP has no verb for a query or for
// a setting, so these two paths sit beside it on the same socket.
const (
	QueryPath     = "/kitbash/v1/query"
	RetentionPath = "/kitbash/v1/retention"
)

// NotRunningFix is what an agent is told when the socket is not there. It is
// the one failure in the tel family the caller cannot act on alone.
const NotRunningFix = "kitbashd is not running on this host; ask an administrator to start it."

// maxResponseBytes bounds what one JSON answer may be, matching the request
// limit kitbashd enforces.
const maxResponseBytes = 4 << 20

// Client calls the JSON API of kitbashd over its unix socket. Identity is the
// socket's peer credentials, so the client carries no token and no user name.
type Client struct {
	socket string
	http   *http.Client
}

// NewClient builds the client for one socket.
func NewClient(socket string) *Client {
	if socket == "" {
		socket = SocketPath()
	}
	return &Client{socket: socket, http: socketClient(socket)}
}

// Query forwards a tel_query input verbatim and returns kitbashd's body
// verbatim. Neither is reshaped here: the schemas on both sides are the same
// ones spec/mcp-surface.yaml publishes.
func (c *Client) Query(ctx context.Context, input json.RawMessage) (json.RawMessage, *problem.Problem) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	return c.do(ctx, http.MethodPost, QueryPath, input, "tel_query")
}

// Retention reads the window per signal, or sets one or more of them when set
// is not empty. Reading is a GET and setting is a PUT, so a caller that sends
// no set never needs the permission a change needs.
func (c *Client) Retention(ctx context.Context, set json.RawMessage) (json.RawMessage, *problem.Problem) {
	if len(set) == 0 || string(set) == "null" {
		return c.do(ctx, http.MethodGet, RetentionPath, nil, "tel_retention")
	}
	return c.do(ctx, http.MethodPut, RetentionPath, set, "tel_retention")
}

func (c *Client) do(ctx context.Context, method, path string, body json.RawMessage, instance string) (json.RawMessage, *problem.Problem) {
	if c == nil {
		return nil, problem.Internal(instance, "telemetry is not configured for this session", NotRunningFix)
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://kitbashd"+path, reader)
	if err != nil {
		return nil, problem.Internal(instance, err.Error(), "")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		// A socket that is absent or refuses is the same story to the agent:
		// this host has no Telemetry right now and only an operator can
		// change that.
		return nil, problem.Internal(instance,
			fmt.Sprintf("%s %s over %s: %v", method, path, c.socket, err), NotRunningFix)
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return nil, problem.Internal(instance, err.Error(), NotRunningFix)
	}
	if res.StatusCode >= 300 {
		return nil, c.failure(instance, res, payload)
	}
	if !json.Valid(payload) {
		return nil, problem.Internal(instance,
			fmt.Sprintf("%s answered %s with a body that is not JSON", path, res.Status), "")
	}
	return json.RawMessage(payload), nil
}

// failure turns a refusal into the problem the agent sees. kitbashd answers in
// the same problem details the surface speaks, so a 403 from the daemon
// reaches the agent unchanged, slug and fix and all.
func (c *Client) failure(instance string, res *http.Response, payload []byte) *problem.Problem {
	if p, err := decodeProblem(payload); err == nil {
		return p
	}
	detail := strings.TrimSpace(string(payload))
	if detail == "" {
		detail = res.Status
	}
	return problem.Internal(instance,
		fmt.Sprintf("kitbashd answered %s: %s", res.Status, detail), "")
}

// decodeProblem reads an RFC 9457 body kitbashd sent, refusing anything that
// is not one of the surface's own error classes.
func decodeProblem(payload []byte) (*problem.Problem, error) {
	var p problem.Problem
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(p.Type, problem.Base) || p.Title == "" || p.Status == 0 {
		return nil, errors.New("not problem details")
	}
	return &p, nil
}
