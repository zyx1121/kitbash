package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/zyx1121/kitbash/internal/problem"
)

// ApprovalsPath is the approval queue of spec/kitbashd-api.yaml. An operation
// outside the caller's own space, which in version 1 is a member's fs_write or
// pkg_import under /org, is queued here instead of failing, see PLAN.md
// section 2.1.
const ApprovalsPath = "/kitbash/v1/approvals"

// The two tools that queue. Everything else outside the caller's space is
// simply not permitted.
const (
	ToolFSWrite   = "fs_write"
	ToolPkgImport = "pkg_import"
)

// The states an approval moves through: pending to approved and a result once,
// or pending to rejected.
const (
	StatePending  = "pending"
	StateApproved = "approved"
	StateRejected = "rejected"
)

// Approval is one queued call as kitbashd records it. The input is the tool's
// own input, verbatim, so the session that executes it validates it again the
// way the session that queued it would have.
type Approval struct {
	ID          string          `json:"id"`
	Requester   string          `json:"requester"`
	Tool        string          `json:"tool"`
	Input       json.RawMessage `json:"input,omitempty"`
	State       string          `json:"state"`
	RequestedAt string          `json:"requestedAt,omitempty"`
	DecidedAt   string          `json:"decidedAt,omitempty"`
	DecidedBy   string          `json:"decidedBy,omitempty"`
	Note        string          `json:"note,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

// CreateApproval queues one call. The requester is the socket's peer, so the
// body carries only what is being asked for.
func (c *Client) CreateApproval(ctx context.Context, tool string, input json.RawMessage) (*Approval, *problem.Problem) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	body, err := json.Marshal(struct {
		Tool  string          `json:"tool"`
		Input json.RawMessage `json:"input"`
	}{Tool: tool, Input: input})
	if err != nil {
		return nil, problem.Internal(tool, err.Error(), "")
	}
	payload, prob := c.do(ctx, http.MethodPost, ApprovalsPath, body, tool)
	if prob != nil {
		return nil, prob
	}
	return decodeApproval(tool, payload)
}

// ListApprovals answers approvals_list. The state is the query parameter of
// spec/kitbashd-api.yaml; an empty one leaves the daemon's default, pending.
func (c *Client) ListApprovals(ctx context.Context, state string) (json.RawMessage, *problem.Problem) {
	path := ApprovalsPath
	if state != "" {
		path += "?state=" + url.QueryEscape(state)
	}
	return c.do(ctx, http.MethodGet, path, nil, "approvals_list")
}

// ClaimApproval moves one approval from pending to approved and returns what
// the session needs to execute it: the requester, the tool and its input.
// kitbashd enforces both the admin and the state, so a second claim of the
// same approval is a conflict this client passes through unchanged.
func (c *Client) ClaimApproval(ctx context.Context, id, note string) (*Approval, *problem.Problem) {
	body, err := json.Marshal(map[string]string{"note": note})
	if err != nil {
		return nil, problem.Internal(id, err.Error(), "")
	}
	payload, prob := c.do(ctx, http.MethodPost, approvalPath(id)+"/claim", body, id)
	if prob != nil {
		return nil, prob
	}
	return decodeApproval(id, payload)
}

// StoreApprovalResult records the outcome of an executed approval: the tool's
// output, or the problem the execution returned. The requester reads it back
// with approvals_list, which is the only way they learn what happened.
func (c *Client) StoreApprovalResult(ctx context.Context, id string, result json.RawMessage) *problem.Problem {
	if len(result) == 0 {
		result = json.RawMessage(`{}`)
	}
	body, err := json.Marshal(struct {
		Result json.RawMessage `json:"result"`
	}{Result: result})
	if err != nil {
		return problem.Internal(id, err.Error(), "")
	}
	if c == nil {
		return problem.Internal(id, "telemetry is not configured for this session", NotRunningFix)
	}
	status, payload, prob := c.send(ctx, http.MethodPut, approvalPath(id)+"/result", body, id)
	if prob != nil {
		return prob
	}
	if status >= 300 {
		return c.failure(id, statusText(status), payload)
	}
	return nil
}

// RejectApproval answers approvals_reject.
func (c *Client) RejectApproval(ctx context.Context, id, reason string) (json.RawMessage, *problem.Problem) {
	body, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return nil, problem.Internal(id, err.Error(), "")
	}
	return c.do(ctx, http.MethodPost, approvalPath(id)+"/reject", body, id)
}

// approvalPath is the path of one approval.
func approvalPath(id string) string {
	return ApprovalsPath + "/" + url.PathEscape(id)
}

// decodeApproval reads an approval the daemon answered with. An answer without
// an id is unusable: the id is what the queued problem carries back to the
// requester and what the admin approves.
func decodeApproval(instance string, payload json.RawMessage) (*Approval, *problem.Problem) {
	var approval Approval
	if err := json.Unmarshal(payload, &approval); err != nil {
		return nil, problem.Internal(instance,
			fmt.Sprintf("%s answered with a body that is not an approval: %v", ApprovalsPath, err), "")
	}
	if approval.ID == "" {
		return nil, problem.Internal(instance,
			fmt.Sprintf("%s answered without an approval id", ApprovalsPath), "")
	}
	return &approval, nil
}
