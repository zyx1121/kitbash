package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// queue puts one operation in the queue and returns its id.
func (h *harness) queue(input string) string {
	h.t.Helper()
	res, body := h.postJSON(http.MethodPost, approvalsPath, approvalRequest{
		Tool:  store.ToolFSWrite,
		Input: json.RawMessage(input),
	})
	if res.StatusCode != http.StatusOK {
		h.t.Fatalf("queue status = %d, body %s", res.StatusCode, body)
	}
	var got approvalResponse
	if err := json.Unmarshal(body, &got); err != nil {
		h.t.Fatalf("body %q: %v", body, err)
	}
	if got.State != store.StatePending || got.RequestedAt == "" || !uuidV7.MatchString(got.ID) {
		h.t.Fatalf("queued %+v, want a UUIDv7 pending with a time", got)
	}
	return got.ID
}

// listApprovalsOf reads the queue in one state.
func (h *harness) listApprovalsOf(state string) []store.Approval {
	h.t.Helper()
	path := approvalsPath
	if state != "" {
		path += "?state=" + state
	}
	res, body := h.do(http.MethodGet, path, "", nil)
	if res.StatusCode != http.StatusOK {
		h.t.Fatalf("list status = %d, body %s", res.StatusCode, body)
	}
	var got approvalList
	if err := json.Unmarshal(body, &got); err != nil {
		h.t.Fatalf("body %q: %v", body, err)
	}
	return got.Approvals
}

// TestApprovalsQueueAndApprove walks the state machine PLAN.md section 2.1
// describes: a member queues, an admin claims, the admin stores the result,
// and the requester reads it back.
func TestApprovalsQueueAndApprove(t *testing.T) {
	h, _ := serveUsers(t, true)
	input := `{"path":"/org/handbook/README.md","message":"Add a line"}`
	id := h.queue(input)

	pending := h.listApprovalsOf("")
	if len(pending) != 1 || pending[0].ID != id || pending[0].Requester != h.user {
		t.Fatalf("pending = %+v, want the one just queued", pending)
	}
	if string(pending[0].Input) != input {
		t.Errorf("input = %s, want it stored verbatim", pending[0].Input)
	}
	if pending[0].DecidedAt != nil || pending[0].DecidedBy != "" {
		t.Errorf("a pending approval carries a decision: %+v", pending[0])
	}

	res, body := h.postJSON(http.MethodPost, approvalsPath+"/"+id+"/claim", claimRequest{Note: "looks right"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("claim status = %d, body %s", res.StatusCode, body)
	}
	var claimed claimResponse
	if err := json.Unmarshal(body, &claimed); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if claimed.State != store.StateApproved || claimed.Tool != store.ToolFSWrite || string(claimed.Input) != input {
		t.Errorf("claim = %+v, want the approved call with its input", claimed)
	}
	if claimed.Requester != h.user {
		t.Errorf("requester = %q, want %q", claimed.Requester, h.user)
	}

	// Claiming the same approval twice is a conflict: it is decided once.
	res, body = h.postJSON(http.MethodPost, approvalsPath+"/"+id+"/claim", claimRequest{})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("second claim status = %d, body %s", res.StatusCode, body)
	}
	if slug := h.problemOf(res, body).Slug(); slug != problem.SlugConflict {
		t.Errorf("slug = %q, want conflict", slug)
	}

	result := `{"commit":"abc1234","path":"/org/handbook/README.md"}`
	res, body = h.postJSON(http.MethodPut, approvalsPath+"/"+id+"/result",
		resultRequest{Result: json.RawMessage(result)})
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("result status = %d, body %s", res.StatusCode, body)
	}

	// A result is stored once.
	res, body = h.postJSON(http.MethodPut, approvalsPath+"/"+id+"/result",
		resultRequest{Result: json.RawMessage(`{"commit":"other"}`)})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("second result status = %d, body %s", res.StatusCode, body)
	}

	approved := h.listApprovalsOf(store.StateApproved)
	if len(approved) != 1 || approved[0].State != store.StateApproved {
		t.Fatalf("approved = %+v, want the one that was claimed", approved)
	}
	if string(approved[0].Result) != result {
		t.Errorf("result = %s, want it stored verbatim", approved[0].Result)
	}
	if approved[0].DecidedBy != h.user || approved[0].Note != "looks right" || approved[0].DecidedAt == nil {
		t.Errorf("decision = %+v, want the admin, the note and the time", approved[0])
	}
	if len(h.listApprovalsOf(store.StatePending)) != 0 {
		t.Errorf("the approval is still pending after being approved")
	}
}

func TestApprovalsReject(t *testing.T) {
	h, _ := serveUsers(t, true)
	id := h.queue(`{"path":"/org/handbook/README.md"}`)

	res, body := h.postJSON(http.MethodPost, approvalsPath+"/"+id+"/reject",
		rejectRequest{Reason: "write it under your own home instead"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("reject status = %d, body %s", res.StatusCode, body)
	}
	var got rejectResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if got.ID != id || got.State != store.StateRejected {
		t.Errorf("reject = %+v, want the id rejected", got)
	}

	rejected := h.listApprovalsOf(store.StateRejected)
	if len(rejected) != 1 || rejected[0].Reason == "" || rejected[0].DecidedBy != h.user {
		t.Errorf("rejected = %+v, want the reason and the admin", rejected)
	}

	// A decided approval cannot be claimed after the fact.
	res, body = h.postJSON(http.MethodPost, approvalsPath+"/"+id+"/claim", claimRequest{})
	if res.StatusCode != http.StatusConflict {
		t.Errorf("claim after reject status = %d, body %s", res.StatusCode, body)
	}

	// A reason too short to act on is refused.
	other := h.queue(`{"path":"/org/handbook/other.md"}`)
	res, body = h.postJSON(http.MethodPost, approvalsPath+"/"+other+"/reject", rejectRequest{Reason: "no"})
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("short reason status = %d, body %s", res.StatusCode, body)
	}
}

// TestApprovalsResultIsTheClaimants is the rule the result rests on: the
// answer a requester reads comes from the session that ran the call.
func TestApprovalsResultIsTheClaimants(t *testing.T) {
	h, _ := serveUsers(t, true)
	id := h.queue(`{"path":"/org/handbook/README.md"}`)

	// Another admin claims it, which the socket cannot do in a test because
	// the peer is always this process, so the store is asked directly.
	if _, found, err := h.store.ClaimApproval(context.Background(), id, "other-admin", "mine", h.server.now()); err != nil || !found {
		t.Fatalf("ClaimApproval: %t, %v", found, err)
	}

	res, body := h.postJSON(http.MethodPut, approvalsPath+"/"+id+"/result",
		resultRequest{Result: json.RawMessage(`{"commit":"abc"}`)})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body %s", res.StatusCode, body)
	}
	if slug := h.problemOf(res, body).Slug(); slug != problem.SlugNotPermitted {
		t.Errorf("slug = %q, want not-permitted", slug)
	}

	stored, found, err := h.store.Approval(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("Approval: %t, %v", found, err)
	}
	if len(stored.Result) != 0 {
		t.Errorf("result = %s, want nothing written by another admin", stored.Result)
	}
}

// TestApprovalsAreVisibleToTheirRequester is the reading rule: a member sees
// their own queue, an admin sees the machine.
func TestApprovalsAreVisibleToTheirRequester(t *testing.T) {
	h, _ := serveUsers(t, false)
	mine := h.queue(`{"path":"/org/handbook/README.md"}`)

	// One queued by somebody else, written straight to the store.
	theirs := uuid.V7()
	if err := h.store.CreateApproval(context.Background(), store.Approval{
		ID: theirs, Requester: "alice", Tool: store.ToolPkgImport,
		Input: json.RawMessage(`{"source":"docker.io/library/alpine"}`), RequestedAt: h.server.now(),
	}, 0); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}

	list := h.listApprovalsOf(store.StatePending)
	if len(list) != 1 || list[0].ID != mine {
		t.Fatalf("a member sees %+v, want their own approval alone", list)
	}

	// A member may not decide anything, their own included.
	for _, path := range []string{"/claim", "/reject"} {
		res, body := h.postJSON(http.MethodPost, approvalsPath+"/"+mine+path,
			rejectRequest{Reason: "changed my mind"})
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s status = %d, want 403, body %s", path, res.StatusCode, body)
		}
	}

	// The same daemon with an admin caller sees both.
	admin, _ := serveUsers(t, true)
	if err := admin.store.CreateApproval(context.Background(), store.Approval{
		ID: uuid.V7(), Requester: "alice", Tool: store.ToolFSWrite,
		Input: json.RawMessage(`{"path":"/org/handbook/a.md"}`), RequestedAt: admin.server.now(),
	}, 0); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	admin.queue(`{"path":"/org/handbook/b.md"}`)
	if got := admin.listApprovalsOf(store.StatePending); len(got) != 2 {
		t.Errorf("an admin sees %d approvals, want 2", len(got))
	}
}

// TestApprovalsPendingCap keeps one member from filling the queue an admin
// reads.
func TestApprovalsPendingCap(t *testing.T) {
	h, _ := serveUsers(t, false)
	for i := range MaxPendingApprovals {
		h.queue(fmt.Sprintf(`{"path":"/org/handbook/%d.md"}`, i))
	}
	res, body := h.postJSON(http.MethodPost, approvalsPath, approvalRequest{
		Tool:  store.ToolFSWrite,
		Input: json.RawMessage(`{"path":"/org/handbook/last.md"}`),
	})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body %s", res.StatusCode, body)
	}
	if slug := h.problemOf(res, body).Slug(); slug != problem.SlugConflict {
		t.Errorf("slug = %q, want conflict", slug)
	}
}

func TestApprovalsRefusals(t *testing.T) {
	h, _ := serveUsers(t, true)
	big := `{"content":"` + strings.Repeat("a", MaxApprovalBytes) + `"}`

	cases := map[string]struct {
		body   any
		status int
		slug   string
	}{
		"a tool that does not queue": {
			body:   approvalRequest{Tool: "proc_run", Input: json.RawMessage(`{}`)},
			status: http.StatusBadRequest,
			slug:   problem.SlugBadRequest,
		},
		"an input that is not an object": {
			body:   approvalRequest{Tool: store.ToolFSWrite, Input: json.RawMessage(`"a string"`)},
			status: http.StatusBadRequest,
			slug:   problem.SlugBadRequest,
		},
		"no input at all": {
			body:   approvalRequest{Tool: store.ToolFSWrite},
			status: http.StatusBadRequest,
			slug:   problem.SlugBadRequest,
		},
		"an input over the size an approval carries": {
			body:   approvalRequest{Tool: store.ToolFSWrite, Input: json.RawMessage(big)},
			status: http.StatusRequestEntityTooLarge,
			slug:   problem.SlugTooLarge,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			res, body := h.postJSON(http.MethodPost, approvalsPath, c.body)
			if res.StatusCode != c.status {
				t.Fatalf("status = %d, want %d, body %s", res.StatusCode, c.status, body)
			}
			if slug := h.problemOf(res, body).Slug(); slug != c.slug {
				t.Errorf("slug = %q, want %q", slug, c.slug)
			}
		})
	}

	// An id that is not an id, and a state that is not a state.
	res, body := h.postJSON(http.MethodPost, approvalsPath+"/not-an-id/claim", claimRequest{})
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("claim of a bad id status = %d, body %s", res.StatusCode, body)
	}
	res, body = h.postJSON(http.MethodPost, approvalsPath+"/"+uuid.V7()+"/claim", claimRequest{})
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("claim of an unknown id status = %d, body %s", res.StatusCode, body)
	}
	res, body = h.do(http.MethodGet, approvalsPath+"?state=maybe", "", nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("list of a bad state status = %d, body %s", res.StatusCode, body)
	}
}
