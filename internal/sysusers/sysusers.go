// Package sysusers is the system side of the users family: creating a Linux
// user, giving it an SSH key, listing the members and taking one away again.
// Creating a member is root's work, which is why kitbashd serves it and
// kitbash-mcp only forwards, see PLAN.md section 4.5.
//
// Everything that shells out to the host lives behind System and Runner, so
// the daemon is tested against a fake on a machine that has neither useradd
// nor podman. The real implementations are Host and Podman.
package sysusers

import (
	"context"
	"errors"
	"os/exec"
	"regexp"

	"github.com/zyx1121/kitbash/internal/podman"
)

// Groups every member and every admin belongs to, see PLAN.md section 4.5.
const (
	UsersGroup = "kitbash-users"
	AdminGroup = "kitbash-admin"
)

// Name is the member name spec/mcp-surface.yaml declares. It is also a Linux
// user name, so the pattern is the narrow one both can carry.
var Name = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// The failures a caller distinguishes. Anything else is a host failure the
// daemon reports as internal and writes to the server log.
var (
	// ErrExists reports a member name already taken on this host.
	ErrExists = errors.New("sysusers: the member already exists")
	// ErrNotFound reports a name no member on this host carries.
	ErrNotFound = errors.New("sysusers: no such member")
	// ErrName reports a name outside the pattern spec/mcp-surface.yaml sets.
	ErrName = errors.New("sysusers: not a member name")
	// ErrKey reports an SSH public key line kitbash will not write.
	ErrKey = errors.New("sysusers: not an OpenSSH public key line")
	// ErrNoContainer reports a container the runtime does not have, which is
	// what restore unregisters rather than retries.
	ErrNoContainer = errors.New("sysusers: no such container")
	// ErrNoImage reports an image the member's store does not have, which is
	// a Process whose Package was never built here or whose build is gone.
	ErrNoImage = errors.New("sysusers: no such image")
	// ErrUsage reports a run the container runtime refused to parse, which is
	// exit 125: the options of the unit are wrong, and the member is the one
	// who can change them.
	ErrUsage = errors.New("sysusers: the container runtime refused the options")
	// ErrTimeout reports a runtime that was still working when its budget ran
	// out. It is not a failure of the Package: stopping a container that
	// ignores SIGTERM takes the whole grace, and a host under load takes
	// longer still.
	ErrTimeout = errors.New("sysusers: the container runtime did not answer in time")
	// ErrHomeShape reports a home that is not the shape kitbashd writes into:
	// a .ssh that is a link or belongs to somebody else, an authorized_keys
	// that is not a regular file the member owns. kitbashd is root and a
	// member owns every name under their home, so this is refused rather than
	// repaired.
	ErrHomeShape = errors.New("sysusers: the home is not the shape kitbash writes into")
)

// Member is one organization member. Keys is how many public keys their
// authorized_keys holds, and Groups the group names they belong to. The
// running Process count of users_list is the store's answer, not this
// package's, so it is not a field here.
type Member struct {
	Name   string
	UID    int
	GID    int
	Admin  bool
	Home   string
	Keys   int
	Groups []string
}

// InGroup reports whether the member belongs to a group by name.
func (m Member) InGroup(name string) bool {
	for _, g := range m.Groups {
		if g == name {
			return true
		}
	}
	return false
}

// IsMember reports whether this account is an organization member rather than
// some other account on the host. Only a member of kitbash-users is one, and
// root is never one however it is grouped: the users family writes SSH keys
// and deletes accounts, so the accounts it may touch are named exactly.
func (m Member) IsMember() bool {
	return m.UID != 0 && m.InGroup(UsersGroup)
}

// Spec is one member to create.
type Spec struct {
	Name   string
	SSHKey string
	Admin  bool
}

// System is what the users family does to the host. Every method is safe to
// call again with the same arguments: Create refuses a name that exists rather
// than half building a second one, AddKey does not write a key twice, and
// Remove reports ErrNotFound once the member is gone.
type System interface {
	// Create makes the member, their home, their subordinate id range and
	// their first key. ErrExists when the name is taken.
	Create(ctx context.Context, spec Spec) (Member, error)
	// AddKey appends one public key line and answers how many the member now
	// has. A key already present is not added twice.
	AddKey(ctx context.Context, name, key string) (Member, error)
	// Remove stops the member's Processes, ends their sessions, archives
	// their home and deletes the account. It answers where the home went.
	Remove(ctx context.Context, name string) (archived string, err error)
	// List answers every member of kitbash-users.
	List(ctx context.Context) ([]Member, error)
	// Lookup answers one member by name, found false when there is none.
	Lookup(ctx context.Context, name string) (Member, bool, error)
}

// Runner runs the container runtime as a member. It is the part of kitbashd
// that changes uid: restore, removing a member, and every Process start,
// because a member cannot place their own container in a delegated cgroup and
// kitbashd can, see PLAN.md section 2.3.
//
// cgroup is the leaf directory the child is started in, which internal/cgroups
// answers per member. An empty one runs the child where the daemon is, which
// is a host that records limits rather than enforcing them.
type Runner interface {
	// Run starts one container as the member and returns its runtime id. The
	// options carry the whole command line, env file included; nothing here
	// reads a manifest. An image the member does not have is ErrNoImage, and
	// a command the runtime refused to parse is ErrUsage.
	Run(ctx context.Context, m Member, opts podman.RunOptions, cgroup string) (string, error)
	// Start creates the member's runtime directory and starts one container
	// as them. A container the runtime does not have is ErrNoContainer, which
	// restore answers by unregistering the Process. A container keeps the
	// cgroup parent it was created with, so restoring one only has to place
	// the child again.
	Start(ctx context.Context, m Member, container, cgroup string) error
	// Stop stops one container as the member, giving it timeout seconds to
	// exit on its own. A container the runtime does not have is
	// ErrNoContainer.
	Stop(ctx context.Context, m Member, container string, timeout int) error
	// RemoveContainer removes one container as the member. A container the
	// runtime does not have is ErrNoContainer. The name says container
	// because Remove on the System of this package is a member.
	RemoveContainer(ctx context.Context, m Member, container string, force bool) error
	// RemoveAll force removes every kitbash container of one member. It is
	// best effort: a runtime that will not answer must not stop an admin from
	// deleting the account.
	RemoveAll(ctx context.Context, m Member) error
}

// Sessions starts one kitbash-mcp as a member, which is how a Process reaches
// the MCP surface as its owner, see mcp_for_processes in
// spec/kitbashd-api.yaml. It is a second interface rather than a method on
// Runner because it runs a kitbash binary and not the container runtime.
type Sessions interface {
	// MCPCommand builds the command that runs binary as the member with the
	// caller credential of one session. The command is not started: the
	// caller owns its pipes, which are the MCP session.
	MCPCommand(ctx context.Context, m Member, binary, credential string) (*exec.Cmd, error)
}

// EnvCaller is the variable that tells a kitbash-mcp which session it serves.
// It carries a credential kitbashd minted for that session, not the Process
// id: the child records it on every span and kitbashd resolves it back, so no
// producer can name a Process it does not run as, see mcp_for_processes in
// spec/kitbashd-api.yaml.
//
// It is read by internal/telemetry, whose constant of the same value is the
// reader's spelling; this package cannot import that one without pulling the
// OpenTelemetry SDK into kitbashd.
const EnvCaller = "KITBASH_CALLER"

// ValidName reports whether a name is one kitbash will create.
func ValidName(name string) bool { return Name.MatchString(name) }
