package sysusers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	DefaultProc       = "/proc"
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

// ExitWait is how long a member's processes get to end after their session is
// killed, before the account is deleted anyway. userdel refuses while a
// process of that user is still running on some hosts, and a member with a
// wedged container should still be removable.
const (
	ExitWait = 10 * time.Second
	ExitPoll = 200 * time.Millisecond
)

// Host is the real System: it runs the host's own tools, useradd, usermod,
// userdel and pkill, and writes the files they do not.
//
// Everything below a member's home is written through an os.Root opened on
// that home and checked on the file descriptor, never on the path. kitbashd is
// root and the member owns every name under their home, so a name resolved
// twice is a name that can change in between, see keys.
type Host struct {
	// Passwd and Group are read to answer List.
	Passwd string
	Group  string
	// SubUID and SubGID hold the subordinate id blocks rootless podman needs,
	// and SubIDLock is the file the allocation locks. That file is kitbash's
	// own and is never shadow's /etc/subuid.lock, see issue #83.
	SubUID    string
	SubGID    string
	SubIDLock string
	// RunUser is where a member's XDG runtime directory goes.
	RunUser string
	// Archive is where a removed member's home is moved to.
	Archive string
	// Proc is the process filesystem, read to see whether a member still has
	// anything running.
	Proc string
	// Runner removes a member's containers as that member.
	Runner Runner

	// subIDs serialises this daemon's own allocations; SubIDLock serialises
	// them against kitbash-adduser on the console.
	subIDs sync.Mutex
}

// NewHost returns the System kitbashd uses on a kitbash host.
func NewHost(runner Runner) *Host {
	return &Host{
		Passwd:    DefaultPasswdFile,
		Group:     DefaultGroupFile,
		SubUID:    DefaultSubUIDFile,
		SubGID:    DefaultSubGIDFile,
		SubIDLock: DefaultSubIDLock,
		RunUser:   DefaultRunUser,
		Archive:   DefaultArchive,
		Proc:      DefaultProc,
		Runner:    runner,
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
//
// The target has to be a member of kitbash-users. Writing a key into the
// authorized_keys of root, of sshd or of any other account on the host would
// be handing out that account, not adding a key to a member.
func (h *Host) AddKey(ctx context.Context, name, key string) (Member, error) {
	line, err := ValidateKey(key)
	if err != nil {
		return Member{}, err
	}
	m, err := h.member(ctx, name)
	if err != nil {
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
// The order matters. The containers stop first, then the sessions end and the
// account goes, and only a member whose account is gone has their home moved:
// a home archived before a failed userdel would leave an account whose home
// does not exist. The container removal and the kill are best effort, because
// a runtime that will not answer must not leave an admin with an account they
// cannot delete.
func (h *Host) Remove(ctx context.Context, name string) (string, error) {
	m, err := h.member(ctx, name)
	if err != nil {
		return "", err
	}

	if h.Runner != nil {
		removeCtx, cancel := context.WithTimeout(ctx, RemoveTimeout)
		err := h.Runner.RemoveAll(removeCtx, m)
		cancel()
		if err != nil {
			logger.Printf("removing the containers of %s: %v", name, err)
		}
	}
	h.endSessions(ctx, m)

	// userdel without -r: the home is moved rather than deleted, and it is
	// moved only once the account it belonged to is gone.
	if err := h.run(ctx, "userdel", name); err != nil {
		return "", err
	}
	archived, err := h.archiveHome(m)
	if err != nil {
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

// endSessions kills what the member is running and waits for it to end. A
// process still holding the account open is logged and the removal goes on:
// the account is what an admin asked to be gone.
func (h *Host) endSessions(ctx context.Context, m Member) {
	// pkill answers 1 when it matched nothing, which is the common case for a
	// member with no session open.
	if err := h.run(ctx, "pkill", "-u", m.Name); err != nil && !exitCode(err, 1) {
		logger.Printf("ending the sessions of %s: %v", m.Name, err)
	}
	if h.waitForExit(m, ExitWait) {
		return
	}
	logger.Printf("%s still has processes after %s, killing them", m.Name, ExitWait)
	if err := h.run(ctx, "pkill", "-KILL", "-u", m.Name); err != nil && !exitCode(err, 1) {
		logger.Printf("killing the processes of %s: %v", m.Name, err)
	}
	if !h.waitForExit(m, ExitWait) {
		logger.Printf("%s still has processes; deleting the account anyway", m.Name)
	}
}

// waitForExit polls until the member has nothing running or the wait is over,
// and reports whether they are gone.
func (h *Host) waitForExit(m Member, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		running, err := h.running(m)
		if err != nil {
			logger.Printf("reading the processes of %s: %v", m.Name, err)
			return true
		}
		if running == 0 {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(ExitPoll)
	}
}

// running is how many processes the member has, read from the process
// filesystem rather than from a tool: pgrep is not on every host and its exit
// status says less than the count does.
func (h *Host) running(m Member) (int, error) {
	proc := h.Proc
	if proc == "" {
		proc = DefaultProc
	}
	entries, err := os.ReadDir(proc)
	if err != nil {
		return 0, fmt.Errorf("sysusers: read %s: %w", proc, err)
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		info, err := os.Stat(filepath.Join(proc, entry.Name()))
		if err != nil {
			// The process ended while this loop ran, which is the answer
			// this function is looking for anyway.
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if ok && int(st.Uid) == m.UID {
			count++
		}
	}
	return count, nil
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

// Lookup answers one account from the host's own user database. It answers
// about any account, not only a member: users_me is asked about the peer, and
// the peer of a socket is whoever opened it.
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

// member is Lookup for the two calls that write: the account has to exist, has
// to be a member of kitbash-users, and may never be root. An account that is
// not a member is not found as far as this family is concerned, which is also
// what stops the family from being a way to reach the rest of the host.
func (h *Host) member(ctx context.Context, name string) (Member, error) {
	if !ValidName(name) {
		return Member{}, fmt.Errorf("%w: %q", ErrName, name)
	}
	m, found, err := h.Lookup(ctx, name)
	if err != nil {
		return Member{}, err
	}
	if !found || !m.IsMember() || m.UID == 0 {
		return Member{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return m, nil
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
//
// The whole allocation is under one lock: reading the highest block and
// appending the next are two steps, and two of them interleaved would hand two
// members the same range, which is two members sharing the ownership of each
// other's container files.
func (h *Host) ensureSubIDs(name string) error {
	h.subIDs.Lock()
	defer h.subIDs.Unlock()

	unlock, err := lockSubIDs(h.SubIDLock)
	if err != nil {
		return err
	}
	defer unlock()

	for _, path := range []string{h.SubUID, h.SubGID} {
		if err := allocate(path, name); err != nil {
			return err
		}
	}
	// The last word before the lock is dropped. kitbash-adduser on a host
	// without flock takes a different kind of lock, and the two do not exclude
	// each other, so the files are read once more: a member without a block is
	// a member whose rootless podman will not start, and saying so here is
	// better than finding out at proc_run.
	for _, path := range []string{h.SubUID, h.SubGID} {
		has, err := hasSubID(path, name)
		if err != nil {
			return err
		}
		if !has {
			return fmt.Errorf("sysusers: the subordinate id block for %s in %s was lost to another writer",
				name, path)
		}
	}
	return nil
}

// keys opens the member's authorized_keys and hands back a file descriptor.
//
// Every step is taken through an os.Root opened on the home, so no name under
// it can resolve outside the home however the member arranges their symlinks,
// and every check is made on the descriptor rather than on the path: kitbashd
// is root here, and a name it resolves twice is a name the member can change
// in between. A .ssh that is not a directory the member owns, or an
// authorized_keys that is not a regular file the member owns with one link, is
// refused rather than written.
func (h *Host) keys(m Member, create bool) (*os.File, error) {
	if m.Home == "" {
		return nil, fmt.Errorf("sysusers: %s has no home directory", m.Name)
	}
	// Reading is a read: users_list and users_me count a member's keys, and
	// counting must not narrow a home or create a .ssh that was not there.
	if err := h.checkHome(m, create); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(m.Home)
	if err != nil {
		return nil, fmt.Errorf("sysusers: open the home of %s: %w", m.Name, err)
	}
	defer root.Close()

	dir, err := h.sshDir(root, m, create)
	if err != nil {
		return nil, err
	}
	if dir != nil {
		dir.Close()
	}

	const name = ".ssh/authorized_keys"
	if create {
		f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, KeysMode)
		switch {
		case err == nil:
			// Created by kitbashd, so it belongs to nobody else yet.
			if err := f.Chmod(KeysMode); err != nil {
				f.Close()
				return nil, fmt.Errorf("sysusers: narrow the authorized_keys of %s: %w", m.Name, err)
			}
			if err := f.Chown(m.UID, m.GID); err != nil {
				f.Close()
				return nil, fmt.Errorf("sysusers: give the authorized_keys of %s to them: %w", m.Name, err)
			}
			return f, nil
		case !os.IsExist(err):
			return nil, fmt.Errorf("sysusers: create the authorized_keys of %s: %w", m.Name, err)
		}
	}

	f, err := root.OpenFile(name, os.O_RDWR, KeysMode)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, err
		}
		return nil, fmt.Errorf("sysusers: open the authorized_keys of %s: %w", m.Name, err)
	}
	if err := ownedFile(f, m); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Chmod(KeysMode); err != nil {
		f.Close()
		return nil, fmt.Errorf("sysusers: narrow the authorized_keys of %s: %w", m.Name, err)
	}
	return f, nil
}

// sshDir makes sure ~/.ssh is a directory the member owns, at 0700, and hands
// back a descriptor on it. A .ssh that is a symlink, or that belongs to
// somebody else, is refused: sshd reads what is in it as authority.
func (h *Host) sshDir(root *os.Root, m Member, create bool) (*os.File, error) {
	const name = ".ssh"
	info, err := root.Lstat(name)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return nil, fmt.Errorf("%w: the .ssh of %s is a symbolic link", ErrHomeShape, m.Name)
	case err != nil && !os.IsNotExist(err):
		return nil, fmt.Errorf("sysusers: read the .ssh of %s: %w", m.Name, err)
	case err != nil:
		if !create {
			return nil, err
		}
		if err := root.Mkdir(name, SSHDirMode); err != nil && !os.IsExist(err) {
			return nil, fmt.Errorf("sysusers: create the .ssh of %s: %w", m.Name, err)
		}
	}

	dir, err := root.OpenFile(name, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, fmt.Errorf("sysusers: open the .ssh of %s: %w", m.Name, err)
	}
	st, err := dir.Stat()
	if err != nil {
		dir.Close()
		return nil, fmt.Errorf("sysusers: read the .ssh of %s: %w", m.Name, err)
	}
	if !st.IsDir() {
		dir.Close()
		return nil, fmt.Errorf("%w: the .ssh of %s is not a directory", ErrHomeShape, m.Name)
	}
	// A directory kitbashd has just created belongs to root until it is given
	// away; one that was already there has to belong to the member.
	if owner, ok := ownerOf(st); ok && owner != m.UID && owner != 0 {
		dir.Close()
		return nil, fmt.Errorf("%w: the .ssh of %s belongs to uid %d", ErrHomeShape, m.Name, owner)
	}
	if err := dir.Chmod(SSHDirMode); err != nil {
		dir.Close()
		return nil, fmt.Errorf("sysusers: narrow the .ssh of %s: %w", m.Name, err)
	}
	if err := dir.Chown(m.UID, m.GID); err != nil {
		dir.Close()
		return nil, fmt.Errorf("sysusers: give the .ssh of %s to them: %w", m.Name, err)
	}
	return dir, nil
}

// checkHome reads the shape of the member's home and, when the caller is going
// to write, narrows it to the member alone. The home itself comes from the
// user database and its parent belongs to root, so it is the one name in this
// path a member cannot replace; it is still opened without following a link,
// so a home that is a symlink is refused rather than followed.
func (h *Host) checkHome(m Member, narrow bool) error {
	info, err := os.Lstat(m.Home)
	if os.IsNotExist(err) && narrow {
		if err := os.Mkdir(m.Home, HomeMode); err != nil {
			return fmt.Errorf("sysusers: create the home of %s: %w", m.Name, err)
		}
		info, err = os.Lstat(m.Home)
	}
	if err != nil {
		return fmt.Errorf("sysusers: read the home of %s: %w", m.Name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: the home of %s is a symbolic link", ErrHomeShape, m.Name)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: the home of %s is not a directory", ErrHomeShape, m.Name)
	}
	if !narrow {
		return nil
	}

	dir, err := os.OpenFile(m.Home, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("sysusers: open the home of %s: %w", m.Name, err)
	}
	defer dir.Close()
	if err := dir.Chmod(HomeMode); err != nil {
		return fmt.Errorf("sysusers: narrow the home of %s: %w", m.Name, err)
	}
	if err := dir.Chown(m.UID, m.GID); err != nil {
		return fmt.Errorf("sysusers: give the home of %s to them: %w", m.Name, err)
	}
	return nil
}

// writeKey appends one validated key line unless it is already there, and
// answers how many keys the file then holds.
func (h *Host) writeKey(m Member, line string) (int, error) {
	f, err := h.keys(m, true)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	content, err := io.ReadAll(f)
	if err != nil {
		return 0, fmt.Errorf("sysusers: read the authorized_keys of %s: %w", m.Name, err)
	}
	lines := keyLines(string(content))
	for _, existing := range lines {
		if existing == line {
			return len(lines), nil
		}
	}
	lines = append(lines, line)

	body := strings.Join(lines, "\n") + "\n"
	if err := f.Truncate(0); err != nil {
		return 0, fmt.Errorf("sysusers: write the authorized_keys of %s: %w", m.Name, err)
	}
	if _, err := f.WriteAt([]byte(body), 0); err != nil {
		return 0, fmt.Errorf("sysusers: write the authorized_keys of %s: %w", m.Name, err)
	}
	if err := f.Chown(m.UID, m.GID); err != nil {
		return 0, fmt.Errorf("sysusers: give the authorized_keys of %s to them: %w", m.Name, err)
	}
	return len(lines), nil
}

// countKeys is how many public keys a member has. A home this daemon cannot
// read, or one whose .ssh is not the shape it should be, counts as none: a
// listing is not the place to refuse.
func (h *Host) countKeys(m Member) (int, error) {
	if m.Home == "" || m.UID == 0 {
		return 0, nil
	}
	f, err := h.keys(m, false)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Printf("reading the authorized_keys of %s: %v", m.Name, err)
		}
		return 0, nil
	}
	defer f.Close()
	content, err := io.ReadAll(f)
	if err != nil {
		logger.Printf("reading the authorized_keys of %s: %v", m.Name, err)
		return 0, nil
	}
	return len(keyLines(string(content))), nil
}

// ownedFile is what an authorized_keys has to be before kitbashd writes it: a
// regular file the member owns with exactly one name. The link count is what
// refuses a hard link a member made to a file they do not own, which no check
// on the path could catch.
func ownedFile(f *os.File, m Member) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("sysusers: read the authorized_keys of %s: %w", m.Name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: the authorized_keys of %s is not a regular file", ErrHomeShape, m.Name)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(st.Uid) != m.UID {
		return fmt.Errorf("%w: the authorized_keys of %s belongs to uid %d", ErrHomeShape, m.Name, st.Uid)
	}
	if st.Nlink != 1 {
		return fmt.Errorf("%w: the authorized_keys of %s has %d links", ErrHomeShape, m.Name, st.Nlink)
	}
	return nil
}

// ownerOf reads the uid of a stat result, false on a platform that has none.
func ownerOf(info os.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
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
	if _, err := os.Lstat(m.Home); os.IsNotExist(err) {
		return target, nil
	}
	// A second member of the same name removed twice would otherwise land on
	// the first archive. The suffix keeps both.
	if _, err := os.Lstat(target); err == nil {
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

// chownTree gives a whole directory to one owner, without following a link out
// of it: the tree came from a member's home and every name in it was theirs.
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
