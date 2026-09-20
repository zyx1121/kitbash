package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Removing, Removed and Failed are the three states of one member's removal.
// A member is removing from the moment users_remove is answered until the job
// kitbashd runs for it has been through every step, and then it is removed, or
// failed with the step that failed on it, see
// internal/daemon/removal.go.
const (
	Removing = "removing"
	Removed  = "removed"
	Failed   = "failed"
)

// Removal is one member's removal as this host remembers it. The row is what
// makes the job survive a restart: a daemon that stopped halfway through
// reads the removing rows at its next start and goes on from the first step,
// because every step is idempotent, see PLAN.md section 2.3.
type Removal struct {
	Name string `json:"user"`
	// State is removing, removed or failed.
	State string `json:"state"`
	// Step is the step that failed, empty for every other state. It is the
	// word an admin reads through users_list to know what is left to do by
	// hand.
	Step string `json:"step,omitempty"`
	// Archived is where the home went, written by the step that moved it and
	// empty until then.
	Archived   string    `json:"archived,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
}

// BeginRemoval marks one member removing and reports whether this call is the
// one that started it. A member already being removed answers false, which is
// what makes a second users_remove the same answer rather than a second job: a
// removal is a state the caller asked for, not a queue.
//
// A row of a removal that is over is replaced, because a name removed once and
// created again is a member of its own and its removal is not the one that is
// recorded here.
func (s *Store) BeginRemoval(ctx context.Context, name string, at time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: begin the removal of %s: %w", name, err)
	}
	defer tx.Rollback()

	var state string
	err = tx.QueryRowContext(ctx, "SELECT state FROM removals WHERE name = ?", name).Scan(&state)
	switch {
	case err == nil && state == Removing:
		return false, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("store: read the removal of %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO removals (name, state, step, archived, started_at, finished_at)
		 VALUES (?, ?, '', '', ?, 0)
		 ON CONFLICT(name) DO UPDATE SET
		   state = excluded.state, step = '', archived = '',
		   started_at = excluded.started_at, finished_at = 0`,
		name, Removing, at.UnixNano()); err != nil {
		return false, fmt.Errorf("store: begin the removal of %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: begin the removal of %s: %w", name, err)
	}
	return true, nil
}

// FinishRemoval writes how one removal ended: removed, or failed with the step
// that failed on it and wherever the home reached.
func (s *Store) FinishRemoval(ctx context.Context, name, state, step, archived string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		"UPDATE removals SET state = ?, step = ?, archived = ?, finished_at = ? WHERE name = ?",
		state, step, archived, at.UnixNano(), name); err != nil {
		return fmt.Errorf("store: record the removal of %s: %w", name, err)
	}
	return nil
}

// DeleteRemoval forgets one removal, which is what creating a member of that
// name again does: the account is new and the record of the old one is not
// about it.
func (s *Store) DeleteRemoval(ctx context.Context, name string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM removals WHERE name = ?", name); err != nil {
		return fmt.Errorf("store: forget the removal of %s: %w", name, err)
	}
	return nil
}

// Removal answers what this host remembers about one member's removal.
func (s *Store) Removal(ctx context.Context, name string) (Removal, bool, error) {
	list, err := s.removals(ctx, "WHERE name = ?", name)
	if err != nil || len(list) == 0 {
		return Removal{}, false, err
	}
	return list[0], true, nil
}

// Removals answers every removal this host remembers, which is what users_list
// carries: a member being removed is still on the host, and one whose removal
// failed is a name an admin has work left on.
func (s *Store) Removals(ctx context.Context) ([]Removal, error) {
	return s.removals(ctx, "")
}

// RemovalsInState answers the removals in one state, which is how a daemon
// that has just started finds the jobs it has to resume.
func (s *Store) RemovalsInState(ctx context.Context, state string) ([]Removal, error) {
	return s.removals(ctx, "WHERE state = ?", state)
}

func (s *Store) removals(ctx context.Context, where string, args ...any) ([]Removal, error) {
	query := "SELECT name, state, step, archived, started_at, finished_at FROM removals"
	if where != "" {
		query += " " + where
	}
	query += " ORDER BY name"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read the removals: %w", err)
	}
	defer rows.Close()
	var list []Removal
	for rows.Next() {
		var r Removal
		var started, finished int64
		if err := rows.Scan(&r.Name, &r.State, &r.Step, &r.Archived, &started, &finished); err != nil {
			return nil, fmt.Errorf("store: read the removals: %w", err)
		}
		r.StartedAt = time.Unix(0, started).UTC()
		if finished > 0 {
			r.FinishedAt = time.Unix(0, finished).UTC()
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read the removals: %w", err)
	}
	return list, nil
}
