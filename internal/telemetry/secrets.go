package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/zyx1121/kitbash/internal/problem"
)

// The secrets family of spec/kitbashd-api.yaml. The values are root owned
// files and a session runs as the member, so every one of these is forwarded
// over the socket and kitbashd answers about the peer: there is no member name
// in any of them, which is what makes an admin's set their own and nobody
// else's, see PLAN.md section 2.3.
const SecretsPath = "/kitbash/v1/secrets"

// SetSecret answers secrets_set. The name is in the path and the value in the
// body, the one place a value crosses this socket; nothing here logs it, and
// the span the call records carries the tool name and no argument.
func (c *Client) SetSecret(ctx context.Context, name, value string) (json.RawMessage, *problem.Problem) {
	body, err := json.Marshal(map[string]string{"value": value})
	if err != nil {
		return nil, problem.Internal("secrets_set", err.Error(), "")
	}
	return c.do(ctx, http.MethodPut, SecretsPath+"/"+url.PathEscape(name), body, "secrets_set")
}

// ListSecrets answers secrets_list: the caller's own names and when each was
// written, never a value.
func (c *Client) ListSecrets(ctx context.Context) (json.RawMessage, *problem.Problem) {
	return c.do(ctx, http.MethodGet, SecretsPath, nil, "secrets_list")
}

// RemoveSecret answers secrets_remove, which is idempotent: a name the caller
// does not hold answers removed false rather than failing.
func (c *Client) RemoveSecret(ctx context.Context, name string) (json.RawMessage, *problem.Problem) {
	return c.do(ctx, http.MethodDelete, SecretsPath+"/"+url.PathEscape(name), nil, "secrets_remove")
}
