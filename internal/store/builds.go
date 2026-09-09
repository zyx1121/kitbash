package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// MaxBuildsListed bounds one answer of Builds. A caller asks about one commit
// or one digest and reads the newest few; nobody reads four thousand rows.
const MaxBuildsListed = 100

// ErrNoBuild reports a build kitbashd has no record of, which is what a fetch
// of an image nobody recorded answers.
var ErrNoBuild = errors.New("store: no such build")

// Build is one image built from one commit of one Package. It is the record
// that makes an image shareable: kitbashd copies an image between two members'
// stores only for a build it has a row for, see PLAN.md section 2.2.
//
// Path, Commit and Digest together are one build, and the same three recorded
// twice replace the row rather than adding one: two members who built the same
// commit to the same image are one build, and Builder names the one kitbashd
// asks for a copy.
//
// The commit is the commit_sha column because commit is SQL's own word, and a
// column that has to be quoted everywhere is a column somebody forgets to
// quote once.
type Build struct {
	Path    string    `json:"path"`
	Commit  string    `json:"commit"`
	Digest  string    `json:"digest"`
	Builder string    `json:"builder"`
	BuiltAt time.Time `json:"builtAt"`
	Size    int64     `json:"size,omitempty"`
}

// BuildFilter selects build records. Every field is an exact match and an
// empty one matches everything; Limit defaults to MaxBuildsListed.
type BuildFilter struct {
	Path    string
	Commit  string
	Digest  string
	Builder string
	Limit   int
}

// BuildLimits bounds what the build table holds. Both are the caller's, see
// daemon.MaxBuildsPerPath and daemon.MaxBuildsPerBuilder, and both are applied
// inside the transaction that writes, so two sessions recording at once cannot
// both write past them. A zero field is no bound, which is what a caller not
// exercising that cap passes.
//
// PerBuilder is the one that matters for safety: without it a member could
// record enough rows of one path to prune everybody else's out of the table.
type BuildLimits struct {
	PerPath    int
	PerBuilder int
}

// RecordBuild writes one build record and prunes the oldest rows beyond the
// limits.
//
// The builder of an existing row is never replaced. Recording a triple that is
// already there refreshes the time and the size for the member who recorded
// it, and does nothing at all for anybody else: the row names the member
// kitbashd asks for a copy of this image, so a member who could take it over
// could point every other member's fetch at themselves.
func (s *Store) RecordBuild(ctx context.Context, b Build, limits BuildLimits) error {
	if b.Path == "" || b.Commit == "" || b.Digest == "" || b.Builder == "" {
		return errors.New("store: a build record needs a path, a commit, a digest and a builder")
	}
	if b.BuiltAt.IsZero() {
		b.BuiltAt = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()

	// The WHERE on the update is what keeps the builder: a row of another
	// member's is left exactly as it is, and the insert reports no error,
	// because two members holding one image of one commit is the normal case
	// and not something to refuse.
	if _, err := tx.ExecContext(ctx, `INSERT INTO builds
		(path, commit_sha, digest, builder, built_at, size)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(path, commit_sha, digest) DO UPDATE SET
			built_at = excluded.built_at, size = excluded.size
		WHERE builds.builder = excluded.builder`,
		b.Path, b.Commit, b.Digest, b.Builder, b.BuiltAt.UnixNano(), b.Size); err != nil {
		return fmt.Errorf("store: record the build of %s: %w", b.Path, err)
	}
	// Both prunes run in the same transaction as the insert, so the caps hold
	// however many sessions record a build of one path at the same moment.
	//
	// The per builder prune comes first and is the one that bounds what one
	// member can do: it takes only their own rows, so no member can push
	// another member's record out of the table.
	if limits.PerBuilder > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM builds
			WHERE path = ? AND builder = ? AND id NOT IN
			(SELECT id FROM builds WHERE path = ? AND builder = ?
			 ORDER BY built_at DESC, id DESC LIMIT ?)`,
			b.Path, b.Builder, b.Path, b.Builder, limits.PerBuilder); err != nil {
			return fmt.Errorf("store: prune the builds of %s by %s: %w", b.Path, b.Builder, err)
		}
	}
	if limits.PerPath > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM builds WHERE path = ? AND id NOT IN
			(SELECT id FROM builds WHERE path = ? ORDER BY built_at DESC, id DESC LIMIT ?)`,
			b.Path, b.Path, limits.PerPath); err != nil {
			return fmt.Errorf("store: prune the builds of %s: %w", b.Path, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// Builds lists the build records that match the filter, newest first.
func (s *Store) Builds(ctx context.Context, f BuildFilter) ([]Build, error) {
	query := `SELECT path, commit_sha, digest, builder, built_at, size FROM builds`
	var args []any
	clauses := 0
	for _, pair := range [][2]string{
		{"path", f.Path}, {"commit_sha", f.Commit}, {"digest", f.Digest}, {"builder", f.Builder},
	} {
		if pair[1] == "" {
			continue
		}
		if clauses == 0 {
			query += " WHERE "
		} else {
			query += " AND "
		}
		query += pair[0] + " = ?"
		args = append(args, pair[1])
		clauses++
	}
	limit := f.Limit
	if limit <= 0 || limit > MaxBuildsListed {
		limit = MaxBuildsListed
	}
	query += " ORDER BY built_at DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list the builds of %s: %w", f.Path, err)
	}
	defer rows.Close()
	out := []Build{}
	for rows.Next() {
		var b Build
		var built int64
		if err := rows.Scan(&b.Path, &b.Commit, &b.Digest, &b.Builder, &built, &b.Size); err != nil {
			return nil, fmt.Errorf("store: read a build: %w", err)
		}
		b.BuiltAt = time.Unix(0, built).UTC()
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list the builds of %s: %w", f.Path, err)
	}
	return out, nil
}

// BuildCount is how many records one path holds, which is what the prune
// bounds. It is a call of its own because Builds answers one page.
func (s *Store) BuildCount(ctx context.Context, path string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		"SELECT count(*) FROM builds WHERE path = ?", path).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count the builds of %s: %w", path, err)
	}
	return n, nil
}

// DeleteBuildsByBuilder removes every build one member recorded, which is part
// of removing that member: a row naming an account that is gone points at an
// image store that went with it.
func (s *Store) DeleteBuildsByBuilder(ctx context.Context, builder string) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM builds WHERE builder = ?", builder)
	if err != nil {
		return 0, fmt.Errorf("store: delete the builds of %s: %w", builder, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete the builds of %s: %w", builder, err)
	}
	return n, nil
}
