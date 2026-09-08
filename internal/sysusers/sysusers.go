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
	"regexp"
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

// Runner runs the container runtime as a member. It is the one part of
// restore and of removing a member that has to change uid, see
// spec/kitbashd-api.yaml.
type Runner interface {
	// Start creates the member's runtime directory and starts one container
	// as them. A container the runtime does not have is ErrNoContainer, which
	// restore answers by unregistering the Process.
	Start(ctx context.Context, m Member, container string) error
	// RemoveAll force removes every kitbash container of one member. It is
	// best effort: a runtime that will not answer must not stop an admin from
	// deleting the account.
	RemoveAll(ctx context.Context, m Member) error
}

// ValidName reports whether a name is one kitbash will create.
func ValidName(name string) bool { return Name.MatchString(name) }
