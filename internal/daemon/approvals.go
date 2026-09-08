package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// MaxApprovalBytes bounds the tool input one approval carries and the result
// stored on it. A write queued for an admin is a file the admin will commit,
// and a megabyte is far above what a manifest or a source file needs.
const MaxApprovalBytes = 1 << 20

// MaxPendingApprovals is how many approvals one member may have waiting. The
// queue is what an admin reads, so a member who cannot be refused could push
// every other member's request out of sight.
const MaxPendingApprovals = 64

// MaxNoteBytes and MaxReasonBytes are the two texts a decision carries, the
// same bounds spec/mcp-surface.yaml puts on them.
const (
	MaxNoteBytes   = 500
	MaxReasonBytes = 500
)

// The two sub paths under /kitbash/v1/approvals/{id}.
const (
	claimPath  = "claim"
	resultPath = "result"
	rejectPath = "reject"
)

// approvalRequest is the approvals_create input of spec/kitbashd-api.yaml.
type approvalRequest struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
}

// approvalResponse is what queueing one answers. The requester reads the
// outcome back with approvals_list.
type approvalResponse struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	RequestedAt string `json:"requestedAt"`
}

// approvalList is what approvals_list answers.
type approvalList struct {
	Approvals []store.Approval `json:"approvals"`
}

// claimRequest and rejectRequest are the two decisions an admin makes.
type claimRequest struct {
	Note string `json:"note,omitempty"`
}

type rejectRequest struct {
	Reason string `json:"reason"`
}

// resultRequest carries the outcome of an executed approval: the tool's normal
// output or the problem it failed with, whichever the admin's session got.
type resultRequest struct {
	Result json.RawMessage `json:"result"`
}

// claimResponse is what claiming answers: everything the admin's session needs
// to run the call the member asked for.
type claimResponse struct {
	ID        string          `json:"id"`
	Requester string          `json:"requester"`
	Tool      string          `json:"tool"`
	Input     json.RawMessage `json:"input"`
	State     string          `json:"state"`
}

// rejectResponse is what rejecting answers.
type rejectResponse struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// approvalsFamily answers POST and GET on /kitbash/v1/approvals.
func (s *Server) approvalsFamily(w http.ResponseWriter, r *http.Request) {
	caller, prob := s.caller(r)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.createApproval(w, r, caller)
	case http.MethodGet:
		s.listApprovals(w, r, caller)
	default:
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s %s is not part of the kitbashd API", r.Method, r.URL.Path),
			"Call POST to queue an operation or GET to read the queue."))
	}
}

// approval answers the three decision paths under an approval id.
func (s *Server) approval(w http.ResponseWriter, r *http.Request) {
	caller, prob := s.caller(r)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, approvalsPath+"/")
	id, action, _ := strings.Cut(rest, "/")
	if !uuidV7.MatchString(id) {
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%q is not an approval id", id),
			"Call the path with the id approvals_list answers with."))
		return
	}

	switch action {
	case claimPath:
		if r.Method != http.MethodPost {
			writeProblem(w, wrongMethod(r, "Call POST to claim an approval."))
			return
		}
		s.claimApproval(w, r, caller, id)
	case resultPath:
		if r.Method != http.MethodPut {
			writeProblem(w, wrongMethod(r, "Call PUT to store the result of an approval."))
			return
		}
		s.resultApproval(w, r, caller, id)
	case rejectPath:
		if r.Method != http.MethodPost {
			writeProblem(w, wrongMethod(r, "Call POST to reject an approval."))
			return
		}
		s.rejectApproval(w, r, caller, id)
	default:
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s is not part of the kitbashd API", r.URL.Path),
			"Call the approval id with /claim, /result or /reject."))
	}
}

// createApproval queues one operation. The requester is the peer: a member
// cannot queue a call in another member's name, because the commit an admin
// makes from it is authored by the requester, see PLAN.md section 2.1.
func (s *Server) createApproval(w http.ResponseWriter, r *http.Request, caller Caller) {
	var req approvalRequest
	if prob := decodeBody(w, r, &req); prob != nil {
		writeProblem(w, prob)
		return
	}
	switch req.Tool {
	case store.ToolFSWrite, store.ToolPkgImport:
	default:
		writeProblem(w, problem.BadRequest(r.URL.Path,
			fmt.Sprintf("%q is not a tool that queues for approval", req.Tool),
			"Queue fs_write or pkg_import; every other call outside your own space is refused rather than queued."))
		return
	}
	if prob := approvalObject(r.URL.Path, "input", req.Input); prob != nil {
		writeProblem(w, prob)
		return
	}

	a := store.Approval{
		ID:          uuid.V7(),
		Requester:   caller.User,
		Tool:        req.Tool,
		Input:       req.Input,
		State:       store.StatePending,
		RequestedAt: s.now().UTC(),
	}
	if err := s.store.CreateApproval(r.Context(), a, MaxPendingApprovals); err != nil {
		if errors.Is(err, store.ErrTooManyApprovals) {
			writeProblem(w, problem.ConflictFix(r.URL.Path,
				fmt.Sprintf("%s already has %d approvals pending", caller.User, MaxPendingApprovals),
				"Ask an administrator to work through the queue before adding to it."))
			return
		}
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	writeJSON(w, r.URL.Path, approvalResponse{
		ID:          a.ID,
		State:       a.State,
		RequestedAt: a.RequestedAt.Format(timeLayout),
	})
}

// listApprovals answers the queue: a member's own, or every member's for an
// admin, the same rule tel_query and processes_list follow.
func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request, caller Caller) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = store.StatePending
	}
	switch state {
	case store.StatePending, store.StateApproved, store.StateRejected:
	default:
		writeProblem(w, problem.BadRequest(r.URL.Path,
			fmt.Sprintf("%q is not an approval state", state),
			"Ask for pending, approved or rejected."))
		return
	}
	requester := caller.User
	if caller.Admin {
		requester = ""
	}
	list, err := s.store.Approvals(r.Context(), requester, state)
	if err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	writeJSON(w, r.URL.Path, approvalList{Approvals: list})
}

// claimApproval moves one approval from pending to approved and hands the
// admin's session the call to run. The result comes back to resultApproval.
func (s *Server) claimApproval(w http.ResponseWriter, r *http.Request, caller Caller, id string) {
	if prob := s.requireAdmin(r, caller, "approve a queued operation"); prob != nil {
		writeProblem(w, prob)
		return
	}
	var req claimRequest
	if prob := decodeBody(w, r, &req); prob != nil {
		writeProblem(w, prob)
		return
	}
	if len(req.Note) > MaxNoteBytes {
		writeProblem(w, problem.BadRequest(r.URL.Path,
			fmt.Sprintf("the note is %d bytes, over the %d an approval carries", len(req.Note), MaxNoteBytes),
			"Send a shorter note."))
		return
	}

	a, found, err := s.store.ClaimApproval(r.Context(), id, caller.User, req.Note, s.now().UTC())
	if prob := s.decisionProblem(r, id, found, err); prob != nil {
		writeProblem(w, prob)
		return
	}
	writeJSON(w, r.URL.Path, claimResponse{
		ID:        a.ID,
		Requester: a.Requester,
		Tool:      a.Tool,
		Input:     a.Input,
		State:     a.State,
	})
}

// rejectApproval refuses one queued operation with the reason the requester
// reads back.
func (s *Server) rejectApproval(w http.ResponseWriter, r *http.Request, caller Caller, id string) {
	if prob := s.requireAdmin(r, caller, "reject a queued operation"); prob != nil {
		writeProblem(w, prob)
		return
	}
	var req rejectRequest
	if prob := decodeBody(w, r, &req); prob != nil {
		writeProblem(w, prob)
		return
	}
	if len(req.Reason) < 3 || len(req.Reason) > MaxReasonBytes {
		writeProblem(w, problem.BadRequest(r.URL.Path,
			fmt.Sprintf("the reason is %d bytes, outside the 3 to %d an approval carries",
				len(req.Reason), MaxReasonBytes),
			"Send a reason the requester can act on, of 3 to 500 characters."))
		return
	}

	a, found, err := s.store.RejectApproval(r.Context(), id, caller.User, req.Reason, s.now().UTC())
	if prob := s.decisionProblem(r, id, found, err); prob != nil {
		writeProblem(w, prob)
		return
	}
	writeJSON(w, r.URL.Path, rejectResponse{ID: a.ID, State: a.State})
}

// resultApproval stores the outcome of an executed approval, once, and only
// from the admin who claimed it: the result is what the requester reads as the
// answer to their own call, so it comes from the session that ran it.
func (s *Server) resultApproval(w http.ResponseWriter, r *http.Request, caller Caller, id string) {
	if prob := s.requireAdmin(r, caller, "store the result of a queued operation"); prob != nil {
		writeProblem(w, prob)
		return
	}
	var req resultRequest
	if prob := decodeBody(w, r, &req); prob != nil {
		writeProblem(w, prob)
		return
	}
	if prob := approvalObject(r.URL.Path, "result", req.Result); prob != nil {
		writeProblem(w, prob)
		return
	}

	found, err := s.store.ResultApproval(r.Context(), id, caller.User, req.Result)
	switch {
	case err == nil && !found:
		writeProblem(w, approvalNotFound(r, id))
		return
	case errors.Is(err, store.ErrApprovalClaimant):
		writeProblem(w, problem.NotPermitted(r.URL.Path,
			fmt.Sprintf("the approval %s was claimed by another administrator", id),
			"The administrator who claimed an approval stores its result; claim one of your own."))
		return
	case errors.Is(err, store.ErrApprovalState):
		writeProblem(w, problem.ConflictFix(r.URL.Path,
			fmt.Sprintf("the approval %s already carries a result, or was never approved", id),
			"Read the approval to see what state it is in; a result is stored once."))
		return
	case err != nil:
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decisionProblem is the answer a claim or a reject gives when the approval is
// missing or is not pending.
func (s *Server) decisionProblem(r *http.Request, id string, found bool, err error) *problem.Problem {
	switch {
	case err == nil && !found:
		return approvalNotFound(r, id)
	case errors.Is(err, store.ErrApprovalState):
		return problem.ConflictFix(r.URL.Path,
			fmt.Sprintf("the approval %s is not pending", id),
			"Read the queue: an approval is decided once, and this one already was.")
	case err != nil:
		return problem.Internal(r.URL.Path, err.Error(), "")
	}
	return nil
}

func approvalNotFound(r *http.Request, id string) *problem.Problem {
	return problem.NotFoundFix(r.URL.Path,
		fmt.Sprintf("kitbashd has no approval %s", id),
		"List the approvals to see which ids are queued.")
}

// approvalObject checks that a field carries one JSON object within the size
// an approval holds. The tool's own input is not validated here: kitbashd
// queues the call and the admin's session validates it against the tool's
// schema when it runs it.
func approvalObject(instance, field string, raw json.RawMessage) *problem.Problem {
	if len(raw) > MaxApprovalBytes {
		return problem.TooLarge(instance,
			fmt.Sprintf("the %s is %d bytes, over the %d an approval carries", field, len(raw), MaxApprovalBytes),
			"Queue a smaller call; an approval carries at most a megabyte of arguments.")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return problem.BadRequest(instance,
			fmt.Sprintf("the %s is not a JSON object", field),
			fmt.Sprintf("Send the %s as the object the tool takes.", field))
	}
	return nil
}
