package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/zyx1121/kitbash/internal/problem"
)

// The users family of spec/kitbashd-api.yaml. Creating a member is root's
// work, so kitbash-mcp forwards every one of these over the socket and
// kitbashd checks the peer is an admin, see PLAN.md section 4.5.
const (
	UsersPath   = "/kitbash/v1/users"
	UsersMePath = "/kitbash/v1/users/me"
)

// Identity is the users_me answer: who the socket's peer credentials say the
// caller is, and whether they are in kitbash-admin.
type Identity struct {
	User   string   `json:"user"`
	UID    int      `json:"uid"`
	Admin  bool     `json:"admin"`
	Groups []string `json:"groups,omitempty"`
}

// Me answers users_me. The body is kitbashd's, passed through as it arrived:
// the daemon and the surface publish the same schema for it.
func (c *Client) Me(ctx context.Context) (json.RawMessage, *problem.Problem) {
	return c.do(ctx, http.MethodGet, UsersMePath, nil, "users_me")
}

// CreateUser answers users_create with the tool's input verbatim.
func (c *Client) CreateUser(ctx context.Context, input json.RawMessage) (json.RawMessage, *problem.Problem) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	return c.do(ctx, http.MethodPost, UsersPath, input, "users_create")
}

// ListUsers answers users_list.
func (c *Client) ListUsers(ctx context.Context) (json.RawMessage, *problem.Problem) {
	return c.do(ctx, http.MethodGet, UsersPath, nil, "users_list")
}

// AddKey answers users_add_key. The member is in the path and the key in the
// body, which is the shape users_add_key has in spec/kitbashd-api.yaml.
func (c *Client) AddKey(ctx context.Context, name, sshKey string) (json.RawMessage, *problem.Problem) {
	body, err := json.Marshal(map[string]string{"sshKey": sshKey})
	if err != nil {
		return nil, problem.Internal("users_add_key", err.Error(), "")
	}
	return c.do(ctx, http.MethodPost, UsersPath+"/"+url.PathEscape(name)+"/keys", body, "users_add_key")
}

// RemoveUser answers users_remove.
func (c *Client) RemoveUser(ctx context.Context, name string) (json.RawMessage, *problem.Problem) {
	return c.do(ctx, http.MethodDelete, UsersPath+"/"+url.PathEscape(name), nil, "users_remove")
}

// Admin reports whether the caller is an admin, asking kitbashd once per
// session and keeping the answer. A session is one SSH connection, and a
// member made an admin in the middle of one reconnects, the same rule the
// surface applies to the tools a Process publishes.
//
// The second return says whether kitbashd answered at all. There is no safe
// guess for a daemon that is not there: calling the caller a member would
// queue a call nothing can be queued with, and calling them an admin would
// claim a permission this process cannot grant. The caller falls through to
// the kernel instead, which is the whole permission model in version 1.
//
// Only an answer is kept. A failure is not: the daemon may be starting, and
// one refused call must not decide the rest of the session.
func (c *Client) Admin(ctx context.Context) (admin, known bool) {
	if c == nil {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.asked {
		return c.identity.Admin, true
	}
	body, prob := c.Me(ctx)
	if prob != nil {
		return false, false
	}
	var identity Identity
	if err := json.Unmarshal(body, &identity); err != nil {
		return false, false
	}
	c.identity = identity
	c.asked = true
	return identity.Admin, true
}
