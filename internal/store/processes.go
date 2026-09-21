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

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/mounts"
	"github.com/zyx1121/kitbash/internal/podman"
)

// SubscriptionTelemetry is the one subscription a manifest can declare, see
// PLAN.md section 2.4.
const SubscriptionTelemetry = "telemetry"

// TokenBytes is how much entropy a Process token carries, see
// spec/kitbashd-api.yaml. The token itself is that many random bytes in
// base64url, returned once and never stored.
const TokenBytes = 32

// FanoutSecretBytes is how much entropy the bearer of a fan out request
// carries, see fan_out.authentication in spec/kitbashd-api.yaml. It is as wide
// as a Process token, and unlike the token it is kept in the clear: kitbashd
// is the sender, so a hash would leave it with nothing to send.
const FanoutSecretBytes = 32

// ErrProcessOwned reports a registration for an id another member already
// owns. Two members' Processes never share an id, so this is a conflict and
// not a replacement.
var ErrProcessOwned = errors.New("store: the Process id belongs to another member")

// ErrTooManyProcesses reports a member at their registration limit. The limit
// is the caller's, see daemon.MaxProcessesPerMember; the store enforces it
// inside the transaction, so two sessions registering at once cannot both pass
// a check and both write.
var ErrTooManyProcesses = errors.New("store: the member has too many Processes registered")

// ErrTooManyScheduled reports a member at their limit of jobs. It is a second
// bound under the first: every job is a container the host starts by itself,
// so what it costs is the ticker's time and not only a registration, see
// daemon.MaxScheduledPerMember.
var ErrTooManyScheduled = errors.New("store: the member has too many scheduled Processes registered")

// Quota is what one member may hold, checked inside the transaction that
// writes a registration: how many Processes, and how many of those may be
// jobs. A zero field is no bound, which is what a caller replacing a record
// passes, and a replacement is never refused by either.
type Quota struct {
	Processes int
	Scheduled int
}

// Process is one registered Process: who runs it, what it runs, and what it
// asked to receive. The token is not part of it; only the hash of the token
// lives in the store, see spec/kitbashd-api.yaml.
//
// Every field is written even when it is empty, because this is also the shape
// processes_list answers with and kitbash-mcp reads: a key that comes and goes
// is a key a client has to guess at.
type Process struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
	Admin bool   `json:"admin"`
	// Package is the folder the Process was built from, Container the name
	// the runtime holds it under and Digest the image it runs. The last two
	// are what boot restore needs: with them kitbashd starts a Process again
	// without reading a manifest, see spec/kitbashd-api.yaml.
	Package   string `json:"package"`
	Name      string `json:"name"`
	Container string `json:"container"`
	Digest    string `json:"digest"`
	Expose    string `json:"expose"`
	Endpoint  string `json:"endpoint"`
	// Hostname is the name this Process's unit declared for itself, empty for
	// one that declared none and is served under the name kitbashd derives.
	// It travels with the registration because the routing table is rebuilt
	// from these rows at every start: after a reboot nothing else remembers
	// which name a Process holds, see PLAN.md section 2.3.
	Hostname      string    `json:"hostname,omitempty"`
	Subscriptions []string  `json:"subscriptions"`
	RegisteredAt  time.Time `json:"registeredAt"`
	// Runner is the Package path of the run kit that owns this Process, empty
	// for every Process kitbashd runs itself. A Process a kit owns has no
	// container on this host, so this is what restore reads to leave it alone
	// rather than unregister it as a registration that names nothing, see
	// PLAN.md section 3.
	Runner string `json:"runner"`
	// Permits is what this Process may call back over /mcp, as its Package's
	// manifest declared it at registration. It is listed like the rest of the
	// record: what a Process may do is not a secret from its owner, and
	// pkg_inspect already shows the same block. A registration written before
	// permits existed carries none, which permits nothing.
	Permits manifest.Permits `json:"permits"`
	// Limits is the ceiling this Process runs under, as the start that created
	// its container received it: the manifest's own spelling of memory and
	// cpu, and the number of processes kitbashd bounded it to. It is listed
	// like the rest of the record, and it is what restore writes into the
	// Process's cgroup again after a reboot, when the cgroup filesystem is
	// empty and nothing else remembers. A registration written before this
	// carries none and restores without a ceiling until it is run again.
	Limits Limits `json:"limits"`
	// Health is the probe this Process declares and the result of the most
	// recent one kitbashd ran, see internal/daemon/health.go. Only the
	// declaration is stored, because a probe is a reading of a moment and
	// this table is the registration.
	Health Health `json:"health"`
	// Mounts is the folders of Files this Process sees, as kitbashd resolved
	// them when the Process was registered. The registration is authoritative:
	// a start and a restore mount what is recorded here and never what a
	// request or a manifest claims today, see PLAN.md section 2.3. They are
	// listed like the rest of the record, because what a Process can read is
	// not a secret from its owner. A registration written before mounts
	// existed carries none, which is a Process that sees no Files.
	Mounts []mounts.Resolved `json:"mounts,omitempty"`
	// Secrets is the names of the values this Process is given as environment,
	// as its unit declared them. Only the names are here and only the names
	// are ever stored: the values live with the member, outside the database,
	// and every start resolves each name to the owner's current value, which
	// is what makes rotation one call to secrets_set, see PLAN.md section 2.3.
	// They are listed like the rest of the record, because which credentials a
	// Process was given is not a secret from its owner; what a listing never
	// carries is a value. A registration written before secrets existed
	// carries none, which is a Process that is given none.
	Secrets []string `json:"secrets,omitempty"`
	// Schedule is the cron of a scheduled unit and what its ticks start the
	// container with. A registration that carries one is a job and not a
	// Process that stays up: kitbashd starts its container at each tick and
	// nothing else does, see internal/daemon/schedule.go. It is listed like
	// the rest of the record, because when a job runs is not a secret from
	// its owner. A registration written before schedules existed carries
	// none, which is a Process nothing starts on time.
	Schedule Schedule `json:"schedule,omitempty"`
	// Composition is the pod this Process runs as, for a Package that
	// declares more than one unit. A Package that declares one runs as the
	// bare container Container names and carries none of this, so nothing
	// about a single unit Process changed when pods arrived, see PLAN.md
	// section 5.6. A registration written before pods existed carries none
	// and is read as the single unit Process it is.
	//
	// The fields above describe the face: Container, Digest, Expose,
	// Endpoint, Mounts and Secrets are the exposed unit's, so every reader
	// that knew one container per Process still reads the one that answers.
	Composition Composition `json:"composition,omitempty"`
	// FanoutSecret is the bearer kitbashd puts on every fan out request to
	// this Process. It is minted with the token at registration and handed to
	// the container once. The tag keeps it out of processes_list, which is the
	// one place this struct is serialised for a caller: a listing carries no
	// secret any more than it carries a token. A registration written before
	// the fan out was authenticated carries none.
	FanoutSecret string `json:"-"`
}

// Composition is one Process that runs as a pod: the pod the units share and
// the units themselves, in the order the manifest declared them. It is the
// whole of what a start and a boot restore need beyond the row's own fields.
//
// Declared is what every reader branches on, and it is two units or more by
// construction: a Package with one unit runs as a bare container and is never
// written here, so a reader that sees a composition sees a pod.
type Composition struct {
	Pod   string `json:"pod"`
	Units []Unit `json:"units"`
}

// Unit is one container of a pod as the registration records it: the name the
// manifest gave it, the container the runtime holds it under, the image it
// runs, whether it is the Process's face, and the three things kitbashd
// resolves or enforces per unit.
//
// What the unit runs and the environment it runs with are not here, for the
// reason a single unit Process does not carry them either: a start sends them
// and a restore reads them back off the container the runtime still has, see
// internal/daemon/restore.go.
type Unit struct {
	Name      string `json:"name"`
	Container string `json:"container"`
	Digest    string `json:"digest"`
	// Face is the one unit that declares expose mcp or http, which is the
	// unit the Process's endpoint, health probe and route belong to. Exactly
	// one unit of a Package carries it, see PLAN.md section 5.6.
	Face bool `json:"face,omitempty"`
	// Mounts are the folders of Files this unit sees, as kitbashd resolved
	// them at registration. They are per unit the way they are per Process,
	// and the registration is authoritative for these as it is for the rest.
	Mounts []mounts.Resolved `json:"mounts,omitempty"`
	// Secrets are the names this unit is given the owner's values under.
	// Names only, ever, the same rule the Process's own list follows.
	Secrets []string `json:"secrets,omitempty"`
	// Limits is what this unit may spend, under the Process's ceiling. The
	// pod is the ceiling and the units are placed below it, so two units of
	// one Process cannot together spend more than the Process was given.
	Limits Limits `json:"limits,omitempty"`
}

// Declared reports whether this registration is a pod at all. A Process of one
// unit is not, which is what keeps every path that knew one container per
// Process the path a single unit Package still takes.
func (c Composition) Declared() bool { return c.Pod != "" && len(c.Units) > 1 }

// Face is the unit that is the Process's face, and false for a composition
// that declares none, which is a row nothing this release wrote.
func (c Composition) Face() (Unit, bool) {
	for _, u := range c.Units {
		if u.Face {
			return u, true
		}
	}
	return Unit{}, false
}

// Names is the unit names of this Process, which is what a Telemetry record
// carrying kitbash.unit is held to: a name that is not one of these is a
// producer naming a unit of somebody else's Process, see PLAN.md section 2.4.
func (c Composition) Names() []string {
	names := make([]string, 0, len(c.Units))
	for _, u := range c.Units {
		names = append(names, u.Name)
	}
	return names
}

// JSON renders the composition for the column. A Process that is not a pod is
// an empty string rather than an object of nulls, so a legacy row and a single
// unit Process read back the same.
func (c Composition) JSON() string {
	if !c.Declared() {
		return ""
	}
	body, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(body)
}

// Health is the probe of one Process: the path its Package's manifest declared
// under deploy.units[0].health.http, how often kitbashd requests it, and what
// the most recent request saw.
//
// HTTP and Interval are the registration and are stored. Last and Healthy are
// the prober's own memory, filled in when processes_list is answered: a daemon
// that has just started lists a declaration with no result yet, and nothing is
// written to the table when a probe runs. The records a probe writes are
// Telemetry, see PLAN.md section 2.4.
type Health struct {
	HTTP     string `json:"http,omitempty"`
	Interval string `json:"interval,omitempty"`
	Last     string `json:"last,omitempty"`
	Healthy  *bool  `json:"healthy,omitempty"`
}

// Declared reports whether this Process declares an HTTP probe at all.
func (h Health) Declared() bool { return h.HTTP != "" }

// JSON renders the declaration for the column, and nothing a probe saw. A
// Process that declares no probe is an empty string rather than an object of
// nulls, so a legacy row and a Process without a probe read back the same.
func (h Health) JSON() string {
	if !h.Declared() {
		return ""
	}
	body, err := json.Marshal(Health{HTTP: h.HTTP, Interval: h.Interval})
	if err != nil {
		return ""
	}
	return string(body)
}

// Schedule is one job: the cron expression its owner declared, and what a tick
// has to start the container with. The three beyond the expression are there
// because a tick has no session behind it: the environment a unit declares,
// and the ceiling it asks for, reach kitbashd with the registration or they
// never reach it at all, see PLAN.md section 2.3.
//
// The values a job is given are not here and never will be: the secret names
// are the registration's own, and every run resolves each of them to the
// owner's current value, the same as any start.
type Schedule struct {
	Cron    string            `json:"cron"`
	Env     map[string]string `json:"env,omitempty"`
	Command []string          `json:"command,omitempty"`
	Memory  string            `json:"memory,omitempty"`
	CPU     string            `json:"cpu,omitempty"`
}

// Declared reports whether this registration is a job at all.
func (s Schedule) Declared() bool { return s.Cron != "" }

// JSON renders the schedule for the column. A Process that declares none is an
// empty string rather than an object of nulls, so a legacy row and a Process
// that is not a job read back the same.
func (s Schedule) JSON() string {
	if !s.Declared() {
		return ""
	}
	body, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(body)
}

// Limits is one Process's ceiling, in the spelling the start request carried:
// memory and cpu as the manifest declares them, pids as kitbashd bounded it.
// Converting them is the caller's, so what is stored is what was asked for
// rather than one host's reading of it.
type Limits struct {
	Memory string `json:"memory,omitempty"`
	CPU    string `json:"cpu,omitempty"`
	Pids   int    `json:"pids,omitempty"`
	// Ceiling says kitbashd made this Process a ceiling cgroup of its own and
	// created its container under it. It is what tells a restore that a
	// Process with no limits at all is one it has already placed, rather than
	// a registration written before limits were recorded, whose container
	// still names its member's cgroup as its parent and cannot start there.
	// A ceiling that limits nothing is still a ceiling: the directory exists,
	// it is delegated, and the container lives under it.
	Ceiling bool `json:"ceiling,omitempty"`
}

// Empty reports whether this ceiling limits nothing, which is what a
// registration written before limits were recorded carries, and what a Process
// healed by restore carries until it is run again.
func (l Limits) Empty() bool { return l.Memory == "" && l.CPU == "" && l.Pids <= 0 }

// Written reports whether kitbashd has placed this Process under a ceiling of
// its own. A row that carries limits was written by a release that always
// created the ceiling and always named it as the container's cgroup parent, so
// it counts as written without the flag; one that carries neither is the
// legacy registration restore heals, see spec/kitbashd-api.yaml.
func (l Limits) Written() bool { return !l.Empty() || l.Ceiling }

// JSON renders the limits for the column. A ceiling that is neither written
// nor recorded is an empty string rather than an object of nulls, so a legacy
// row and a Process with no limits read back the same.
func (l Limits) JSON() string {
	if !l.Written() {
		return ""
	}
	body, err := json.Marshal(l)
	if err != nil {
		return ""
	}
	return string(body)
}

// mountsJSON renders the mounts for the column. A Process that declared none
// is an empty string rather than an empty array, so a legacy row and a Process
// with no mounts read back the same.
func mountsJSON(list []mounts.Resolved) string {
	if len(list) == 0 {
		return ""
	}
	body, err := json.Marshal(list)
	if err != nil {
		return ""
	}
	return string(body)
}

// secretsJSON renders the declared names for the column. A Process that
// declared none is an empty string rather than an empty array, so a legacy row
// and a Process with no secrets read back the same.
func secretsJSON(names []string) string {
	if len(names) == 0 {
		return ""
	}
	body, err := json.Marshal(names)
	if err != nil {
		return ""
	}
	return string(body)
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

// NewFanoutSecret mints the bearer of one Process's fan out. Unlike a token it
// is stored as it is sent: the subscriber compares what kitbashd gave it at
// start with what arrives, and only kitbashd can produce it, so nothing else
// on the host can feed a subscriber records.
func NewFanoutSecret() (string, error) {
	var b [FanoutSecretBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: mint a fan out secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
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
// quota bounds how many Processes and how many jobs one member may hold; a
// zero field is no bound, which is what a caller not exercising the limit
// passes. A replacement is not a new Process and is never refused by either.
func (s *Store) RegisterProcess(ctx context.Context, p Process, tokenHash string, quota Quota) error {
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
	// The counts are inside the transaction, so two sessions registering at
	// the same moment cannot both read a count under the limit and both write.
	if !replacing && quota.Processes > 0 {
		var held int
		if err := tx.QueryRowContext(ctx,
			"SELECT count(*) FROM processes WHERE owner = ?", p.Owner).Scan(&held); err != nil {
			return fmt.Errorf("store: count the Processes of %s: %w", p.Owner, err)
		}
		if held >= quota.Processes {
			return ErrTooManyProcesses
		}
	}
	// The jobs are counted the same way and in the same transaction. A
	// registration that is not one is never refused by this, and one that
	// replaces a job of the same id is not a new job: the id is excluded
	// rather than the replacing flag read, because a Process that was not a
	// job and is one now is a new job under an id the table already has.
	if quota.Scheduled > 0 && p.Schedule.Declared() {
		var held int
		if err := tx.QueryRowContext(ctx,
			"SELECT count(*) FROM processes WHERE owner = ? AND id != ? AND schedule != ''",
			p.Owner, p.ID).Scan(&held); err != nil {
			return fmt.Errorf("store: count the scheduled Processes of %s: %w", p.Owner, err)
		}
		if held >= quota.Scheduled {
			return ErrTooManyScheduled
		}
	}

	subscriptions, err := json.Marshal(subscriptionList(p.Subscriptions))
	if err != nil {
		return fmt.Errorf("store: encode subscriptions: %w", err)
	}
	// The permits block is written as the manifest declared it, so what the
	// child of an MCP session is given is what the Process was registered
	// with and not what its Package says today.
	permits := p.Permits.JSON()
	// The ceiling is written with the rest of the record: after a reboot the
	// cgroup filesystem is empty, and this is the only thing that remembers
	// what the Process was limited to.
	limits := p.Limits.JSON()
	// The probe travels with the registration for the same reason the limits
	// do: after a reboot nothing else remembers what a Process declared, and
	// the prober starts from the registrations kitbashd holds.
	health := p.Health.JSON()
	// The mounts are written as kitbashd resolved them, not as the manifest
	// declared them: a start and a restore mount the resolved path, so nothing
	// between here and podman has to resolve anything again.
	mounted := mountsJSON(p.Mounts)
	// The secret names are written as the unit declared them. The values are
	// not here and never will be: they are root owned files outside this
	// database, so the nightly copy of it carries none, see PLAN.md 4.7.
	named := secretsJSON(p.Secrets)
	// The schedule travels with the registration for the reason the probe
	// does: after a reboot nothing else remembers that this Process is a job,
	// and the ticker is built from these rows.
	scheduled := p.Schedule.JSON()
	// The units travel with it for the same reason again: a pod and every
	// container in it are made again at the next boot, and this row is the
	// only thing that says which units the Process has, see PLAN.md 5.6.
	composed := p.Composition.JSON()
	// The fan out secret is replaced with the token, because the two are minted
	// together: a container holding the old token holds the old secret.
	if _, err := tx.ExecContext(ctx, `INSERT INTO processes
		(id, owner, admin, package, name, container, digest, expose, endpoint, hostname,
		 subscriptions, runner, permits, limits, health, mounts, secrets, schedule, units, token_hash,
		 fanout_secret, registered_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			owner = excluded.owner, admin = excluded.admin, package = excluded.package,
			name = excluded.name, container = excluded.container, digest = excluded.digest,
			expose = excluded.expose, endpoint = excluded.endpoint,
			hostname = excluded.hostname,
			subscriptions = excluded.subscriptions, runner = excluded.runner,
			permits = excluded.permits,
			limits = excluded.limits, health = excluded.health,
			mounts = excluded.mounts, secrets = excluded.secrets,
			schedule = excluded.schedule, units = excluded.units,
			token_hash = excluded.token_hash,
			fanout_secret = excluded.fanout_secret, registered_at = excluded.registered_at`,
		p.ID, p.Owner, p.Admin, p.Package, p.Name, p.Container, p.Digest, p.Expose, p.Endpoint, p.Hostname,
		string(subscriptions), p.Runner, string(permits), limits, health, mounted, named, scheduled,
		composed, tokenHash, p.FanoutSecret,
		p.RegisteredAt.UnixNano()); err != nil {
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

// DeleteProcessesByOwner removes every registration of one member and answers
// the ids it removed, so the caller stops the fan out for each of them. It is
// part of removing a member: their tokens are revoked with their account, see
// spec/kitbashd-api.yaml.
func (s *Store) DeleteProcessesByOwner(ctx context.Context, owner string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM processes WHERE owner = ?", owner)
	if err != nil {
		return nil, fmt.Errorf("store: list the Processes of %s: %w", owner, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: list the Processes of %s: %w", owner, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: list the Processes of %s: %w", owner, err)
	}
	rows.Close()
	if len(ids) == 0 {
		return nil, nil
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM processes WHERE owner = ?", owner); err != nil {
		return nil, fmt.Errorf("store: unregister the Processes of %s: %w", owner, err)
	}
	return ids, nil
}

// ProcessCounts is how many Processes each member holds registered, which is
// the running Process count users_list answers with.
func (s *Store) ProcessCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT owner, count(*) FROM processes GROUP BY owner")
	if err != nil {
		return nil, fmt.Errorf("store: count the Processes per member: %w", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var owner string
		var n int
		if err := rows.Scan(&owner, &n); err != nil {
			return nil, fmt.Errorf("store: count the Processes per member: %w", err)
		}
		counts[owner] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: count the Processes per member: %w", err)
	}
	return counts, nil
}

// UpdateProcessRun writes the ceiling and the token of a Process that is
// already registered, and reports whether there was one to write. It inserts
// nothing: a scheduled run mints its token after it read the registration, and
// the proc_stop that landed in between must stay deleted rather than be
// written back by the tick it raced, see internal/daemon/schedule.go.
//
// Everything else about the row is left as it is, so this is the one write
// that cannot resurrect a registration and cannot carry a stale field over
// another writer's.
func (s *Store) UpdateProcessRun(ctx context.Context, id string, limits Limits, tokenHash string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE processes SET limits = ?, token_hash = ? WHERE id = ?", limits.JSON(), tokenHash, id)
	if err != nil {
		return false, fmt.Errorf("store: record the run of %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: record the run of %s: %w", id, err)
	}
	return n > 0, nil
}

// RevokeProcessToken makes one token answer for nothing, without touching the
// registration. It is what the end of a scheduled run calls: the container is
// kept until the next tick so proc_logs can read it, and a container that has
// exited is not a producer, so the token it was given dies with the run.
//
// The hash is named rather than the id alone, so a run that ended after its
// Process was registered again revokes the token it was given and not the one
// the new registration minted. The column is emptied rather than deleted: no
// token hashes to the empty string, so an empty column is a registration
// nothing can export as.
func (s *Store) RevokeProcessToken(ctx context.Context, id, tokenHash string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE processes SET token_hash = '' WHERE id = ? AND token_hash = ?", id, tokenHash)
	if err != nil {
		return false, fmt.Errorf("store: revoke the token of %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: revoke the token of %s: %w", id, err)
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
	row := s.db.QueryRowContext(ctx, processColumns+" FROM processes WHERE "+where, arg)
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
	query := processColumns + " FROM processes"
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

// processColumns is the one select every read of this table shares.
const processColumns = `SELECT id, owner, admin, package, name, container, digest,
	expose, endpoint, hostname, subscriptions, runner, permits, limits, health, mounts, secrets, schedule,
	units, fanout_secret, registered_at`

// scanner is what both a single row and a row of a result set satisfy.
type scanner interface {
	Scan(dest ...any) error
}

func scanProcess(row scanner) (Process, error) {
	var p Process
	var subscriptions, permits, limits, health, mounted, named, scheduled, composed string
	var registered int64
	if err := row.Scan(&p.ID, &p.Owner, &p.Admin, &p.Package, &p.Name, &p.Container, &p.Digest,
		&p.Expose, &p.Endpoint, &p.Hostname, &subscriptions, &p.Runner, &permits, &limits, &health, &mounted,
		&named, &scheduled, &composed, &p.FanoutSecret,
		&registered); err != nil {
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
	// A record written before the column existed carries an empty string,
	// which is the empty block: that Process is permitted nothing until it is
	// registered again, which is the safe end of the two.
	if limits != "" {
		if err := json.Unmarshal([]byte(limits), &p.Limits); err != nil {
			return Process{}, fmt.Errorf("store: read the limits of %s: %w", p.ID, err)
		}
	}
	if health != "" {
		if err := json.Unmarshal([]byte(health), &p.Health); err != nil {
			return Process{}, fmt.Errorf("store: read the health probe of %s: %w", p.ID, err)
		}
	}
	if mounted != "" {
		if err := json.Unmarshal([]byte(mounted), &p.Mounts); err != nil {
			return Process{}, fmt.Errorf("store: read the mounts of %s: %w", p.ID, err)
		}
	}
	if named != "" {
		if err := json.Unmarshal([]byte(named), &p.Secrets); err != nil {
			return Process{}, fmt.Errorf("store: read the secrets of %s: %w", p.ID, err)
		}
	}
	if scheduled != "" {
		p.Schedule = readSchedule(p.ID, scheduled)
	}
	// An empty column is a Process of one unit, which is every registration
	// written before pods existed and every Package that declares one unit
	// today. It is read as what it is rather than as a pod of one.
	if composed != "" {
		if err := json.Unmarshal([]byte(composed), &p.Composition); err != nil {
			return Process{}, fmt.Errorf("store: read the units of %s: %w", p.ID, err)
		}
	}
	if permits != "" {
		block, err := manifest.ParsePermits([]byte(permits))
		if err != nil {
			return Process{}, fmt.Errorf("store: read the permits of %s: %w", p.ID, err)
		}
		p.Permits = block
	}
	return p, nil
}

// readSchedule reads the schedule column and answers the job it describes, or
// no job at all. A column this release cannot read is not a failed listing: a
// row written by something else, or by a release that spelled a schedule
// differently, would otherwise stop every Process of this host from being
// listed. It is said once per read and that Process is not a job until it is
// registered again, which is the end of the two that cannot surprise a member.
//
// The expression and the ceiling are held to the same rules a registration is,
// because this is where the ticker reads them: a row whose cron nothing can
// parse is a job that would never fire, and one whose ceiling the runtime
// would refuse is a job that would never start.
func readSchedule(id, column string) Schedule {
	var held Schedule
	if err := json.Unmarshal([]byte(column), &held); err != nil {
		logger.Printf("store: the schedule of %s is not readable, so it is not a job: %v", id, err)
		return Schedule{}
	}
	if _, err := manifest.ParseCron(held.Cron); err != nil {
		logger.Printf("store: the schedule of %s is not one this release reads, so it is not a job: %v", id, err)
		return Schedule{}
	}
	if held.Memory != "" && !podman.ValidMemory(podman.MemoryLimit(held.Memory)) {
		logger.Printf("store: the schedule of %s asks for memory %q, which is not a size the runtime takes",
			id, held.Memory)
		return Schedule{}
	}
	if held.CPU != "" && !podman.ValidCPUs(held.CPU) {
		logger.Printf("store: the schedule of %s asks for cpu %q, which is not a number of cores",
			id, held.CPU)
		return Schedule{}
	}
	return held
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
		if err := addTextColumn(db, table, "producer"); err != nil {
			return err
		}
		// kitbash.caller arrives with M6: the Process whose MCP session
		// recorded the record. Every record written before it carries none.
		if err := addTextColumn(db, table, "caller"); err != nil {
			return err
		}
		// kitbash.internal arrives with the admin readable causes: the record
		// is the cause of an internal problem. It is nullable like eval,
		// because a record that is not one carries no value at all rather
		// than a false.
		if err := addColumn(db, table, "internal", "INTEGER"); err != nil {
			return err
		}
		// kitbash.unit arrives with M12: the unit of a Process that runs as a
		// pod. Every record written before it carries none, which is what a
		// record about a Process of one unit carries today. The column is
		// kitbash_unit because the metrics table already has a unit, which is
		// the unit of measure of a data point.
		if err := addTextColumn(db, table, "kitbash_unit"); err != nil {
			return err
		}
	}
	// The container name and the image digest arrive with M5, the fan out
	// secret with the authenticated fan out. A registration written before any
	// of them carries none: restore skips a Process without a container, and
	// the fan out delivers to a Process without a secret with no bearer on the
	// request until it is registered again.
	// The permits block arrives with the narrowed /mcp surface. A
	// registration written before it carries none, so that Process reaches
	// nothing over /mcp until it is run again, which is the end of the two
	// that cannot surprise a member.
	// The limits arrive with the Process cgroup: kitbashd writes the ceiling
	// into it, and after a reboot the registration is the only thing that
	// remembers what the ceiling was. A registration written before this
	// restores without one until the Process is run again.
	// The runner arrives with build and run kit dispatch: the Package path of
	// the kit that owns the Process, and the thing that tells restore there is
	// no container of it here to start. Every registration written before it
	// carries none, which is a Process kitbashd runs itself.
	// The health probe arrives with the probe loop: the path a Process
	// declares and how often it is requested. A registration written before it
	// declares none, so that Process is not probed until it is run again.
	// The mounts arrive with M8: the folders of Files the Process sees, as
	// kitbashd resolved them. A registration written before them carries none,
	// which is a Process that sees no Files, the way every Process did.
	// The secret names arrive with M9: the environment variables the Process
	// is given the member's values under. A registration written before them
	// carries none, which is a Process given nothing beyond what kitbashd
	// speaks for, the way every Process was.
	// The host name arrives with the reverse proxy: the one name a unit
	// declared for itself, which kitbashd serves in place of the name it
	// derives. A registration written before it carries none, which is a
	// Process served under the derived name and nothing else.
	// The schedule arrives with the built in scheduler: the cron a unit
	// declared and what its ticks start the container with. A registration
	// written before it carries none, which is a Process that stays up and
	// that nothing starts on time, the way every Process was.
	// The units arrive with M12: the pod a Package of more than one unit runs
	// as, and every container in it. A registration written before them
	// carries none, which is the single unit Process it was, the way every
	// Process was.
	for _, column := range []string{
		"container", "digest", "fanout_secret", "permits", "limits", "runner", "health", "mounts", "secrets",
		"hostname", "schedule", "units",
	} {
		if err := addTextColumn(db, "processes", column); err != nil {
			return err
		}
	}
	return nil
}

// addTextColumn adds one text column with an empty default unless the table
// already has it. A table that does not exist yet has no columns, which is the
// answer a fresh store gives before the schema runs.
func addTextColumn(db *sql.DB, table, column string) error {
	return addColumn(db, table, column, "TEXT NOT NULL DEFAULT ''")
}

// addColumn adds one column with the declaration given unless the table
// already has it.
func addColumn(db *sql.DB, table, column, declaration string) error {
	has, err := hasColumn(db, table, column)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, declaration)); err != nil {
		return fmt.Errorf("store: add the %s column to %s: %w", column, table, err)
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
