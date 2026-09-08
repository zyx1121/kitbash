package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The states one approval moves through. There is no fourth: an approval is
// waiting, it was approved, or it was refused, see PLAN.md section 2.1.
const (
	StatePending  = "pending"
	StateApproved = "approved"
	StateRejected = "rejected"
)

// The tools that queue rather than fail when a member names a path under /org.
// Everything else outside the caller's own space is simply not permitted, see
// PLAN.md section 2.1.
const (
	ToolFSWrite   = "fs_write"
	ToolPkgImport = "pkg_import"
)

// The failures an approval transition reports. Anything else is a store
// failure the daemon answers as internal.
var (
	// ErrApprovalState reports a transition the approval is not in a state
	// for: claiming one that is not pending, or storing a second result.
	ErrApprovalState = errors.New("store: the approval is not in that state")
	// ErrApprovalClaimant reports an admin storing a result on an approval
	// another admin claimed.
	ErrApprovalClaimant = errors.New("store: the approval was claimed by another administrator")
	// ErrTooManyApprovals reports a member at their pending approval limit.
	ErrTooManyApprovals = errors.New("store: the member has too many pending approvals")
)

// Approval is one queued operation, in the shape spec/kitbashd-api.yaml
// answers approvals_list with. The optional fields are absent until an admin
// decides, so a requester reading the queue never has to tell an empty note
// from a note that was not written yet.
//
// Input and Result are the tool's own JSON, held verbatim: kitbashd queues the
// call, it does not understand it.
type Approval struct {
	ID          string          `json:"id"`
	Requester   string          `json:"requester"`
	Tool        string          `json:"tool"`
	Input       json.RawMessage `json:"input"`
	State       string          `json:"state"`
	RequestedAt time.Time       `json:"requestedAt"`
	DecidedAt   *time.Time      `json:"decidedAt,omitempty"`
	DecidedBy   string          `json:"decidedBy,omitempty"`
	Note        string          `json:"note,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

// CreateApproval queues one operation. maxPending bounds how many a member may
// have waiting; zero means no bound. The count is inside the transaction, so
// two sessions queueing at once cannot both pass a check and both write.
func (s *Store) CreateApproval(ctx context.Context, a Approval, maxPending int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()

	if maxPending > 0 {
		var held int
		if err := tx.QueryRowContext(ctx,
			"SELECT count(*) FROM approvals WHERE requester = ? AND state = ?",
			a.Requester, StatePending).Scan(&held); err != nil {
			return fmt.Errorf("store: count the approvals of %s: %w", a.Requester, err)
		}
		if held >= maxPending {
			return ErrTooManyApprovals
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO approvals
		(id, requester, tool, input, state, requested_at)
		VALUES (?,?,?,?,?,?)`,
		a.ID, a.Requester, a.Tool, string(a.Input), StatePending, a.RequestedAt.UnixNano()); err != nil {
		return fmt.Errorf("store: queue the approval %s: %w", a.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// Approval reads one queued operation by id.
func (s *Store) Approval(ctx context.Context, id string) (Approval, bool, error) {
	row := s.db.QueryRowContext(ctx, approvalColumns+" FROM approvals WHERE id = ?", id)
	a, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, false, nil
	}
	if err != nil {
		return Approval{}, false, err
	}
	return a, true, nil
}

// Approvals lists queued operations, newest first. An empty requester lists
// every member's, which is what an admin reads; an empty state lists them all.
func (s *Store) Approvals(ctx context.Context, requester, state string) ([]Approval, error) {
	query := approvalColumns + " FROM approvals"
	var args []any
	var where []string
	if requester != "" {
		where = append(where, "requester = ?")
		args = append(args, requester)
	}
	if state != "" {
		where = append(where, "state = ?")
		args = append(args, state)
	}
	for i, clause := range where {
		if i == 0 {
			query += " WHERE " + clause
			continue
		}
		query += " AND " + clause
	}
	query += " ORDER BY requested_at DESC, id DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list approvals: %w", err)
	}
	defer rows.Close()
	out := []Approval{}
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list approvals: %w", err)
	}
	return out, nil
}

// ClaimApproval moves one approval from pending to approved and records who
// took it. The admin's session then runs the tool and stores the outcome with
// ResultApproval, see spec/kitbashd-api.yaml.
//
// The state is read and written in one transaction, so two admins claiming the
// same approval at the same moment cannot both be told they have it.
func (s *Store) ClaimApproval(ctx context.Context, id, by, note string, at time.Time) (Approval, bool, error) {
	return s.decide(ctx, id, StateApproved, by, note, "", at)
}

// RejectApproval moves one approval from pending to rejected with the reason
// the requester reads.
func (s *Store) RejectApproval(ctx context.Context, id, by, reason string, at time.Time) (Approval, bool, error) {
	return s.decide(ctx, id, StateRejected, by, "", reason, at)
}

// decide is the one transition both decisions make: pending to something else,
// stamped with the admin and the time.
func (s *Store) decide(ctx context.Context, id, state, by, note, reason string, at time.Time) (Approval, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Approval{}, false, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()

	var current string
	err = tx.QueryRowContext(ctx, "SELECT state FROM approvals WHERE id = ?", id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, false, nil
	}
	if err != nil {
		return Approval{}, false, fmt.Errorf("store: read the approval %s: %w", id, err)
	}
	if current != StatePending {
		return Approval{}, true, ErrApprovalState
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE approvals SET state = ?, decided_by = ?, decided_at = ?, note = ?, reason = ? WHERE id = ?",
		state, by, at.UnixNano(), note, reason, id); err != nil {
		return Approval{}, true, fmt.Errorf("store: decide the approval %s: %w", id, err)
	}

	row := tx.QueryRowContext(ctx, approvalColumns+" FROM approvals WHERE id = ?", id)
	a, err := scanApproval(row)
	if err != nil {
		return Approval{}, true, err
	}
	if err := tx.Commit(); err != nil {
		return Approval{}, true, fmt.Errorf("store: commit: %w", err)
	}
	return a, true, nil
}

// ResultApproval stores the outcome of an approved operation, once. Only the
// admin who claimed it may write it: the result is what the requester reads
// back as the answer to their call, so it comes from the session that ran it.
func (s *Store) ResultApproval(ctx context.Context, id, by string, result json.RawMessage) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()

	var state, decidedBy, stored string
	err = tx.QueryRowContext(ctx,
		"SELECT state, decided_by, result FROM approvals WHERE id = ?", id).Scan(&state, &decidedBy, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read the approval %s: %w", id, err)
	}
	if state != StateApproved {
		return true, ErrApprovalState
	}
	if decidedBy != by {
		return true, ErrApprovalClaimant
	}
	if stored != "" {
		return true, ErrApprovalState
	}
	if _, err := tx.ExecContext(ctx, "UPDATE approvals SET result = ? WHERE id = ?",
		string(result), id); err != nil {
		return true, fmt.Errorf("store: store the result of %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return true, fmt.Errorf("store: commit: %w", err)
	}
	return true, nil
}

// DeleteApprovals removes every approval of one member, which is part of
// removing that member: nothing they queued is executed after their account is
// gone, see spec/kitbashd-api.yaml.
func (s *Store) DeleteApprovals(ctx context.Context, requester string) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM approvals WHERE requester = ?", requester)
	if err != nil {
		return 0, fmt.Errorf("store: delete the approvals of %s: %w", requester, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete the approvals of %s: %w", requester, err)
	}
	return n, nil
}

// approvalColumns is the one select every read of this table shares.
const approvalColumns = `SELECT id, requester, tool, input, state, requested_at,
	decided_at, decided_by, note, reason, result`

func scanApproval(row scanner) (Approval, error) {
	var a Approval
	var input, result string
	var requested, decided int64
	if err := row.Scan(&a.ID, &a.Requester, &a.Tool, &input, &a.State, &requested,
		&decided, &a.DecidedBy, &a.Note, &a.Reason, &result); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Approval{}, err
		}
		return Approval{}, fmt.Errorf("store: read an approval: %w", err)
	}
	a.RequestedAt = time.Unix(0, requested).UTC()
	if decided > 0 {
		at := time.Unix(0, decided).UTC()
		a.DecidedAt = &at
	}
	// An empty column is an approval with no input or no result yet, which is
	// absent from the answer rather than the four characters of a JSON null.
	if input != "" {
		a.Input = json.RawMessage(input)
	}
	if result != "" {
		a.Result = json.RawMessage(result)
	}
	return a, nil
}
