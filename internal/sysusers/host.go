package sysusers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Where the host keeps what this package edits. They are fields on Host rather
// than constants so a test can point them somewhere writable, and defaults so
// a kitbash host needs no configuration.
const (
	DefaultPasswdFile = "/etc/passwd"
	DefaultGroupFile  = "/etc/group"
	DefaultSubUIDFile = "/etc/subuid"
	DefaultSubGIDFile = "/etc/subgid"
	DefaultRunUser    = "/run/user"
	DefaultArchive    = "/org/.archive"
)

// Modes of what a member owns. The home and the runtime directory are private
// to the member; the archive is root's alone, because a removed member's home
// must not be readable through /org, see PLAN.md section 4.5.
const (
	HomeMode    = 0o700
	SSHDirMode  = 0o700
	KeysMode    = 0o600
	RunUserMode = 0o700
	ArchiveMode = 0o700
)

// CommandTimeout bounds one shadow-utils call, and RemoveTimeout the container
// removal of a member being deleted. Neither is a fast path and both are far
// above what the tools take on a working host.
const (
	CommandTimeout = 30 * time.Second
	RemoveTimeout  = 60 * time.Second
)

// Host is the real System: it runs the host's own tools, useradd, usermod,
// userdel and pkill, and writes the files they do not. It is not exercised by
// the unit tests, which run on a machine without those tools and without root;
// what is testable here is validated separately, see keys.go and subids.go.
type Host struct {
	// Passwd and Group are read to answer List.
	Passwd string
	Group  string
	// SubUID and SubGID hold the subordinate id blocks rootless podman needs.
	SubUID string
	SubGID string
	// RunUser is where a member's XDG runtime directory goes.
	RunUser string
	// Archive is where a removed member's home is moved to.
	Archive string
	// Runner removes a member's containers as that member.
	Runner Runner
}

// NewHost returns the System kitbashd uses on a kitbash host.
func NewHost(runner Runner) *Host {
	return &Host{
		Passwd:  DefaultPasswdFile,
		Group:   DefaultGroupFile,
		SubUID:  DefaultSubUIDFile,
		SubGID:  DefaultSubGIDFile,
		RunUser: DefaultRunUser,
		Archive: DefaultArchive,
		Runner:  runner,
	}
}

// Create makes one member. The key is validated first, so a request carrying a
// line that is not a key never creates an account at all.
func (h *Host) Create(ctx context.Context, spec Spec) (Member, error) {
	if !ValidName(spec.Name) {
		return Member{}, fmt.Errorf("%w: %q", ErrName, spec.Name)
	}
	key, err := ValidateKey(spec.SSHKey)
	if err != nil {
		return Member{}, err
	}
	if _, err := user.Lookup(spec.Name); err == nil {
		return Member{}, fmt.Errorf("%w: %s", ErrExists, spec.Name)
	}

	if err := h.run(ctx, "useradd", "-m", "-s", "/bin/sh", "-G", UsersGroup, spec.Name); err != nil {
		return Member{}, err
	}
	// useradd leaves the password field as "!", which sshd reads as a locked
	// account and refuses even a key login. "*" means no password at all.
	if err := h.run(ctx, "usermod", "-p", "*", spec.Name); err != nil {
		return Member{}, err
	}
	if spec.Admin {
		if err := h.run(ctx, "usermod", "-aG", AdminGroup, spec.Name); err != nil {
			return Member{}, err
		}
	}

	m, found, err := h.Lookup(ctx, spec.Name)
	if err != nil {
		return Member{}, err
	}
	if !found {
		return Member{}, fmt.Errorf("sysusers: %s was created but is not in %s", spec.Name, h.Passwd)
	}
	if err := h.ensureSubIDs(m.Name); err != nil {
		return Member{}, err
	}
	if err := h.ensureHome(m); err != nil {
		return Member{}, err
	}
	if _, err := h.writeKey(m, key); err != nil {
		return Member{}, err
	}
	if err := ensureRuntimeDir(h.RunUser, m); err != nil {
		return Member{}, err
	}
	return h.reread(ctx, m.Name)
}

// AddKey appends one key to a member's authorized_keys and answers the member
// with the new key count. A key already in the file is not written twice.
func (h *Host) AddKey(ctx context.Context, name, key string) (Member, error) {
	line, err := ValidateKey(key)
	if err != nil {
		return Member{}, err
	}
	m, found, err := h.Lookup(ctx, name)
	if err != nil {
		return Member{}, err
	}
	if !found {
		return Member{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err := h.ensureHome(m); err != nil {
		return Member{}, err
	}
	if _, err := h.writeKey(m, line); err != nil {
		return Member{}, err
	}
	return h.reread(ctx, name)
}

// Remove takes a member away: their containers, their sessions, their home and
// their account. It answers where the home was archived.
//
// The container removal and the session kill are best effort. A runtime that
// will not answer, or a member with nothing running, must not leave an admin
// with an account they cannot delete.
func (h *Host) Remove(ctx context.Context, name string) (string, error) {
	m, found, err := h.Lookup(ctx, name)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: %s", ErrNotFound, name)
	}

	if h.Runner != nil {
		removeCtx, cancel := context.WithTimeout(ctx, RemoveTimeout)
		err := h.Runner.RemoveAll(removeCtx, m)
		cancel()
		if err != nil {
			logger.Printf("removing the containers of %s: %v", name, err)
		}
	}
	// pkill answers 1 when it matched nothing, which is the common case for a
	// member with no session open.
	if err := h.run(ctx, "pkill", "-u", name); err != nil && !exitCode(err, 1) {
		logger.Printf("ending the sessions of %s: %v", name, err)
	}

	archived, err := h.archiveHome(m)
	if err != nil {
		return "", err
	}
	// userdel without -r: the home is already moved and must not be deleted.
	if err := h.run(ctx, "userdel", name); err != nil {
		return "", err
	}
	if _, err := removeSubID(h.SubUID, name); err != nil {
		return "", err
	}
	if _, err := removeSubID(h.SubGID, name); err != nil {
		return "", err
	}
	if m.UID > 0 {
		os.RemoveAll(filepath.Join(h.RunUser, strconv.Itoa(m.UID)))
	}
	return archived, nil
}

// List answers every member of kitbash-users, by name.
func (h *Host) List(ctx context.Context) ([]Member, error) {
	names, err := h.groupMembers()
	if err != nil {
		return nil, err
	}
	out := make([]Member, 0, len(names))
	for _, name := range names {
		m, found, err := h.Lookup(ctx, name)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// Lookup answers one member from the host's own user database.
func (h *Host) Lookup(_ context.Context, name string) (Member, bool, error) {
	u, err := user.Lookup(name)
	if err != nil {
		var unknown user.UnknownUserError
		if os.IsNotExist(err) || errors.As(err, &unknown) {
			return Member{}, false, nil
		}
		return Member{}, false, fmt.Errorf("sysusers: look up %s: %w", name, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return Member{}, false, fmt.Errorf("sysusers: %s has an unreadable uid %q", name, u.Uid)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return Member{}, false, fmt.Errorf("sysusers: %s has an unreadable gid %q", name, u.Gid)
	}
	m := Member{Name: u.Username, UID: uid, GID: gid, Home: u.HomeDir, Groups: []string{}}

	ids, err := u.GroupIds()
	if err != nil {
		return Member{}, false, fmt.Errorf("sysusers: read the groups of %s: %w", name, err)
	}
	for _, id := range ids {
		g, err := user.LookupGroupId(id)
		if err != nil {
			continue
		}
		m.Groups = append(m.Groups, g.Name)
		if g.Name == AdminGroup {
			m.Admin = true
		}
	}
	m.Keys, err = h.countKeys(m)
	if err != nil {
		return Member{}, false, err
	}
	return m, true, nil
}

// reread answers the member as the host now holds them, which is what a Create
// or an AddKey returns.
func (h *Host) reread(ctx context.Context, name string) (Member, error) {
	m, found, err := h.Lookup(ctx, name)
	if err != nil {
		return Member{}, err
	}
	if !found {
		return Member{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return m, nil
}

// ensureSubIDs gives the member one 65536 wide block in each of the two files,
// the next one free above SubIDBase. A member who already has a block keeps it.
func (h *Host) ensureSubIDs(name string) error {
	for _, path := range []string{h.SubUID, h.SubGID} {
		has, err := hasSubID(path, name)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		start, err := nextSubID(path)
		if err != nil {
			return err
		}
		if err := appendSubID(path, name, start, SubIDCount); err != nil {
			return err
		}
	}
	return nil
}

// ensureHome narrows the home to the member alone and makes sure the .ssh
// directory and the authorized_keys file exist with the modes sshd requires.
func (h *Host) ensureHome(m Member) error {
	if m.Home == "" {
		return fmt.Errorf("sysusers: %s has no home directory", m.Name)
	}
	if err := os.MkdirAll(m.Home, HomeMode); err != nil {
		return fmt.Errorf("sysusers: create the home of %s: %w", m.Name, err)
	}
	if err := os.Chmod(m.Home, HomeMode); err != nil {
		return fmt.Errorf("sysusers: narrow the home of %s: %w", m.Name, err)
	}
	if err := os.Chown(m.Home, m.UID, m.GID); err != nil {
		return fmt.Errorf("sysusers: give the home of %s to them: %w", m.Name, err)
	}

	dir := filepath.Join(m.Home, ".ssh")
	if err := os.MkdirAll(dir, SSHDirMode); err != nil {
		return fmt.Errorf("sysusers: create the .ssh of %s: %w", m.Name, err)
	}
	if err := os.Chmod(dir, SSHDirMode); err != nil {
		return fmt.Errorf("sysusers: narrow the .ssh of %s: %w", m.Name, err)
	}
	if err := os.Chown(dir, m.UID, m.GID); err != nil {
		return fmt.Errorf("sysusers: give the .ssh of %s to them: %w", m.Name, err)
	}

	keys := filepath.Join(dir, "authorized_keys")
	f, err := os.OpenFile(keys, os.O_CREATE|os.O_WRONLY, KeysMode)
	if err != nil {
		return fmt.Errorf("sysusers: create the authorized_keys of %s: %w", m.Name, err)
	}
	f.Close()
	if err := os.Chmod(keys, KeysMode); err != nil {
		return fmt.Errorf("sysusers: narrow the authorized_keys of %s: %w", m.Name, err)
	}
	if err := os.Chown(keys, m.UID, m.GID); err != nil {
		return fmt.Errorf("sysusers: give the authorized_keys of %s to them: %w", m.Name, err)
	}
	return nil
}

// writeKey appends one validated key line unless it is already there, and
// answers how many keys the file then holds.
func (h *Host) writeKey(m Member, line string) (int, error) {
	path := filepath.Join(m.Home, ".ssh", "authorized_keys")
	content, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("sysusers: read the authorized_keys of %s: %w", m.Name, err)
	}
	lines := keyLines(string(content))
	for _, existing := range lines {
		if existing == line {
			return len(lines), nil
		}
	}
	lines = append(lines, line)

	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), KeysMode); err != nil {
		return 0, fmt.Errorf("sysusers: write the authorized_keys of %s: %w", m.Name, err)
	}
	if err := os.Chmod(path, KeysMode); err != nil {
		return 0, fmt.Errorf("sysusers: narrow the authorized_keys of %s: %w", m.Name, err)
	}
	if err := os.Chown(path, m.UID, m.GID); err != nil {
		return 0, fmt.Errorf("sysusers: give the authorized_keys of %s to them: %w", m.Name, err)
	}
	return len(lines), nil
}

// countKeys is how many public keys a member has.
func (h *Host) countKeys(m Member) (int, error) {
	if m.Home == "" {
		return 0, nil
	}
	content, err := os.ReadFile(filepath.Join(m.Home, ".ssh", "authorized_keys"))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		// A home kitbashd may not read is not a reason to fail a listing.
		logger.Printf("reading the authorized_keys of %s: %v", m.Name, err)
		return 0, nil
	}
	return len(keyLines(string(content))), nil
}

// archiveHome moves a removed member's home under /org/.archive, where the
// directory is root's alone and the MCP surface cannot see it: /org/.archive
// carries no kitbash.yaml, so it does not exist as far as Files is concerned,
// see PLAN.md section 2.1.
func (h *Host) archiveHome(m Member) (string, error) {
	if err := os.MkdirAll(h.Archive, ArchiveMode); err != nil {
		return "", fmt.Errorf("sysusers: create %s: %w", h.Archive, err)
	}
	if err := os.Chmod(h.Archive, ArchiveMode); err != nil {
		return "", fmt.Errorf("sysusers: narrow %s: %w", h.Archive, err)
	}
	if err := os.Chown(h.Archive, 0, 0); err != nil {
		return "", fmt.Errorf("sysusers: give %s to root: %w", h.Archive, err)
	}

	target := filepath.Join(h.Archive, m.Name)
	if m.Home == "" {
		return target, nil
	}
	if _, err := os.Stat(m.Home); os.IsNotExist(err) {
		return target, nil
	}
	// A second member of the same name removed twice would otherwise land on
	// the first archive. The suffix keeps both.
	if _, err := os.Stat(target); err == nil {
		target = fmt.Sprintf("%s.%d", target, time.Now().UTC().Unix())
	}
	if err := os.Rename(m.Home, target); err != nil {
		return "", fmt.Errorf("sysusers: archive the home of %s: %w", m.Name, err)
	}
	if err := chownTree(target, 0, 0); err != nil {
		return "", err
	}
	if err := os.Chmod(target, ArchiveMode); err != nil {
		return "", fmt.Errorf("sysusers: narrow %s: %w", target, err)
	}
	return target, nil
}

// groupMembers reads the names in kitbash-users, including the members whose
// primary group it is. os/user has no way to list a group, so the file is read
// the way every other tool on the host reads it.
func (h *Host) groupMembers() ([]string, error) {
	content, err := os.ReadFile(h.Group)
	if err != nil {
		return nil, fmt.Errorf("sysusers: read %s: %w", h.Group, err)
	}
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}

	gid := ""
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 4 || fields[0] != UsersGroup {
			continue
		}
		gid = fields[2]
		for _, name := range strings.Split(fields[3], ",") {
			add(name)
		}
	}
	if gid == "" {
		return names, nil
	}

	passwd, err := os.ReadFile(h.Passwd)
	if err != nil {
		return nil, fmt.Errorf("sysusers: read %s: %w", h.Passwd, err)
	}
	for _, line := range strings.Split(string(passwd), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 4 {
			continue
		}
		if fields[3] == gid {
			add(fields[0])
		}
	}
	return names, nil
}

// run executes one host tool with a timeout. The error carries the tool's
// standard error, which is for the server log: it names host paths and shadow
// utilities, and an agent can do nothing with either.
func (h *Host) run(ctx context.Context, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, CommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("sysusers: %s: %w", name, err)
		}
		return fmt.Errorf("sysusers: %s: %w: %s", name, err, msg)
	}
	return nil
}

// exitCode reports whether a command failed with one particular status, which
// is how pkill says it matched nothing.
func exitCode(err error, code int) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	return exit.ExitCode() == code
}

// chownTree gives a whole directory to one owner.
func chownTree(root string, uid, gid int) error {
	return filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if err := os.Lchown(path, uid, gid); err != nil {
			return fmt.Errorf("sysusers: give %s to %d:%d: %w", path, uid, gid, err)
		}
		return nil
	})
}

// ensureRuntimeDir creates /run/user/<uid> for one member. rootless podman
// needs it and nothing else creates it between boots, see PLAN.md section 2.3.
func ensureRuntimeDir(root string, m Member) error {
	if root == "" || m.UID <= 0 {
		return nil
	}
	dir := filepath.Join(root, strconv.Itoa(m.UID))
	if err := os.MkdirAll(dir, RunUserMode); err != nil {
		return fmt.Errorf("sysusers: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, RunUserMode); err != nil {
		return fmt.Errorf("sysusers: narrow %s: %w", dir, err)
	}
	if err := os.Chown(dir, m.UID, m.GID); err != nil {
		return fmt.Errorf("sysusers: give %s to %s: %w", dir, m.Name, err)
	}
	return nil
}
