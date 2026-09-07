package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SubscriptionTelemetry is the one subscription a manifest can declare, see
// PLAN.md section 2.4.
const SubscriptionTelemetry = "telemetry"

// TokenBytes is how much entropy a Process token carries, see
// spec/kitbashd-api.yaml. The token itself is that many random bytes in
// base64url, returned once and never stored.
const TokenBytes = 32

// ErrProcessOwned reports a registration for an id another member already
// owns. Two members' Processes never share an id, so this is a conflict and
// not a replacement.
var ErrProcessOwned = errors.New("store: the Process id belongs to another member")

// ErrTooManyProcesses reports a member at their registration limit. The limit
// is the caller's, see daemon.MaxProcessesPerMember; the store enforces it
// inside the transaction, so two sessions registering at once cannot both pass
// a check and both write.
var ErrTooManyProcesses = errors.New("store: the member has too many Processes registered")

// Process is one registered Process: who runs it, what it runs, and what it
// asked to receive. The token is not part of it; only the hash of the token
// lives in the store, see spec/kitbashd-api.yaml.
//
// Every field is written even when it is empty, because this is also the shape
// processes_list answers with and kitbash-mcp reads: a key that comes and goes
// is a key a client has to guess at.
type Process struct {
	ID            string    `json:"id"`
	Owner         string    `json:"owner"`
	Admin         bool      `json:"admin"`
	Package       string    `json:"package"`
	Name          string    `json:"name"`
	Expose        string    `json:"expose"`
	Endpoint      string    `json:"endpoint"`
	Subscriptions []string  `json:"subscriptions"`
	RegisteredAt  time.Time `json:"registeredAt"`
}

// Subscribes reports whether this Process asked for the Telemetry fan out.
func (p Process) Subscribes() bool {
	for _, s := range p.Subscriptions {
		if s == SubscriptionTelemetry {
			return true
		}
	}
	return false
}

// NewToken mints one Process token and returns it with its hash. The token is
// handed to the caller once; the store never sees it again, so a leaked store
// cannot be turned back into a token.
func NewToken() (token, hash string, err error) {
	var b [TokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("store: mint a Process token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b[:])
	return token, HashToken(token), nil
}

// HashToken is how a token becomes the value the store holds and looks up by.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// RegisterProcess writes one Process and its token hash. Registering an id the
// same owner already holds replaces the record and revokes the old token,
// which is what makes a re-run of the same Process safe to repeat. The same id
// held by another member is ErrProcessOwned.
//
// maxPerOwner bounds how many Processes one member may hold; zero means no
// bound, which is what a caller not exercising the limit passes. A replacement
// is not a new Process and is never refused by it.
func (s *Store) RegisterProcess(ctx context.Context, p Process, tokenHash string, maxPerOwner int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()

	var owner string
	replacing := true
	err = tx.QueryRowContext(ctx, "SELECT owner FROM processes WHERE id = ?", p.ID).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		replacing = false
	case err != nil:
		return fmt.Errorf("store: read the Process %s: %w", p.ID, err)
	case owner != p.Owner:
		return ErrProcessOwned
	}
	// The count is inside the transaction, so two sessions registering at the
	// same moment cannot both read a count under the limit and both write.
	if !replacing && maxPerOwner > 0 {
		var held int
		if err := tx.QueryRowContext(ctx,
			"SELECT count(*) FROM processes WHERE owner = ?", p.Owner).Scan(&held); err != nil {
			return fmt.Errorf("store: count the Processes of %s: %w", p.Owner, err)
		}
		if held >= maxPerOwner {
			return ErrTooManyProcesses
		}
	}

	subscriptions, err := json.Marshal(subscriptionList(p.Subscriptions))
	if err != nil {
		return fmt.Errorf("store: encode subscriptions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO processes
		(id, owner, admin, package, name, expose, endpoint, subscriptions, token_hash, registered_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			owner = excluded.owner, admin = excluded.admin, package = excluded.package,
			name = excluded.name, expose = excluded.expose, endpoint = excluded.endpoint,
			subscriptions = excluded.subscriptions, token_hash = excluded.token_hash,
			registered_at = excluded.registered_at`,
		p.ID, p.Owner, p.Admin, p.Package, p.Name, p.Expose, p.Endpoint,
		string(subscriptions), tokenHash, p.RegisteredAt.UnixNano()); err != nil {
		return fmt.Errorf("store: register the Process %s: %w", p.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// DeleteProcess removes one Process, which revokes its token. It reports
// whether there was one to remove.
func (s *Store) DeleteProcess(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM processes WHERE id = ?", id)
	if err != nil {
		return false, fmt.Errorf("store: unregister the Process %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: unregister the Process %s: %w", id, err)
	}
	return n > 0, nil
}

// Process reads one registered Process by id.
func (s *Store) Process(ctx context.Context, id string) (Process, bool, error) {
	return s.process(ctx, "id = ?", id)
}

// ProcessByToken reads the Process a token names. The lookup is by the hash of
// the token, so the store holds nothing that could be replayed as a token.
func (s *Store) ProcessByToken(ctx context.Context, token string) (Process, bool, error) {
	return s.process(ctx, "token_hash = ?", HashToken(token))
}

func (s *Store) process(ctx context.Context, where string, arg any) (Process, bool, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT id, owner, admin, package, name, expose, endpoint, subscriptions, registered_at FROM processes WHERE "+where, arg)
	p, err := scanProcess(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Process{}, false, nil
	}
	if err != nil {
		return Process{}, false, err
	}
	return p, true, nil
}

// Processes lists registered Processes, newest first. An empty owner lists
// every member's, which is what an admin reads and what the fan out loads.
func (s *Store) Processes(ctx context.Context, owner string) ([]Process, error) {
	query := "SELECT id, owner, admin, package, name, expose, endpoint, subscriptions, registered_at FROM processes"
	var args []any
	if owner != "" {
		query += " WHERE owner = ?"
		args = append(args, owner)
	}
	query += " ORDER BY registered_at DESC, id DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list Processes: %w", err)
	}
	defer rows.Close()
	out := []Process{}
	for rows.Next() {
		p, err := scanProcess(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list Processes: %w", err)
	}
	return out, nil
}

// scanner is what both a single row and a row of a result set satisfy.
type scanner interface {
	Scan(dest ...any) error
}

func scanProcess(row scanner) (Process, error) {
	var p Process
	var subscriptions string
	var registered int64
	if err := row.Scan(&p.ID, &p.Owner, &p.Admin, &p.Package, &p.Name, &p.Expose, &p.Endpoint,
		&subscriptions, &registered); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Process{}, err
		}
		return Process{}, fmt.Errorf("store: read a Process: %w", err)
	}
	p.RegisteredAt = time.Unix(0, registered).UTC()
	p.Subscriptions = []string{}
	if subscriptions != "" {
		if err := json.Unmarshal([]byte(subscriptions), &p.Subscriptions); err != nil {
			return Process{}, fmt.Errorf("store: read the subscriptions of %s: %w", p.ID, err)
		}
	}
	return p, nil
}

// subscriptionList keeps the column a JSON array even when the Process
// declared none, so a reader never has to tell null from empty.
func subscriptionList(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// migrate adds what a store written by an earlier version does not have. Only
// additive changes belong here: a column with a default, never a drop or a
// rewrite, so a downgrade keeps reading the same records.
func migrate(db *sql.DB) error {
	for _, table := range []string{"spans", "logs", "metrics"} {
		has, err := hasColumn(db, table, "producer")
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN producer TEXT NOT NULL DEFAULT ''", table)); err != nil {
			return fmt.Errorf("store: add the producer column to %s: %w", table, err)
		}
	}
	return nil
}

// hasColumn reports whether a table already carries a column. A table that
// does not exist has no columns, which is the answer a fresh store gives
// before the schema runs.
func hasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("store: read the columns of %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var index int
		var name, kind string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&index, &name, &kind, &notNull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("store: read the columns of %s: %w", table, err)
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("store: read the columns of %s: %w", table, err)
	}
	return false, nil
}
