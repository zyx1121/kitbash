package teltest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// PendingLimit is the cap of spec/kitbashd-api.yaml: a member holds at most 64
// pending approvals and the 65th is a conflict.
const PendingLimit = 64

// Approval is one queued call the fake holds, as approvals_list returns it.
type Approval struct {
	ID          string          `json:"id"`
	Requester   string          `json:"requester"`
	Tool        string          `json:"tool"`
	Input       json.RawMessage `json:"input"`
	State       string          `json:"state"`
	RequestedAt string          `json:"requestedAt"`
	DecidedAt   string          `json:"decidedAt,omitempty"`
	DecidedBy   string          `json:"decidedBy,omitempty"`
	Note        string          `json:"note,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

// AnswerApprovals replaces what the approval queue returns. The zero Response
// restores the fake's own behaviour.
func (d *Daemon) AnswerApprovals(r Response) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.approvalsAnswer = r
}

// AnswerResult replaces what storing a result returns, and only that, so a
// test can let an approval be claimed and executed and then have the daemon
// refuse the outcome. The zero Response restores the fake's own behaviour.
func (d *Daemon) AnswerResult(r Response) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.resultAnswer = r
}

// AddApproval seeds the queue, standing in for a call queued in another
// session. An approval without an id or a state gets both.
func (d *Daemon) AddApproval(approval Approval) Approval {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.queue(approval)
}

// Approvals are the approvals the fake holds, in the order they were queued.
func (d *Daemon) Approvals() []Approval {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Approval, 0, len(d.approvalOrder))
	for _, id := range d.approvalOrder {
		out = append(out, d.approvals[id])
	}
	return out
}

// Approval is one approval by id.
func (d *Daemon) Approval(id string) (Approval, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	approval, ok := d.approvals[id]
	return approval, ok
}

// queue records one approval. The caller holds the lock.
func (d *Daemon) queue(approval Approval) Approval {
	if approval.ID == "" {
		d.approvalIDs++
		approval.ID = fmt.Sprintf("0199a000-0000-7000-8000-%012d", d.approvalIDs)
	}
	if approval.State == "" {
		approval.State = "pending"
	}
	if approval.RequestedAt == "" {
		approval.RequestedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if approval.Requester == "" {
		approval.Requester = d.identity.User
	}
	if _, held := d.approvals[approval.ID]; !held {
		d.approvalOrder = append(d.approvalOrder, approval.ID)
	}
	d.approvals[approval.ID] = approval
	return approval
}

// createApproval answers approvals_create: any member may queue a call, and a
// member holding PendingLimit pending approvals may not queue another.
func (d *Daemon) createApproval(w http.ResponseWriter, r *http.Request) {
	d.wait()
	body, err := readAll(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	if override, ok := d.approvalsOverride(); ok {
		d.mu.Unlock()
		write(w, override)
		return
	}
	var in struct {
		Tool  string          `json:"tool"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.Tool == "" {
		d.mu.Unlock()
		write(w, Problem(http.StatusBadRequest, "bad-request", "Bad request",
			"the body is not a queued call", "Send the tool and its input."))
		return
	}
	if d.pending() >= PendingLimit {
		d.mu.Unlock()
		write(w, Problem(http.StatusConflict, "conflict", "Conflict",
			fmt.Sprintf("you hold %d pending approvals", PendingLimit),
			"Wait for an admin to decide on the ones you have."))
		return
	}
	approval := d.queue(Approval{Tool: in.Tool, Input: in.Input})
	d.mu.Unlock()
	answer, _ := json.Marshal(map[string]any{
		"id": approval.ID, "state": approval.State, "requestedAt": approval.RequestedAt,
	})
	write(w, Response{Status: http.StatusOK, ContentType: "application/json", Body: string(answer)})
}

// listApprovals answers approvals_list, filtered by state.
func (d *Daemon) listApprovals(w http.ResponseWriter, r *http.Request) {
	d.wait()
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "pending"
	}
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.RequestURI()})
	if override, ok := d.approvalsOverride(); ok {
		d.mu.Unlock()
		write(w, override)
		return
	}
	list := make([]Approval, 0, len(d.approvalOrder))
	for _, id := range d.approvalOrder {
		if d.approvals[id].State == state {
			list = append(list, d.approvals[id])
		}
	}
	d.mu.Unlock()
	answer, _ := json.Marshal(map[string]any{"approvals": list})
	write(w, Response{Status: http.StatusOK, ContentType: "application/json", Body: string(answer)})
}

// claimApproval answers approvals_claim: admin only, pending to approved, and
// a conflict for anything else.
func (d *Daemon) claimApproval(w http.ResponseWriter, r *http.Request) {
	d.wait()
	id := r.PathValue("id")
	body, err := readAll(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var in struct {
		Note string `json:"note"`
	}
	json.Unmarshal(body, &in)
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	approval, answer, ok := d.decide(id, "approved")
	if !ok {
		d.mu.Unlock()
		write(w, answer)
		return
	}
	approval.Note = in.Note
	d.approvals[id] = approval
	d.mu.Unlock()
	claimed, _ := json.Marshal(map[string]any{
		"id": approval.ID, "requester": approval.Requester, "tool": approval.Tool,
		"input": approval.Input, "state": approval.State,
	})
	write(w, Response{Status: http.StatusOK, ContentType: "application/json", Body: string(claimed)})
}

// storeResult answers approvals_result: the admin who claimed it, once.
func (d *Daemon) storeResult(w http.ResponseWriter, r *http.Request) {
	d.wait()
	id := r.PathValue("id")
	body, err := readAll(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	if d.resultAnswer.Status != 0 {
		answer := d.resultAnswer
		d.mu.Unlock()
		write(w, answer)
		return
	}
	if override, ok := d.approvalsOverride(); ok {
		d.mu.Unlock()
		write(w, override)
		return
	}
	approval, held := d.approvals[id]
	if !held {
		d.mu.Unlock()
		write(w, Problem(http.StatusNotFound, "not-found", "Not found",
			"no approval has this id", ""))
		return
	}
	if approval.State != "approved" || approval.Result != nil {
		d.mu.Unlock()
		write(w, Problem(http.StatusConflict, "conflict", "Conflict",
			fmt.Sprintf("this approval is %s and its result is written once", approval.State), ""))
		return
	}
	var in struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.Result == nil {
		d.mu.Unlock()
		write(w, Problem(http.StatusBadRequest, "bad-request", "Bad request",
			"the body carries no result", "Send the tool's output or the problem it failed with."))
		return
	}
	approval.Result = in.Result
	d.approvals[id] = approval
	d.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// rejectApproval answers approvals_reject: admin only, pending to rejected.
func (d *Daemon) rejectApproval(w http.ResponseWriter, r *http.Request) {
	d.wait()
	id := r.PathValue("id")
	body, err := readAll(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	json.Unmarshal(body, &in)
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	approval, answer, ok := d.decide(id, "rejected")
	if !ok {
		d.mu.Unlock()
		write(w, answer)
		return
	}
	approval.Reason = in.Reason
	d.approvals[id] = approval
	d.mu.Unlock()
	decided, _ := json.Marshal(map[string]any{"id": approval.ID, "state": approval.State})
	write(w, Response{Status: http.StatusOK, ContentType: "application/json", Body: string(decided)})
}

// decide moves one pending approval to a decided state. It answers the
// refusals both decisions share: an override, a caller who is not an admin, an
// id nobody queued, and an approval that was decided already. The caller holds
// the lock.
func (d *Daemon) decide(id, state string) (Approval, Response, bool) {
	if override, ok := d.approvalsOverride(); ok {
		return Approval{}, override, false
	}
	if !d.identity.Admin {
		return Approval{}, Problem(http.StatusForbidden, "not-permitted", "Not permitted",
			"deciding an approval is for admins", "Ask an admin to decide it."), false
	}
	approval, held := d.approvals[id]
	if !held {
		return Approval{}, Problem(http.StatusNotFound, "not-found", "Not found",
			"no approval has this id", ""), false
	}
	if approval.State != "pending" {
		return Approval{}, Problem(http.StatusConflict, "conflict", "Conflict",
			fmt.Sprintf("this approval is already %s", approval.State), ""), false
	}
	approval.State = state
	approval.DecidedAt = time.Now().UTC().Format(time.RFC3339)
	approval.DecidedBy = d.identity.User
	d.approvals[id] = approval
	return approval, Response{}, true
}

// pending counts the approvals waiting for a decision. The caller holds the
// lock.
func (d *Daemon) pending() int {
	count := 0
	for _, approval := range d.approvals {
		if approval.State == "pending" {
			count++
		}
	}
	return count
}

// approvalsOverride is the answer AnswerApprovals installed, if any. The
// caller holds the lock.
func (d *Daemon) approvalsOverride() (Response, bool) {
	if d.approvalsAnswer.Status == 0 {
		return Response{}, false
	}
	return d.approvalsAnswer, true
}
