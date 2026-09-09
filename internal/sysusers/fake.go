package sysusers

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"sync"
	"syscall"

	"github.com/zyx1121/kitbash/internal/podman"
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
	// StartErr, RunErr, StopErr, RemoveContainerErr and RemoveAllErr make the
	// runtime fail on demand.
	StartErr           error
	RunErr             error
	StopErr            error
	RemoveContainerErr error
	RemoveAllErr       error
	// RunID is the container id Run answers. Empty means a fixed one.
	RunID string

	// Missing are containers Start, Stop and Remove answer ErrNoContainer
	// for, by name, and images Run answers ErrNoImage for.
	Missing map[string]bool
	// Running are containers Start answers ErrAlreadyRunning for, which is
	// what a daemon that restarted without the host finds.
	Running map[string]bool

	// Created, AddedKeys and Removed record what the caller asked for, and
	// Started, Ran, Stopped, RemovedContainers and RemovedFor what the
	// runtime was asked to do.
	Created           []Spec
	AddedKeys         []KeyCall
	Removed           []string
	Started           []StartCall
	Ran               []RunCall
	Stopped           []StopCall
	RemovedContainers []StopCall
	RemovedFor        []string

	members map[string]*fakeMember
	nextUID int
}

// KeyCall is one recorded AddKey.
type KeyCall struct {
	Name string
	Key  string
}

// StartCall is one recorded container start, with the member it ran as and
// the cgroup leaf the child was placed in.
type StartCall struct {
	Member    string
	UID       int
	Container string
	Cgroup    string
}

// RunCall is one recorded container run. Args is the command line the runtime
// would have been given, built by the same function the real runner uses, so a
// test asserts on the flags rather than on a struct kitbashd filled in.
//
// Env is what the environment file held when the run happened, with its mode
// and owner: the file is the caller's to write and to remove, and a test that
// only looked afterwards would find nothing.
type RunCall struct {
	Member  string
	UID     int
	Options podman.RunOptions
	Args    []string
	Cgroup  string
	EnvFile string
	Env     string
	EnvMode fs.FileMode
	EnvUID  int
	EnvGID  int
}

// StopCall is one recorded stop or removal.
type StopCall struct {
	Member    string
	Container string
	Timeout   int
	Force     bool
}

// fakeMember is one member of the fake host.
type fakeMember struct {
	member Member
	keys   []string
}

// NewFake returns an empty fake host.
func NewFake() *Fake { return &Fake{} }

// Add puts a member into the fake host without going through Create, which is
// how a test gets the user it is already running as, or a second admin. What
// the caller leaves empty is filled in the way a host would: a uid, a home
// under /home, and membership of kitbash-users.
func (f *Fake) Add(m Member) Member {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.add(m, nil).member
}

// AddAccount puts an account in exactly as it is given, uid included. It is
// how a test gets root, sshd or any other account that lives on a host and is
// not an organization member.
func (f *Fake) AddAccount(m Member) Member {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.members == nil {
		f.members = map[string]*fakeMember{}
	}
	if m.Groups == nil {
		m.Groups = []string{}
	}
	entry := &fakeMember{member: m}
	f.members[m.Name] = entry
	return entry.member
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
	if m.Groups == nil {
		m.Groups = groupsOf(m)
	}
	m.Keys = len(keys)
	entry := &fakeMember{member: m, keys: keys}
	f.members[m.Name] = entry
	return entry
}

// member is the lookup the two calls that write share: the account has to be
// an organization member, which is what keeps this family away from root, from
// sshd and from every other account on the host. It is called with the lock
// held.
func (f *Fake) member(name string) (*fakeMember, error) {
	entry, held := f.members[name]
	if !held || !entry.member.IsMember() {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return entry, nil
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
	entry, err := f.member(name)
	if err != nil {
		return Member{}, err
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
	if _, err := f.member(name); err != nil {
		return "", err
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
func (f *Fake) Start(_ context.Context, m Member, container, cgroup string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Missing[container] {
		return fmt.Errorf("%w: %s", ErrNoContainer, container)
	}
	if f.Running[container] {
		return fmt.Errorf("%w: %s", ErrAlreadyRunning, container)
	}
	if f.StartErr != nil {
		return f.StartErr
	}
	f.Started = append(f.Started, StartCall{
		Member: m.Name, UID: m.UID, Container: container, Cgroup: cgroup,
	})
	return nil
}

// Run records a container run as one member, reading the environment file
// while it still exists. A container named in Missing is ErrNoImage: the fake
// has no image store, so the one thing a caller stages is an image that is not
// there.
func (f *Fake) Run(_ context.Context, m Member, opts podman.RunOptions, cgroup string) (string, error) {
	call := RunCall{
		Member:  m.Name,
		UID:     m.UID,
		Options: opts,
		Args:    podman.RunArgs(opts, nil),
		Cgroup:  cgroup,
		EnvFile: opts.EnvFile,
	}
	if opts.EnvFile != "" {
		if body, err := os.ReadFile(opts.EnvFile); err == nil {
			call.Env = string(body)
		}
		if info, err := os.Stat(opts.EnvFile); err == nil {
			call.EnvMode = info.Mode().Perm()
			if sys, ok := info.Sys().(*syscall.Stat_t); ok {
				call.EnvUID = int(sys.Uid)
				call.EnvGID = int(sys.Gid)
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Missing[opts.Image] {
		return "", fmt.Errorf("%w: %s", ErrNoImage, opts.Image)
	}
	if f.RunErr != nil {
		return "", f.RunErr
	}
	f.Ran = append(f.Ran, call)
	if f.RunID != "" {
		return f.RunID, nil
	}
	return "container-" + opts.Name, nil
}

// Stop records a stop as one member.
func (f *Fake) Stop(_ context.Context, m Member, container string, timeout int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Missing[container] {
		return fmt.Errorf("%w: %s", ErrNoContainer, container)
	}
	if f.StopErr != nil {
		return f.StopErr
	}
	f.Stopped = append(f.Stopped, StopCall{Member: m.Name, Container: container, Timeout: timeout})
	return nil
}

// RemoveContainer records a removal as one member.
func (f *Fake) RemoveContainer(_ context.Context, m Member, container string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Missing[container] {
		return fmt.Errorf("%w: %s", ErrNoContainer, container)
	}
	if f.RemoveContainerErr != nil {
		return f.RemoveContainerErr
	}
	f.RemovedContainers = append(f.RemovedContainers, StopCall{
		Member: m.Name, Container: container, Force: force,
	})
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

// Runs answers the recorded container runs, newest last.
func (f *Fake) Runs() []RunCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RunCall, len(f.Ran))
	copy(out, f.Ran)
	return out
}

// Stops answers the recorded stops, newest last.
func (f *Fake) Stops() []StopCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]StopCall, len(f.Stopped))
	copy(out, f.Stopped)
	return out
}

// Removals answers the recorded container removals, newest last.
func (f *Fake) Removals() []StopCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]StopCall, len(f.RemovedContainers))
	copy(out, f.RemovedContainers)
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
