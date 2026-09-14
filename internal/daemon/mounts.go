package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/zyx1121/kitbash/internal/mounts"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// The mounts of a Process are resolved here, as root, and nowhere else that
// counts. kitbash-mcp runs the same check as the member first so the problem
// arrives in the session that caused it, but that process is the member's and
// what it says about a path is a claim: the daemon is the one that decides,
// see mounts in spec/kitbashd-api.yaml.
//
// The check runs twice, at registration and at every start, because the answer
// can change in between: a folder can be removed, replaced by a symlink or
// lose its manifest while the Process is registered and not running. A restore
// takes the same path as a start, so a mount that has stopped being legal
// fails the restore and proc_list reports that Process failed with the reason.

// mountChecker is the checker for one member on this host: their own home, the
// shared root and the uid every source under the home has to be owned by.
func (s *Server) mountChecker(m sysusers.Member) *mounts.Checker {
	home := m.Home
	if home == "" {
		home = mounts.HomesRoot + "/" + m.Name
	}
	return &mounts.Checker{
		Owner: m.Name,
		Home:  home,
		Org:   s.orgRoot,
		UID:   m.UID,
		Homes: mounts.HomesRoot,
	}
}

// resolveMounts checks what a registration declared and answers what it
// resolves to, which is what goes into the store. A member this host does not
// know is refused: every rule is about a member that exists, and a Process runs
// as one.
func (s *Server) resolveMounts(ctx context.Context, instance, owner string, declared []mounts.Declared) ([]mounts.Resolved, *problem.Problem) {
	if len(declared) == 0 {
		return nil, nil
	}
	m, found, err := s.users.Lookup(ctx, owner)
	if err != nil {
		return nil, problem.Internal(instance, err.Error(), "")
	}
	if !found {
		return nil, problem.NotPermitted(instance,
			owner+" is not a member of this host, so kitbashd cannot say which folders they own",
			"Ask an administrator to create a member for this account.")
	}
	return s.mountChecker(m).Resolve(instance, declared)
}

// revalidateMounts resolves what a registration already holds, before the
// container is created. The stored source is already the resolved path, so
// this is the same question asked again rather than a second interpretation of
// the manifest: what it catches is a folder that has changed since.
func (s *Server) revalidateMounts(instance string, p store.Process, m sysusers.Member) ([]mounts.Resolved, *problem.Problem) {
	if len(p.Mounts) == 0 {
		return nil, nil
	}
	return s.mountChecker(m).Resolve(instance, mounts.Redeclare(p.Mounts))
}

// DefaultProcRoot is this host's process table, which is how kitbashd reads
// what a container has in its own mount namespace: /proc/<pid>/root resolves
// inside that namespace, and root needs nothing in the image to read it.
const DefaultProcRoot = "/proc"

// MountSwapped is the detail of the one refusal this file exists for. It is
// spelled once because three callers answer with it and a test matches on it.
const MountSwapped = "the mount source changed between validation and start"

// mountSwapFix is what the owner of that Process can do about it, which is
// look at the folder rather than at kitbash.
const mountSwapFix = "Check the folder deploy.units[0].mounts names: it was replaced while the Process was starting. Run the Package again once it is the folder you meant."

// verifyMounts holds a prepared container to what was validated, and answers a
// problem when it does not match. There is no fallback: a container kitbashd
// cannot read is a container that does not start.
//
// It is called between podman init and podman start. The bind mounts of a
// container are made by init, which leaves its init process created and the
// image's entrypoint not yet executed, so this reads what the container
// actually got before its first instruction runs. Verified on podman 5.7.0: the
// state after init is initialized, the pid is not zero, the mounts are there,
// and a fixture whose entrypoint writes a marker writes nothing until start.
//
// The check at validation says what was true then, and podman resolves the
// source path itself, in its own process, afterwards: a source replaced by a
// symlink in between is followed by podman and the container gets whatever it
// pointed at. For a member of kitbash-admin that would be a rw mount of /org,
// which is the one thing the approval queue exists to be the trail of, so this
// refuses the start rather than reporting it after the fact.
//
// Two questions are asked, because one of them alone is not enough.
//
// The first is what podman was given: the source of each mount it reports
// against the source the registration holds. podman reports the path it was
// asked for and not what it resolved, verified against podman 5.7.0, so this
// catches a runtime that mounted something other than what kitbashd asked for
// and nothing else.
//
// The second is what the container actually has. /proc/<pid>/root resolves in
// the container's own mount namespace, so a stat of the target through it names
// the folder that is mounted there, whatever the path it came from is called
// now. Comparing its device and inode with the ones the validation opened is
// the ground truth: a source swapped for a link, and a source swapped and then
// swapped back, are both a different inode at the target. kitbashd is root, so
// it needs nothing inside the image to read this.
//
// A pid of zero is a container the runtime did not prepare, which is what a
// source that stopped existing between the create and the init leaves behind.
// It is refused with everything else: there is no namespace to read, so there
// is nothing this can say about it.
func (s *Server) verifyMounts(ctx context.Context, instance string, p store.Process, m sysusers.Member, expected []mounts.Resolved) *problem.Problem {
	if len(expected) == 0 {
		return nil
	}
	config, err := s.runner.ContainerConfig(ctx, m, p.Container)
	if err != nil {
		return problem.Internal(instance,
			fmt.Sprintf("reading back what %s was prepared with: %v", p.Container, err), "")
	}
	return s.verifyConfig(instance, p, config, expected)
}

// verifyConfig is verifyMounts against a configuration the caller has already
// read, which is what restore does: it asks the runtime once, for the state and
// the pid as well as the mounts.
func (s *Server) verifyConfig(instance string, p store.Process, config sysusers.ContainerConfig, expected []mounts.Resolved) *problem.Problem {
	if len(expected) == 0 {
		return nil
	}
	swapped := func(detail string) *problem.Problem {
		logger.Printf("processes: %s of %s: %s: %s", p.ID, p.Owner, MountSwapped, detail)
		return problem.NotPermitted(instance, MountSwapped, mountSwapFix)
	}
	mounted := make(map[string]podman.Mount, len(config.Mounts))
	for _, got := range config.Mounts {
		mounted[got.Target] = got
	}
	for _, want := range expected {
		got, ok := mounted[want.Target]
		if !ok {
			return swapped(fmt.Sprintf("%s carries no mount at %s", p.Container, want.Target))
		}
		if got.Source != want.Source {
			return swapped(fmt.Sprintf("%s is mounted at %s, and %s was validated", got.Source, want.Target, want.Source))
		}
		if got.ReadOnly != want.ReadOnly() {
			return swapped(fmt.Sprintf("%s is mounted at %s read only %t, and %s was validated",
				got.Source, want.Target, got.ReadOnly, want.Mode))
		}
	}
	if config.PID <= 0 {
		return swapped(fmt.Sprintf("%s has no process to read its mounts through", p.Container))
	}
	for _, want := range expected {
		device, inode, err := statIdentity(filepath.Join(s.procRoot, strconv.Itoa(config.PID), "root", want.Target))
		if err != nil {
			return swapped(fmt.Sprintf("%s cannot be read inside %s: %v", want.Target, p.Container, err))
		}
		if device != want.Device || inode != want.Inode {
			return swapped(fmt.Sprintf("%s holds another folder than the one %s was validated as", want.Target, want.Source))
		}
	}
	return nil
}

// statIdentity is the device and inode of one path, which is how two readings
// of a folder are compared.
func statIdentity(path string) (device, inode uint64, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("this host does not report what %s is", path)
	}
	return uint64(sys.Dev), sys.Ino, nil
}

// tearDownAfterSwap removes the container of a Process whose mounts did not
// check out. A container that was only prepared has run nothing, so this is
// what makes the refusal true: it is removed by force, and a removal that fails
// is logged rather than returned, because the answer to the caller is the
// refusal and not this.
func (s *Server) tearDownAfterSwap(ctx context.Context, p store.Process, m sysusers.Member) {
	if err := s.runner.RemoveContainer(ctx, m, p.Container, true); err != nil &&
		!errors.Is(err, sysusers.ErrNoContainer) {
		logger.Printf("processes: removing %s after its mount changed: %v", p.Container, err)
	}
}

// prepareAndVerify is the whole of what a start does between making a container
// and running it: the runtime prepares it, which is where the bind mounts are
// made, kitbashd reads them in the container's own namespace, and only a
// container that matches is handed back to be started. A container that does
// not match is removed here, having run nothing.
//
// A Process with no mounts is prepared too. It costs one call and it means one
// path rather than two, and the runtime does at start what it would have done
// here anyway.
func (s *Server) prepareAndVerify(ctx context.Context, instance string, p store.Process, m sysusers.Member,
	leaf string, expected []mounts.Resolved) *problem.Problem {
	if err := s.runner.InitContainer(ctx, m, p.Container, leaf); err != nil {
		if len(expected) == 0 {
			return problem.Internal(instance,
				fmt.Sprintf("the container runtime could not prepare %s: %v", p.Container, err), "")
		}
		// A source that stopped existing between the create and here is what
		// this failure usually is, and it is the same refusal: the container
		// has run nothing and it does not get to.
		logger.Printf("processes: %s of %s: %s: preparing %s: %v", p.ID, p.Owner, MountSwapped, p.Container, err)
		s.tearDownAfterSwap(ctx, p, m)
		return problem.NotPermitted(instance, MountSwapped, mountSwapFix)
	}
	if prob := s.verifyMounts(ctx, instance, p, m, expected); prob != nil {
		s.tearDownAfterSwap(ctx, p, m)
		return prob
	}
	return nil
}

// mountProblem is what restore records about a Process whose mount is no
// longer legal, in the two sentences proc_list carries. The detail is the
// member's own problem rather than the daemon's words for it: they wrote the
// manifest and they can move the folder back.
func mountProblem(prob *problem.Problem) restoreProblem {
	fix := prob.Fix
	if fix == "" {
		fix = "Fix the folder deploy.units[0].mounts names, then run the Package again."
	}
	return restoreProblem{
		Detail: "this Process declares a mount that is no longer legal, so kitbashd did not start it: " + prob.Detail,
		Fix:    fix,
	}
}
