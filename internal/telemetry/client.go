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
	"sync"
	"unicode/utf8"

	"github.com/zyx1121/kitbash/internal/problem"
)

// The JSON API of spec/kitbashd-api.yaml. OTLP has no verb for a query or for
// a setting, so these two paths sit beside it on the same socket.
const (
	QueryPath     = "/kitbash/v1/query"
	RetentionPath = "/kitbash/v1/retention"
	InternalPath  = "/kitbash/v1/internal"
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
	// supervisor is the same socket with a longer patience, for the three
	// calls that make kitbashd run the container runtime, see run.go.
	supervisor *http.Client

	// The one identity question the session asks, users_me, and whether it
	// has been answered. Admin holds them, see users.go.
	mu       sync.Mutex
	asked    bool
	identity Identity
}

// NewClient builds the client for one socket.
func NewClient(socket string) *Client {
	if socket == "" {
		socket = SocketPath()
	}
	return &Client{
		socket:     socket,
		http:       socketClient(socket),
		supervisor: socketClientWith(socket, supervisorTimeout),
	}
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

// Internal reports the cause of one internal problem to kitbashd, which
// stores it as a record only an admin reads, see PLAN.md section 2.4. No
// producer may assert kitbash.internal on an export, so this path is the only
// way a cause becomes Telemetry.
//
// It answers with a plain error and never with a problem: this is what
// problem.Internal calls, and a problem here would call it again.
func (c *Client) Internal(ctx context.Context, instance, cause, tool string) error {
	if c == nil {
		return errors.New("telemetry: no client for this session")
	}
	body, err := json.Marshal(struct {
		Instance string `json:"instance,omitempty"`
		Cause    string `json:"cause"`
		Tool     string `json:"tool,omitempty"`
	}{Instance: instance, Cause: cause, Tool: tool})
	if err != nil {
		return fmt.Errorf("telemetry: encode the internal cause: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://kitbashd"+InternalPath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telemetry: record the internal cause: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("telemetry: record the internal cause over %s: %w", c.socket, err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, maxDetailBytes))
	if res.StatusCode >= 300 {
		return fmt.Errorf("telemetry: kitbashd answered %s to the internal cause", statusText(res.StatusCode))
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body json.RawMessage, instance string) (json.RawMessage, *problem.Problem) {
	if c == nil {
		return nil, problem.Internal(instance, "telemetry is not configured for this session", NotRunningFix)
	}
	status, payload, prob := c.send(ctx, method, path, body, instance)
	if prob != nil {
		return nil, prob
	}
	if status >= 300 {
		return nil, c.failure(instance, statusText(status), payload)
	}
	if !json.Valid(payload) {
		return nil, problem.Internal(instance,
			fmt.Sprintf("%s answered %s with a body that is not JSON", path, statusText(status)), "")
	}
	return json.RawMessage(payload), nil
}

// send performs one request and returns the status and the body. Only a
// transport failure is a problem here: a status is an answer, and what it
// means is the caller's to decide, because a 404 refuses a query and closes a
// Process unregistration.
func (c *Client) send(ctx context.Context, method, path string, body []byte, instance string) (int, []byte, *problem.Problem) {
	return c.sendWith(ctx, c.http, method, path, body, instance)
}

// sendWith is send over one of this client's two HTTP clients, which differ
// only in how long they wait.
func (c *Client) sendWith(ctx context.Context, client *http.Client, method, path string, body []byte, instance string) (int, []byte, *problem.Problem) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://kitbashd"+path, reader)
	if err != nil {
		return 0, nil, problem.Internal(instance, err.Error(), "")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	res, err := client.Do(req)
	if err != nil {
		// A socket that is absent or refuses is the same story to the agent:
		// this host has no Telemetry right now and only an operator can
		// change that.
		return 0, nil, problem.Internal(instance,
			fmt.Sprintf("%s %s over %s: %v", method, path, c.socket, err), NotRunningFix)
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return res.StatusCode, nil, problem.Internal(instance, err.Error(), NotRunningFix)
	}
	return res.StatusCode, payload, nil
}

// failure turns a refusal into the problem the agent sees. kitbashd answers in
// the same problem details the surface speaks, so a 403 from the daemon
// reaches the agent unchanged, slug and fix and all.
func (c *Client) failure(instance, status string, payload []byte) *problem.Problem {
	if p, err := decodeProblem(payload); err == nil {
		return p
	}
	detail := strings.TrimSpace(string(payload))
	if detail == "" {
		detail = status
	}
	return problem.Internal(instance,
		fmt.Sprintf("kitbashd answered %s: %s", status, clip(detail, maxDetailBytes)), "")
}

// maxDetailBytes is how much of a body that is not problem details is worth
// repeating. The answer is a refusal nobody structured, and the whole of it
// could be a megabyte of HTML on its way into the server log.
const maxDetailBytes = 1 << 10

// clip shortens a string to a byte budget, saying that it did.
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	// The budget is bytes, so the cut is moved back off a partial rune.
	cut := limit
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("... (%d bytes truncated)", len(s)-cut)
}

// statusText spells a status the way an HTTP response line does, which is what
// the detail of an unreadable refusal carries.
func statusText(status int) string {
	return fmt.Sprintf("%d %s", status, http.StatusText(status))
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
