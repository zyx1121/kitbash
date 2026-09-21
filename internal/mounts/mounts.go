// Package mounts decides which folders of Files a Process may see as bind
// mounts, see PLAN.md section 2.3 and the mounts note in
// spec/kitbashd-api.yaml.
//
// The check that counts runs in kitbashd, as root, at registration and again
// at every start: root is the only party on this host that can answer the
// question honestly, and the answer can change between the two, because a
// folder can be removed, replaced by a symlink or lose its manifest in
// between. kitbash-mcp runs the same check as the member before it calls the
// daemon, so the problem arrives in the session that caused it; that run is a
// courtesy and is not trusted.
//
// The window between the check and the mount is real and is low severity: the
// container runs as the member, who already reaches everything under their own
// home from their own shell. What this protects is the /org rule, which is what
// makes the approval queue the trail of every write there, and progressive
// disclosure, which is what keeps an invisible folder invisible.
package mounts

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/safeopen"
)

// Max is how many folders one unit may mount, the maxItems of
// deploy.units[].mounts in spec/manifest.schema.json. Four is far above what a
// Process needs and is what keeps one manifest from turning a start into a
// hundred resolutions.
const Max = 4

// The two modes a mount declares. An empty mode is ro, which is the schema's
// default and the safe end of the two.
const (
	ModeRO = "ro"
	ModeRW = "rw"
)

// OrgRoot is the shared root, the same folder internal/fs names OrgRoot. It is
// spelled again here because this package is imported by kitbashd, which does
// not carry the fs family, the same reason sysusers.EnvCaller is spelled twice.
const OrgRoot = "/org"

// HomesRoot is the folder every member's home sits in on a kitbash host. It is
// used for one thing only: telling a member "that is another member's home"
// apart from "that is not a root at all".
const HomesRoot = "/home"

// forbiddenTargets are the trees a container may not be given a folder of Files
// at. Mounting over /etc replaces the container's own accounts and resolver
// configuration, over /proc and /sys hides the kernel interfaces the runtime
// itself set up, and over /dev takes away the devices podman created. The rest
// are where the image keeps the programs it runs: a read only mount over them
// only breaks the Package, and refusing costs one line, so they are refused
// rather than left to fail as something harder to read.
//
// None of these is something a Package has a reason to do, and every one of
// them is a way to make a container behave as something other than what its
// image says.
var forbiddenTargets = []string{
	"/proc", "/sys", "/dev", "/etc",
	"/bin", "/sbin", "/usr", "/lib", "/lib64",
}

// Declared is one mount as deploy.units[].mounts writes it, and as it crosses
// the daemon socket on a registration. It is the manifest's own type rather
// than a copy of it: a manifest and a registration that disagreed about the
// shape of a mount would be a Process running with something other than what
// its Package declared.
type Declared = manifest.Mount

// Resolved is one mount as this package answers it: the source as the kernel
// resolved it, which is the path that goes to podman, and nothing a caller
// claimed. Mode is spelled out rather than left empty, so what is recorded and
// what proc_list publishes say ro rather than nothing.
type Resolved struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Mode   string `json:"mode"`
	// Device and Inode name the folder this resolution actually opened, as the
	// kernel reported it. They are how a caller asks, after the container
	// exists, whether what was mounted is what was checked: a source swapped
	// for a link or for another folder between the check and the mount is a
	// different inode, see PLAN.md section 2.3.
	//
	// They are never stored and never listed. An inode is true of one moment,
	// and a member who removes and writes a folder again has a new one; a
	// registration holding the old one would refuse to start for a reason that
	// is not a rule. Both ends of the comparison come from one start.
	Device uint64 `json:"-"`
	Inode  uint64 `json:"-"`
}

// ReadOnly reports whether this mount is read only.
func (r Resolved) ReadOnly() bool { return r.Mode != ModeRW }

// Checker resolves the mounts of one member. Home and Org are the two roots a
// source may live under; a resolution may not leave either of them.
//
// UID is the numeric id every source under Home has to be owned by. A negative
// UID checks no ownership, which is what a test over a temporary tree uses and
// what a caller that cannot look the member up falls back to; the daemon always
// knows the uid, because it created the account.
//
// Homes is the folder members' homes sit in, /home on a kitbash host. It only
// makes the refusal readable: a source under it that is not Home is another
// member's home, and saying so is more use than "that is not a root".
type Checker struct {
	Owner string
	Home  string
	Org   string
	UID   int
	Homes string
}

// NewChecker builds the checker for one member from this host: their home, the
// shared root and the uid their files carry. A member this host does not know
// is an error, because every rule below is about a member that exists.
func NewChecker(owner string) (*Checker, error) {
	u, err := user.Lookup(owner)
	if err != nil {
		return nil, fmt.Errorf("mounts: looking up %s: %w", owner, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return nil, fmt.Errorf("mounts: the uid of %s is %q: %w", owner, u.Uid, err)
	}
	home := u.HomeDir
	if home == "" {
		home = filepath.Join(HomesRoot, owner)
	}
	return &Checker{Owner: owner, Home: filepath.Clean(home), Org: OrgRoot, UID: uid, Homes: HomesRoot}, nil
}

// homes is the folder members' homes sit in, with the fallback a checker built
// by hand gets.
func (c *Checker) homes() string {
	if c.Homes != "" {
		return c.Homes
	}
	return filepath.Dir(c.Home)
}

// Resolve checks every mount of one unit and answers them resolved, in the
// order they were declared. The first one that does not check out is the
// answer: a member fixes one mount at a time, and reporting four problems at
// once would be four problems to read for one manifest to edit.
//
// instance is what the problem names, which is the Package folder for a caller
// coming from proc_run and the request path for one coming from the daemon.
func (c *Checker) Resolve(instance string, declared []Declared) ([]Resolved, *problem.Problem) {
	if len(declared) == 0 {
		return nil, nil
	}
	if len(declared) > Max {
		return nil, problem.BadRequest(instance,
			fmt.Sprintf("this unit declares %d mounts and a Process may have at most %d", len(declared), Max),
			fmt.Sprintf("Declare at most %d entries in deploy.units[].mounts.", Max))
	}
	out := make([]Resolved, 0, len(declared))
	for _, d := range declared {
		one, prob := c.ResolveOne(instance, d)
		if prob != nil {
			return nil, prob
		}
		out = append(out, one)
	}
	return out, nil
}

// ResolveOne checks a single mount. The order of the rules is the order a
// reader of the problem can act in: the shape of the declaration first, then
// which root it names, then what the kernel finds there.
func (c *Checker) ResolveOne(instance string, d Declared) (Resolved, *problem.Problem) {
	mode := d.Mode
	if mode == "" {
		mode = ModeRO
	}
	if mode != ModeRO && mode != ModeRW {
		return Resolved{}, problem.BadRequest(instance,
			fmt.Sprintf("%q is not a mount mode", d.Mode),
			"Write mode as ro or rw, or leave it out, which is ro.")
	}
	if prob := checkTarget(instance, d.Target); prob != nil {
		return Resolved{}, prob
	}
	root, rel, prob := c.rootOf(instance, d.Source)
	if prob != nil {
		return Resolved{}, prob
	}
	// /org is read only for everyone, admins included. A member writes there
	// through the approval queue, which is the trail of every change, and a
	// Process holding a rw mount would be a way around it that leaves no
	// record at all, see PLAN.md section 2.1.
	if root == c.Org && mode == ModeRW {
		return Resolved{}, problem.NotPermitted(instance,
			fmt.Sprintf("%s is under %s, which is read only for every member, administrators included", d.Source, c.Org),
			"Mount it with mode ro. A write to /org is an approval, so a Process cannot make one directly.")
	}
	// The kernel answers before the surface does. The source is opened rather
	// than looked at: openat2 resolves the whole path in one syscall, refusing
	// every symlink on the way, inside the root as well as out of it, and every
	// resolution that would leave the root. What is fstatted afterwards is the
	// folder that was opened and not the path that was asked for.
	//
	// This runs before the visibility rule so that a link and a file are told
	// apart from a folder nobody made visible: all three would otherwise
	// answer not-visible, because reading a manifest through a link fails for
	// the same reason opening one does.
	if rel == "." {
		return Resolved{}, problem.NotVisible(instance,
			fmt.Sprintf("%s is a root and not a folder inside one, so there is no folder carrying a manifest to mount", d.Source),
			fmt.Sprintf("Mount a folder inside %s that carries a kitbash.yaml with a name and a description.", root))
	}
	f, err := safeopen.Open(root, rel, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return Resolved{}, c.openProblem(instance, d.Source, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Resolved{}, problem.Internal(instance,
			fmt.Sprintf("reading %s: %v", d.Source, err), "")
	}
	if !info.IsDir() {
		return Resolved{}, problem.InvalidPathFix(instance,
			fmt.Sprintf("%s is not a folder, and a mount is a folder", d.Source),
			"Mount the folder the file is in, and read the file inside the container.")
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Resolved{}, problem.Internal(instance,
			fmt.Sprintf("this host does not report what %s is", d.Source), "")
	}
	// Under a home, the folder has to belong to the member the Process runs
	// as. A home holds folders another member shared through a Linux group,
	// and mounting one of those would hand a Process what its owner was given
	// to read rather than what they own.
	if root == c.Home && c.UID >= 0 && int(sys.Uid) != c.UID {
		return Resolved{}, problem.NotPermitted(instance,
			fmt.Sprintf("%s is not owned by %s, and a Process mounts its owner's own folders", d.Source, c.Owner),
			"Mount a folder you own, or copy what the Process needs into one.")
	}
	// The folder has to be one a manifest describes: its own, or one above it
	// up to the root, which is what makes the folders inside a Package
	// mountable without a manifest each. It is the fs family's own rule, read
	// from the same function, because a Process being given a folder an agent
	// cannot list would be progressive disclosure with a way around it, see
	// PLAN.md section 2.1.
	if _, blocked, visible := manifest.VisibleChain(root, rel); !visible {
		return Resolved{}, problem.NotVisible(instance,
			fmt.Sprintf("%s carries no kitbash.yaml with a name and a description, and neither does any folder above it, so nothing inside it can be mounted",
				filepath.Join(root, blocked)),
			"Write a kitbash.yaml with name and description into that folder, then run the Package again.")
	}
	return Resolved{
		Source: filepath.Join(root, rel),
		Target: filepath.Clean(d.Target),
		Mode:   mode,
		Device: uint64(sys.Dev),
		Inode:  sys.Ino,
	}, nil
}

// SameFolder reports whether two resolutions of one mount found the same
// folder: the same path, and the same inode on the same device. It is what a
// caller asks after the container exists, see PLAN.md section 2.3.
func SameFolder(a, b Resolved) bool {
	return a.Source == b.Source && a.Device == b.Device && a.Inode == b.Inode
}

// Redeclare turns what a registration holds back into the declaration it came
// from, which is what a start resolves again. The source is already the
// resolved path, so this is the same question asked a second time and not a
// second reading of the manifest: what it catches is a folder that has changed
// since the registration, see spec/kitbashd-api.yaml.
func Redeclare(resolved []Resolved) []Declared {
	if len(resolved) == 0 {
		return nil
	}
	out := make([]Declared, 0, len(resolved))
	for _, r := range resolved {
		out = append(out, Declared{Source: r.Source, Target: r.Target, Mode: r.Mode})
	}
	return out
}

// Podman is the runtime's spelling of a resolved mount, which is the one form
// this package answers in that leaves kitbash: everything before it is a
// declaration, and everything after it is a command line.
func Podman(resolved []Resolved) []podman.Mount {
	if len(resolved) == 0 {
		return nil
	}
	out := make([]podman.Mount, 0, len(resolved))
	for _, r := range resolved {
		out = append(out, podman.Mount{Source: r.Source, Target: r.Target, ReadOnly: r.ReadOnly()})
	}
	return out
}

// rootOf says which root a source lives under and where below it, and refuses
// a source that is under neither. The lexical rules are applied first, so the
// problem names the path the caller wrote rather than what filepath.Join made
// of it.
func (c *Checker) rootOf(instance, source string) (root, rel string, prob *problem.Problem) {
	if source == "" || !filepath.IsAbs(source) {
		return "", "", problem.InvalidPathFix(instance,
			fmt.Sprintf("%q is not an absolute path", source),
			"Write the source as an absolute path under your home folder or under /org.")
	}
	for _, segment := range strings.Split(source, "/") {
		if segment == ".." {
			return "", "", problem.InvalidPath(instance,
				fmt.Sprintf("the mount source %s contains a parent reference", source))
		}
	}
	clean := filepath.Clean(source)
	for _, candidate := range []string{c.Home, c.Org} {
		if candidate == "" {
			continue
		}
		if clean == candidate {
			return candidate, ".", nil
		}
		if strings.HasPrefix(clean, candidate+"/") {
			return candidate, strings.TrimPrefix(clean, candidate+"/"), nil
		}
	}
	homes := c.homes()
	if homes != "" && strings.HasPrefix(clean, homes+"/") {
		return "", "", problem.NotPermitted(instance,
			fmt.Sprintf("%s is another member's home, and a Process mounts its owner's own folders", source),
			fmt.Sprintf("Mount a folder under %s, or a folder of %s.", c.Home, c.Org))
	}
	return "", "", problem.NotPermitted(instance,
		fmt.Sprintf("%s is outside Files, and the only folders a Process can be given are %s and %s", source, c.Home, c.Org),
		fmt.Sprintf("Mount a folder under %s or under %s.", c.Home, c.Org))
}

// checkTarget holds the path the container sees to the one rule it has: it is
// absolute and it is not one of the four trees that would make the container
// something other than what its image says.
func checkTarget(instance, target string) *problem.Problem {
	if target == "" || !filepath.IsAbs(target) {
		return problem.BadRequest(instance,
			fmt.Sprintf("%q is not a path inside the container", target),
			"Write the target as an absolute path, such as /files/notes.")
	}
	clean := filepath.Clean(target)
	if clean == "/" {
		return problem.BadRequest(instance,
			"a mount target of / would replace the whole container filesystem",
			"Write the target as a folder of its own, such as /files/notes.")
	}
	for _, forbidden := range forbiddenTargets {
		if clean == forbidden || strings.HasPrefix(clean, forbidden+"/") {
			return problem.BadRequest(instance,
				fmt.Sprintf("%s is under %s, which the container runtime owns", target, forbidden),
				"Write the target somewhere the image does not use, such as /files/notes.")
		}
	}
	return nil
}

// openProblem turns what the kernel said about a source into what a member
// reads. A symlink anywhere on the way and a resolution that would leave the
// root are the two this package exists for, and they are told apart from a
// folder that is simply not there.
func (c *Checker) openProblem(instance, source string, err error) *problem.Problem {
	switch {
	case errors.Is(err, syscall.ELOOP):
		return problem.InvalidPathFix(instance,
			fmt.Sprintf("%s is a symbolic link, or one of the folders above it is, and kitbash follows none", source),
			"Mount the folder itself rather than a link to it.")
	case errors.Is(err, syscall.EXDEV):
		return problem.InvalidPath(instance,
			fmt.Sprintf("%s resolves outside the root it was named under", source))
	case errors.Is(err, syscall.ENOTDIR):
		return problem.InvalidPathFix(instance,
			fmt.Sprintf("%s is not a folder, and a mount is a folder", source),
			"Mount the folder the file is in, and read the file inside the container.")
	case errors.Is(err, os.ErrNotExist):
		return problem.NotFoundFix(instance,
			fmt.Sprintf("%s does not exist", source),
			"Call fs_list on the folder above it to see what does, then mount one of those.")
	case errors.Is(err, os.ErrPermission):
		return problem.NotPermitted(instance,
			fmt.Sprintf("%s cannot be read by %s", source, c.Owner), "")
	default:
		return problem.InvalidPath(instance,
			fmt.Sprintf("%s cannot be resolved as a folder to mount: %v", source, err))
	}
}
