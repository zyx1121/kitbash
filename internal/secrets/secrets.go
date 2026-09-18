// Package secrets holds the values a member gives their own Processes: one
// root owned file per name under /var/lib/kitbash/secrets/<member>/<NAME>, see
// PLAN.md section 2.3.
//
// They are files and not rows on purpose. The store is copied once a day and
// that copy leaves the host, so a secret in the database would be a secret in
// every backup; a file outside it is one the backup never reads, see PLAN.md
// section 4.7.
//
// Every open below the base directory goes through internal/safeopen, so no
// component of a path this package resolves may be a symlink: the tree is root
// owned and 0700, so only root can plant one, and a link planted there would
// otherwise be a way to write a member's value into a file of somebody else's
// choosing. The rename that makes a write atomic and the unlink that removes a
// name are both made against a descriptor on the member's directory, so what
// they act on is the folder that was resolved and not the path it came from.
package secrets

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/safeopen"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// DefaultDir is where a kitbash host keeps them, see PLAN.md section 4.7. It
// is root owned and 0700, which is what keeps every member out of every other
// member's values: kitbashd is the only reader, and it answers about the peer
// and nobody else.
const DefaultDir = "/var/lib/kitbash/secrets"

// The modes of the tree. The base directory and each member's directory are
// 0700 and each value is 0600, all owned by root: a Process is given its
// values through the environment file kitbashd writes, so nothing but the
// daemon ever opens one of these.
const (
	DirMode  = 0o700
	FileMode = 0o600
)

// MaxValueBytes is the longest value this API carries. The environment file is
// line based, so a value is one line, and a credential that needs more than
// this is encoded by the member before it is set, see PLAN.md section 2.3.
const MaxValueBytes = 8192

// The three refusals a caller has to tell apart: a name that is not a name, a
// member this host does not spell that way, and a value no environment file
// can carry. They are errors rather than problem details because this package
// says nothing about HTTP; internal/daemon turns them into problems.
var (
	ErrName   = errors.New("secrets: the name is not an environment variable name kitbash holds")
	ErrMember = errors.New("secrets: the member name is not one kitbash creates")
	ErrValue  = errors.New("secrets: the value is not one an environment file carries")
)

// Entry is one name a member holds and when it was last written. It is what
// secrets_list answers with, and it carries no value: a listing exists so a
// member can see which names are there, see PLAN.md section 2.3.
type Entry struct {
	Name    string    `json:"name"`
	Updated time.Time `json:"updated"`
}

// Store is the tree under one base directory. The zero value is not usable;
// call New.
type Store struct {
	dir string
}

// New answers the store rooted at dir. An empty dir means DefaultDir, which is
// what a kitbash host runs with; a test passes a temporary directory, the same
// way the store path is given to kitbashd.
func New(dir string) *Store {
	if dir == "" {
		dir = DefaultDir
	}
	return &Store{dir: dir}
}

// Dir is where this store keeps its files.
func (s *Store) Dir() string { return s.dir }

// Prepare creates the base directory if it is not there and narrows it if an
// upgrade left it wider. kitbashd calls it at start, so an operator never has
// to create it and a host that was installed before secrets existed gains it
// on the next start.
func (s *Store) Prepare() error {
	if err := os.MkdirAll(s.dir, DirMode); err != nil {
		return fmt.Errorf("secrets: the directory %s: %w", s.dir, err)
	}
	// A directory an older release left group or world readable would leave
	// every value readable by anyone who can reach the path, so the mode is
	// set rather than assumed, the same way the store directory is.
	if err := os.Chmod(s.dir, DirMode); err != nil {
		return fmt.Errorf("secrets: the directory %s: %w", s.dir, err)
	}
	return nil
}

// Set writes one value for one member and answers when it was written.
//
// The write is atomic: the value goes into a temporary file in the member's
// own directory and is renamed onto the name, so a reader of a name either
// reads what was there before or reads the whole new value, and a write that
// fails halfway leaves the previous value in place. Nothing that the next
// start reads is ever half a secret.
func (s *Store) Set(member, name, value string) (time.Time, error) {
	if err := check(member, name); err != nil {
		return time.Time{}, err
	}
	if err := CheckValue(value); err != nil {
		return time.Time{}, err
	}
	if err := s.Prepare(); err != nil {
		return time.Time{}, err
	}
	if _, err := safeopen.MkdirAll(s.dir, member, DirMode); err != nil {
		return time.Time{}, fmt.Errorf("secrets: the directory of %s: %w", member, err)
	}
	dir, err := s.memberDir(member)
	if err != nil {
		return time.Time{}, err
	}
	defer dir.Close()

	// The temporary name carries a dot, which no name this package accepts
	// can, so one left behind by a daemon that was killed mid write is never
	// listed and never read as a secret.
	temp := name + "." + stamp() + ".tmp"
	f, err := safeopen.Open(s.dir, filepath.Join(member, temp), os.O_WRONLY|os.O_CREATE|os.O_EXCL, FileMode)
	if err != nil {
		return time.Time{}, fmt.Errorf("secrets: writing %s of %s: %w", name, member, err)
	}
	// The mode is set rather than left to the umask kitbashd happens to run
	// with: 0600 is the promise, not a ceiling.
	if err := f.Chmod(FileMode); err != nil {
		f.Close()
		s.discard(dir, temp)
		return time.Time{}, fmt.Errorf("secrets: writing %s of %s: %w", name, member, err)
	}
	if _, err := f.WriteString(value); err != nil {
		f.Close()
		s.discard(dir, temp)
		return time.Time{}, fmt.Errorf("secrets: writing %s of %s: %w", name, member, err)
	}
	if err := f.Close(); err != nil {
		s.discard(dir, temp)
		return time.Time{}, fmt.Errorf("secrets: writing %s of %s: %w", name, member, err)
	}
	// The rename is made against the descriptor on the member's directory,
	// which is the folder safeopen resolved, so neither side of it is a path
	// the kernel resolves again on our behalf.
	if err := unix.Renameat(int(dir.Fd()), temp, int(dir.Fd()), name); err != nil {
		s.discard(dir, temp)
		return time.Time{}, fmt.Errorf("secrets: writing %s of %s: %w", name, member, err)
	}
	info, err := safeopen.Stat(s.dir, filepath.Join(member, name))
	if err != nil {
		return time.Time{}, fmt.Errorf("secrets: reading back %s of %s: %w", name, member, err)
	}
	return info.ModTime().UTC(), nil
}

// Get answers one value and whether the member holds that name at all. It is
// what a start resolves a declared name with; nothing else in kitbash reads a
// value.
func (s *Store) Get(member, name string) (string, bool, error) {
	if err := check(member, name); err != nil {
		return "", false, err
	}
	f, err := safeopen.Open(s.dir, filepath.Join(member, name), os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("secrets: reading %s of %s: %w", name, member, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", false, fmt.Errorf("secrets: reading %s of %s: %w", name, member, err)
	}
	// Anything that is not a regular file is not a value this daemon wrote,
	// and reading it could block on a FIFO or reach a device.
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("secrets: reading %s of %s: %w", name, member, ErrName)
	}
	// One byte more than a value may be is read, so a file that grew past the
	// limit is refused rather than truncated into a value that is not the one
	// the member set.
	body, err := io.ReadAll(io.LimitReader(f, MaxValueBytes+1))
	if err != nil {
		return "", false, fmt.Errorf("secrets: reading %s of %s: %w", name, member, err)
	}
	if len(body) > MaxValueBytes {
		return "", false, fmt.Errorf("secrets: reading %s of %s: %w", name, member, ErrValue)
	}
	return string(body), true, nil
}

// List answers the names one member holds, sorted, with no value anywhere in
// it. A member who holds none, and a member who has never set one, both answer
// an empty list rather than a failure.
func (s *Store) List(member string) ([]Entry, error) {
	if !sysusers.ValidName(member) {
		return nil, ErrMember
	}
	dir, err := safeopen.Open(s.dir, member, unix.O_DIRECTORY|os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Entry{}, nil
		}
		return nil, fmt.Errorf("secrets: listing the secrets of %s: %w", member, err)
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("secrets: listing the secrets of %s: %w", member, err)
	}
	out := []Entry{}
	for _, entry := range entries {
		// Only a regular file with a name this package accepts is a secret.
		// A temporary file a killed daemon left behind carries a dot and is
		// skipped by the same rule.
		if !entry.Type().IsRegular() || !manifest.ValidSecretName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		out = append(out, Entry{Name: entry.Name(), Updated: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Remove drops one name and reports whether there was one to drop. Removing a
// name the member does not hold is not a failure: the call describes the state
// it wants and repeating it is safe, which is the invariant in PLAN.md 2.6.
func (s *Store) Remove(member, name string) (bool, error) {
	if err := check(member, name); err != nil {
		return false, err
	}
	dir, err := s.memberDir(member)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer dir.Close()
	if err := unix.Unlinkat(int(dir.Fd()), name, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("secrets: removing %s of %s: %w", name, member, err)
	}
	return true, nil
}

// RemoveMember takes a member's whole directory away, which is what removing
// the member does: their account is gone, so the values they held are not
// somebody else's to inherit, see PLAN.md section 2.3.
func (s *Store) RemoveMember(member string) error {
	if !sysusers.ValidName(member) {
		return ErrMember
	}
	// The name is checked above and the base is the daemon's own path, so this
	// removes one directory of this tree and nothing that was named by a
	// caller.
	if err := os.RemoveAll(filepath.Join(s.dir, member)); err != nil {
		return fmt.Errorf("secrets: removing the secrets of %s: %w", member, err)
	}
	return nil
}

// memberDir opens one member's directory without following a symlink at any
// component, which is the descriptor the rename and the unlink act against.
func (s *Store) memberDir(member string) (*os.File, error) {
	dir, err := safeopen.Open(s.dir, member, unix.O_DIRECTORY|unix.O_PATH, 0)
	if err != nil {
		return nil, fmt.Errorf("secrets: the directory of %s: %w", member, err)
	}
	return dir, nil
}

// discard removes a temporary file a failed write left behind. A removal that
// fails is not reported: the caller is already answering with why the write
// failed, and the file is invisible to every other call here.
func (s *Store) discard(dir *os.File, temp string) {
	_ = unix.Unlinkat(int(dir.Fd()), temp, 0)
}

// check holds a member and a name to their shapes. Both are checked on every
// call rather than once at the edge: this package writes paths made of them,
// so it is the one that must not be talked into a path of somebody else's.
func check(member, name string) error {
	if !sysusers.ValidName(member) {
		return ErrMember
	}
	if !manifest.ValidSecretName(name) {
		return ErrName
	}
	return nil
}

// CheckValue holds a value to what an environment file line can carry: at
// least one byte, at most MaxValueBytes, no NUL and no line break, see PLAN.md
// section 2.3. The refusal names the shape and never the value.
func CheckValue(value string) error {
	if len(value) == 0 {
		return fmt.Errorf("%w: it is empty", ErrValue)
	}
	if len(value) > MaxValueBytes {
		return fmt.Errorf("%w: it is %d bytes, over the %d this API carries",
			ErrValue, len(value), MaxValueBytes)
	}
	if strings.ContainsRune(value, 0) {
		return fmt.Errorf("%w: it carries a NUL byte", ErrValue)
	}
	if strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("%w: it carries a line break", ErrValue)
	}
	return nil
}

// stamp is what makes one write's temporary file its own, so two writes of one
// name never share a temporary path.
func stamp() string {
	return fmt.Sprintf("%d.%d", os.Getpid(), time.Now().UnixNano())
}
