package sysusers

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
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
	// StartErr, RunErr, InitErr, StopErr, RemoveContainerErr, RemoveAllErr and
	// CopyErr make the runtime fail on demand. RunErr is the create, which is
	// the call that makes a container.
	StartErr           error
	RunErr             error
	InitErr            error
	StopErr            error
	RemoveContainerErr error
	RemoveAllErr       error
	CopyErr            error
	// ImageInfoErr makes reading an image fail for a reason that is not a
	// missing image, which is the host's failure and not the caller's.
	ImageInfoErr error
	// ConfigErr makes reading one container's configuration fail, which is a
	// host whose runtime answers nothing about a container it has.
	ConfigErr error
	// RenameErr makes a rename fail, which is the one step of a heal that
	// leaves the container where it was.
	RenameErr error
	// LoseCopies makes a copy succeed without the image arriving, which is
	// the one failure a caller cannot see from the exit status of the two
	// children: a save and a load that both said nothing and moved nothing.
	LoseCopies bool
	// RunID is the container id Create answers. Empty means a fixed one.
	RunID string
	// PIDs are the process ids Init gives a container, by name. A container
	// with no entry is given NextPID, and a container mapped to zero is one
	// whose init made no process, which is what a host answers when the
	// runtime could not prepare it.
	PIDs map[string]int
	// NextPID is the pid every other container's init answers. Zero means a
	// fixed one, which is enough for a test that only needs a pid that is not
	// nothing.
	NextPID int
	// ProcRoot is the tree standing in for /proc. When it is set, preparing a
	// container writes what its namespace holds under
	// <ProcRoot>/<pid>/root/<target>, pointing at the source it was created
	// with, which is what an honest host does. A test that wants a host that
	// mounted something else writes over it afterwards.
	ProcRoot string

	// Missing are containers Start, Stop and Remove answer ErrNoContainer
	// for, by name, and images Run answers ErrNoImage for.
	Missing map[string]bool
	// Running are containers Start answers ErrAlreadyRunning for, which is
	// what a daemon that restarted without the host finds.
	Running map[string]bool
	// Pinned are configurations ContainerConfig answers whatever the container
	// was run with, which Run never replaces. It is how a test stages a host
	// that did something other than what it was asked: a runtime that mounted
	// another folder than the one on its command line cannot be staged in
	// Configs, because a run overwrites that with the options it was given.
	Pinned map[string]ContainerConfig
	// Configs are the configurations ContainerConfig answers, by container
	// name. A container with no entry answers an empty configuration, which
	// is one created under no cgroup parent of its own.
	Configs map[string]ContainerConfig

	// Created, AddedKeys and Removed record what the caller asked for, and
	// Started, Ran, Stopped, RemovedContainers, RemovedFor and Copied what
	// the runtime was asked to do.
	Created           []Spec
	AddedKeys         []KeyCall
	Removed           []string
	Started           []StartCall
	Ran               []RunCall
	Inited            []StartCall
	Stopped           []StopCall
	Renamed           []RenameCall
	RemovedContainers []StopCall
	RemovedFor        []string
	Copied            []CopyCall

	members map[string]*fakeMember
	nextUID int
	// made are the containers this fake created itself, which is what tells a
	// container the heal made from the one it replaced, see Start.
	made map[string]bool
	// images is the image store of the fake host, one per member: which
	// digests they hold, how big each one is and what its labels say it is a
	// build of. A copy reads one member's and writes the other's, the way a
	// save into a load does, and the labels travel with the image because
	// they are inside it.
	images map[string]map[string]ImageInfo
}

// CopyCall is one recorded image copy, with the two members it ran as and the
// cgroup leaf each child was placed in.
type CopyCall struct {
	From       string
	To         string
	Digest     string
	FromCgroup string
	ToCgroup   string
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

// RenameCall is one recorded rename.
type RenameCall struct {
	Member string
	From   string
	To     string
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
	// StartErr is the host refusing a container this fake did not make, which
	// is what a container created before kitbashd gave each Process a cgroup
	// of its own does: its parent belongs to root and the member's runtime
	// cannot start it there. One this fake created is one the heal made, and
	// it starts.
	if f.StartErr != nil && !f.made[container] {
		return f.StartErr
	}
	f.Started = append(f.Started, StartCall{
		Member: m.Name, UID: m.UID, Container: container, Cgroup: cgroup,
	})
	return nil
}

// CreateContainer records a container being made as one member, reading the environment
// file while it still exists. Nothing runs in it, which is what the real
// runtime does too. An image named in Missing is ErrNoImage: the fake has no
// image store, so the one thing a caller stages is an image that is not there.
func (f *Fake) CreateContainer(_ context.Context, m Member, opts podman.RunOptions, cgroup string) (string, error) {
	call := RunCall{
		Member:  m.Name,
		UID:     m.UID,
		Options: opts,
		Args:    podman.CreateArgs(opts, nil),
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
	if f.made == nil {
		f.made = map[string]bool{}
	}
	f.made[opts.Name] = true
	// The container exists now, with the configuration the run gave it, which
	// is what ContainerConfig answers and what a health probe is checked
	// against.
	if f.Configs == nil {
		f.Configs = map[string]ContainerConfig{}
	}
	// The container exists and nothing runs in it, which is a created
	// container with no pid: Init is what gives it one.
	f.Configs[opts.Name] = ContainerConfig{
		CgroupParent: opts.CgroupParent,
		Image:        opts.Image,
		Labels:       opts.Labels,
		Restart:      opts.Restart,
		Publish:      opts.Publish,
		Mounts:       opts.Mounts,
	}
	if f.RunID != "" {
		return f.RunID, nil
	}
	return "container-" + opts.Name, nil
}

// InitContainer gives a created container the pid and the mount namespace the real
// runtime gives it, which is what the caller reads its mounts through. A
// container the fake has no configuration for is ErrNoContainer, because a
// container that was never created cannot be prepared.
func (f *Fake) InitContainer(_ context.Context, m Member, container, cgroup string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Missing[container] {
		return fmt.Errorf("%w: %s", ErrNoContainer, container)
	}
	f.Inited = append(f.Inited, StartCall{Member: m.Name, UID: m.UID, Container: container, Cgroup: cgroup})
	if f.InitErr != nil {
		return f.InitErr
	}
	config, held := f.Configs[container]
	if !held {
		return fmt.Errorf("%w: %s", ErrNoContainer, container)
	}
	config.PID = f.pidFor(container)
	f.Configs[container] = config
	if f.ProcRoot == "" {
		return nil
	}
	// An honest host: what the container holds at each target is the folder it
	// was created with. /proc/<pid>/root is a magic link the kernel resolves
	// in the container's namespace, and a symlink is the nearest thing a test
	// can write, so the caller's own stat resolves to the real folder.
	//
	// A target a test has already written is left alone: that test has said
	// what the container holds, which is how a host that mounted something
	// other than what it was asked for is staged.
	for _, mount := range config.Mounts {
		at := filepath.Join(f.ProcRoot, strconv.Itoa(config.PID), "root", mount.Target)
		if _, err := os.Lstat(at); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(mount.Source, at); err != nil {
			return err
		}
	}
	return nil
}

// pidFor is the pid one container's init answers. It is not locked: every
// caller of it holds the lock already.
func (f *Fake) pidFor(container string) int {
	if pid, ok := f.PIDs[container]; ok {
		return pid
	}
	if f.NextPID > 0 {
		return f.NextPID
	}
	return 4242
}

// Inits are the containers the runtime was asked to prepare, newest last.
func (f *Fake) Inits() []StartCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]StartCall, len(f.Inited))
	copy(out, f.Inited)
	return out
}

// ContainerConfig answers what the fake host holds for one container. A
// container named in Missing is ErrNoContainer, and one the test has staged no
// configuration for answers an empty one: what a caller reads off it is the
// cgroup parent, and no parent at all is what a container created before
// kitbashd wrote a ceiling per Process has.
func (f *Fake) ContainerConfig(_ context.Context, _ Member, container string) (ContainerConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Missing[container] {
		return ContainerConfig{}, fmt.Errorf("%w: %s", ErrNoContainer, container)
	}
	if f.ConfigErr != nil {
		return ContainerConfig{}, f.ConfigErr
	}
	if config, ok := f.Pinned[container]; ok {
		return config, nil
	}
	return f.Configs[container], nil
}

// PinConfig stages what the runtime answers about one container, whatever it is
// later run with.
func (f *Fake) PinConfig(container string, config ContainerConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Pinned == nil {
		f.Pinned = map[string]ContainerConfig{}
	}
	f.Pinned[container] = config
}

// RenameContainer records a rename and moves what the fake host holds under
// the old name to the new one. A container named in Missing is ErrNoContainer
// and a name that is taken is refused, the way the runtime refuses it.
func (f *Fake) RenameContainer(_ context.Context, m Member, from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Missing[from] {
		return fmt.Errorf("%w: %s", ErrNoContainer, from)
	}
	if f.RenameErr != nil {
		return f.RenameErr
	}
	if _, held := f.Configs[to]; held {
		return fmt.Errorf("sysusers: podman rename %s: the name %s is already in use", from, to)
	}
	f.Renamed = append(f.Renamed, RenameCall{Member: m.Name, From: from, To: to})
	if config, held := f.Configs[from]; held {
		f.Configs[to] = config
		delete(f.Configs, from)
	}
	if f.Running[from] {
		delete(f.Running, from)
		f.Running[to] = true
	}
	return nil
}

// Renames answers the recorded renames, newest last.
func (f *Fake) Renames() []RenameCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RenameCall, len(f.Renamed))
	copy(out, f.Renamed)
	return out
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
	delete(f.Configs, container)
	delete(f.Running, container)
	return nil
}

// Publish stages a container that publishes these host ports, which is what a
// health probe is checked against. Only the host side matters to the check, so
// the container side is the same one for each.
func (f *Fake) Publish(container string, ports ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Configs == nil {
		f.Configs = map[string]ContainerConfig{}
	}
	config := f.Configs[container]
	config.Publish = nil
	for _, port := range ports {
		config.Publish = append(config.Publish, podman.PortMapping{HostPort: port, ContainerPort: 8080})
	}
	f.Configs[container] = config
}

// AddImage puts one image in a member's store, which is what a build of
// theirs would have left there. The labels are given rather than derived: they
// are the provenance every caller checks, so a test that wants an image whose
// labels lie says so.
func (f *Fake) AddImage(member, digest string, size int64, labels map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addImage(member, digest, ImageInfo{Size: size, Labels: labels})
}

// addImage is AddImage without the lock.
func (f *Fake) addImage(member, digest string, info ImageInfo) {
	if f.images == nil {
		f.images = map[string]map[string]ImageInfo{}
	}
	if f.images[member] == nil {
		f.images[member] = map[string]ImageInfo{}
	}
	if info.Labels == nil {
		info.Labels = map[string]string{}
	}
	f.images[member][digest] = info
}

// DropImages empties one member's image store, which is a member who removed
// what they had built.
func (f *Fake) DropImages(member string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.images, member)
}

// HasImage reports whether a member's store holds one image, which is how a
// test sees that a copy arrived.
func (f *Fake) HasImage(member, digest string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, held := f.images[member][digest]
	return held
}

// CopyImage records the copy and moves the image into the second member's
// store. An image the first member does not have is ErrNoImage, the same
// answer a save of an image that is not there gives.
func (f *Fake) CopyImage(_ context.Context, from, to Member, digest, fromCgroup, toCgroup string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Copied = append(f.Copied, CopyCall{
		From: from.Name, To: to.Name, Digest: digest,
		FromCgroup: fromCgroup, ToCgroup: toCgroup,
	})
	if f.CopyErr != nil {
		return f.CopyErr
	}
	info, held := f.images[from.Name][digest]
	if !held {
		return fmt.Errorf("%w: %s", ErrNoImage, digest)
	}
	if f.LoseCopies {
		return nil
	}
	// The labels are inside the image, so they arrive with it: a copy cannot
	// change what an image says it is a build of.
	f.addImage(to.Name, digest, info)
	return nil
}

// ImageInfo answers what a member's store holds, and ErrNoImage for a digest
// it does not.
func (f *Fake) ImageInfo(_ context.Context, m Member, digest string) (ImageInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ImageInfoErr != nil {
		return ImageInfo{}, f.ImageInfoErr
	}
	info, held := f.images[m.Name][digest]
	if !held {
		return ImageInfo{}, fmt.Errorf("%w: %s", ErrNoImage, digest)
	}
	return info, nil
}

// Copies answers the recorded image copies, newest last.
func (f *Fake) Copies() []CopyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]CopyCall, len(f.Copied))
	copy(out, f.Copied)
	return out
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
