package sysusers

import (
	"context"
	"fmt"
	"path"
	"sort"
	"sync"
)

// Fake is an in memory System and Runner. It keeps the state a host would keep,
// so a test asserts on members and containers rather than on command lines, and
// it records every call for the assertions that are about the call after all.
//
// It lives beside Host rather than in a test file because internal/daemon tests
// against it, and because no machine that runs the unit tests has useradd,
// podman or root.
type Fake struct {
	mu sync.Mutex

	// FirstUID is the uid the first created member gets. Zero means 1000.
	FirstUID int
	// ArchiveDir is where Remove says it put a home. Empty means /org/.archive.
	ArchiveDir string
	// CreateErr, AddKeyErr, RemoveErr and ListErr make the host fail on demand.
	CreateErr error
	AddKeyErr error
	RemoveErr error
	ListErr   error
	// StartErr and RemoveAllErr make the runtime fail on demand.
	StartErr     error
	RemoveAllErr error

	// Missing are containers Start answers ErrNoContainer for, by name.
	Missing map[string]bool

	// Created, AddedKeys and Removed record what the caller asked for, and
	// Started and RemovedFor what the runtime was asked to do.
	Created    []Spec
	AddedKeys  []KeyCall
	Removed    []string
	Started    []StartCall
	RemovedFor []string

	members map[string]*fakeMember
	nextUID int
}

// KeyCall is one recorded AddKey.
type KeyCall struct {
	Name string
	Key  string
}

// StartCall is one recorded container start, with the member it ran as.
type StartCall struct {
	Member    string
	UID       int
	Container string
}

// fakeMember is one member of the fake host.
type fakeMember struct {
	member Member
	keys   []string
}

// NewFake returns an empty fake host.
func NewFake() *Fake { return &Fake{} }

// Add puts a member into the fake host without going through Create, which is
// how a test gets the user it is already running as, or a second admin.
func (f *Fake) Add(m Member) Member {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.add(m, nil).member
}

// add is Add without the lock.
func (f *Fake) add(m Member, keys []string) *fakeMember {
	if f.members == nil {
		f.members = map[string]*fakeMember{}
	}
	if m.UID == 0 {
		m.UID = f.uid()
	}
	if m.GID == 0 {
		m.GID = m.UID
	}
	if m.Home == "" {
		m.Home = path.Join("/home", m.Name)
	}
	m.Groups = groupsOf(m)
	m.Keys = len(keys)
	entry := &fakeMember{member: m, keys: keys}
	f.members[m.Name] = entry
	return entry
}

// uid hands out the next uid, starting at FirstUID.
func (f *Fake) uid() int {
	if f.nextUID == 0 {
		f.nextUID = f.FirstUID
		if f.nextUID == 0 {
			f.nextUID = 1000
		}
	}
	uid := f.nextUID
	f.nextUID++
	return uid
}

// Create records the call and adds the member, with the same refusals the host
// makes: a name outside the pattern, a key that is not a key, a name taken.
func (f *Fake) Create(_ context.Context, spec Spec) (Member, error) {
	if !ValidName(spec.Name) {
		return Member{}, fmt.Errorf("%w: %q", ErrName, spec.Name)
	}
	key, err := ValidateKey(spec.SSHKey)
	if err != nil {
		return Member{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CreateErr != nil {
		return Member{}, f.CreateErr
	}
	if _, held := f.members[spec.Name]; held {
		return Member{}, fmt.Errorf("%w: %s", ErrExists, spec.Name)
	}
	f.Created = append(f.Created, spec)
	entry := f.add(Member{Name: spec.Name, Admin: spec.Admin}, []string{key})
	return entry.member, nil
}

// AddKey records the call and appends the key unless it is already there.
func (f *Fake) AddKey(_ context.Context, name, key string) (Member, error) {
	line, err := ValidateKey(key)
	if err != nil {
		return Member{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.AddKeyErr != nil {
		return Member{}, f.AddKeyErr
	}
	entry, held := f.members[name]
	if !held {
		return Member{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	f.AddedKeys = append(f.AddedKeys, KeyCall{Name: name, Key: line})
	for _, existing := range entry.keys {
		if existing == line {
			return entry.member, nil
		}
	}
	entry.keys = append(entry.keys, line)
	entry.member.Keys = len(entry.keys)
	return entry.member, nil
}

// Remove records the call and takes the member away, answering where their
// home was archived.
func (f *Fake) Remove(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RemoveErr != nil {
		return "", f.RemoveErr
	}
	if _, held := f.members[name]; !held {
		return "", fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	f.Removed = append(f.Removed, name)
	delete(f.members, name)
	dir := f.ArchiveDir
	if dir == "" {
		dir = DefaultArchive
	}
	return path.Join(dir, name), nil
}

// List answers every member, by name.
func (f *Fake) List(_ context.Context) ([]Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	out := make([]Member, 0, len(f.members))
	for _, entry := range f.members {
		out = append(out, entry.member)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Lookup answers one member.
func (f *Fake) Lookup(_ context.Context, name string) (Member, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, held := f.members[name]
	if !held {
		return Member{}, false, nil
	}
	return entry.member, true, nil
}

// Start records a container start as one member. A container named in Missing
// is ErrNoContainer, which is what restore unregisters.
func (f *Fake) Start(_ context.Context, m Member, container string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Missing[container] {
		return fmt.Errorf("%w: %s", ErrNoContainer, container)
	}
	if f.StartErr != nil {
		return f.StartErr
	}
	f.Started = append(f.Started, StartCall{Member: m.Name, UID: m.UID, Container: container})
	return nil
}

// RemoveAll records that one member's containers were removed.
func (f *Fake) RemoveAll(_ context.Context, m Member) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RemoveAllErr != nil {
		return f.RemoveAllErr
	}
	f.RemovedFor = append(f.RemovedFor, m.Name)
	return nil
}

// Calls answers the recorded container starts, newest last.
func (f *Fake) Calls() []StartCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]StartCall, len(f.Started))
	copy(out, f.Started)
	return out
}

// groupsOf is the group list a member of this host has: their own group,
// kitbash-users, and kitbash-admin for an admin.
func groupsOf(m Member) []string {
	groups := []string{m.Name, UsersGroup}
	if m.Admin {
		groups = append(groups, AdminGroup)
	}
	return groups
}
