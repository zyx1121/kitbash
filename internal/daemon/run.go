package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zyx1121/kitbash/internal/cgroups"
	"github.com/zyx1121/kitbash/internal/mounts"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// kitbashd is the supervisor of every Process: a member cannot place their own
// container in a delegated cgroup, so the session registers the Process and
// then asks the daemon to run it. What arrives here is the command line the
// session would have used; what the daemon adds is the environment, the cgroup
// and the member it all runs as, see PLAN.md section 2.3 and issue 69.

// The three actions a session asks of a registered Process, as the last
// segment of its path.
const (
	actionStart  = "start"
	actionStop   = "stop"
	actionRemove = "remove"
)

// The environment kitbashd puts in every Process, see
// environment_given_to_every_process in spec/kitbashd-api.yaml. internal/
// telemetry holds the reader's spelling of the same names; this package cannot
// import that one without pulling the OpenTelemetry SDK into the daemon, the
// same reason sysusers.EnvCaller is spelled twice.
const (
	EnvTelemetryEndpoint = "KITBASH_TELEMETRY_ENDPOINT"
	EnvTelemetryToken    = "KITBASH_TELEMETRY_TOKEN"
	EnvProcess           = "KITBASH_PROCESS"
	EnvPackage           = "KITBASH_PACKAGE"
	EnvUser              = "KITBASH_USER"
	EnvMCPEndpoint       = "KITBASH_MCP_ENDPOINT"
	EnvFanoutSecret      = "KITBASH_FANOUT_SECRET"
	// EnvUnit names the unit one container of a pod runs, given only to a
	// Process that runs as several. A Process of one unit is the Package
	// itself and its container is given exactly what it was given before pods
	// existed, see PLAN.md section 5.6.
	EnvUnit = "KITBASH_UNIT"
)

// ownedEnv is every variable kitbashd speaks for. A manifest that names one is
// not refused: the value is dropped and the daemon's own is written, because
// these are the Process's identity and its two credentials, not configuration.
var ownedEnv = []string{
	EnvTelemetryEndpoint,
	EnvTelemetryToken,
	EnvProcess,
	EnvPackage,
	EnvUser,
	EnvMCPEndpoint,
	EnvFanoutSecret,
	EnvUnit,
}

// DefaultProcessEndpoint is the address a rootless container reaches the
// Process receiver on: rootless networking delivers host.containers.internal
// to the host's primary address, so a Process exports over TCP with a token
// while a session exports over the socket.
const DefaultProcessEndpoint = "http://host.containers.internal:4318"

// DefaultEnvDir is where the environment file of one Process is written. It is
// 0711 root owned: a member opens the file of their own Process by name, which
// kitbashd chowns to them, and lists nothing.
const DefaultEnvDir = "/run/kitbash/env"

// The modes of that directory and of one environment file in it.
const (
	envDirMode  = 0o711
	envFileMode = 0o600
)

// DefaultPidsLimit is the number of processes one Process may hold when its
// manifest names none. The manifest has no pids field: the bound is kitbashd's,
// so a Package cannot fork the host to a standstill, and it is far above what
// a container that serves MCP over stdio needs.
const DefaultPidsLimit = 512

// MaxPidsLimit is the highest a request may ask for.
const MaxPidsLimit = 4096

// What one start request may carry. They are far above what a real unit
// declares and stop one caller from filling a command line with a field.
const (
	MaxEnvEntries   = 64
	MaxEnvKeyBytes  = 128
	MaxEnvBytes     = 4096
	MaxLabels       = 32
	MaxLabelBytes   = 512
	MaxPublishPorts = 8
	MaxCommandWords = 64
	MaxCommandBytes = 4096
)

// envKey is the shape of an environment variable name. It reaches a file
// podman parses, so a name with an equals sign or a line break in it is not
// one this daemon writes.
var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// startRequest is the processes_start input of spec/kitbashd-api.yaml: the
// command line the session would have run, without any of the environment
// kitbashd speaks for. The container and the image are the ones the
// registration already names; they are accepted here so a client can be
// explicit, and refused when they disagree.
//
// The Telemetry token and the fan out secret are not in it, and never will be:
// a request body is a thing that gets logged.
type startRequest struct {
	Container string            `json:"container,omitempty"`
	Image     string            `json:"image,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Restart   string            `json:"restart,omitempty"`
	CPU       string            `json:"cpu,omitempty"`
	Memory    string            `json:"memory,omitempty"`
	PidsLimit int               `json:"pidsLimit,omitempty"`
	Publish   []portMapping     `json:"publish,omitempty"`
	// Command is what the container runs in place of the command of its
	// image, as the unit declared it. It is the one field of a start request
	// that lands on podman's command line by design, because that is where
	// the arguments after an image go; nothing of the environment ever does.
	Command []string `json:"command,omitempty"`
	// Units is the command line of each unit of a Process that runs as a pod,
	// one per unit the registration names. A Process of one unit sends none
	// and is started by the fields above alone, exactly as it was before pods
	// existed, see pods.go.
	Units []startUnit `json:"units,omitempty"`
}

// portMapping is one published port. A host port of zero leaves the choice to
// the runtime, which is what a Process whose endpoint nobody had to know does.
type portMapping struct {
	HostPort      int `json:"hostPort,omitempty"`
	ContainerPort int `json:"containerPort"`
}

// startResponse is what a start answers: the Process it ran and the id the
// runtime gave the container.
type startResponse struct {
	ID          string `json:"id"`
	Container   string `json:"container"`
	ContainerID string `json:"containerId"`
}

// processAction answers the three verbs on one registered Process:
// /kitbash/v1/processes/{id}/start, /stop and /remove. Every one of them runs
// the container runtime as the owner, which is the whole point: the child is
// started inside the owner's cgroup leaf and the container is created under
// their delegated subtree.
func (s *Server) processAction(w http.ResponseWriter, r *http.Request, id, action string) {
	caller, prob := s.caller(r)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	if r.Method != http.MethodPost {
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s %s is not part of the kitbashd API", r.Method, r.URL.Path),
			fmt.Sprintf("Call POST %s instead.", r.URL.Path)))
		return
	}
	if prob := s.notRemoving(r, caller.User, "run a Process"); prob != nil {
		writeProblem(w, prob)
		return
	}
	p, m, prob := s.owned(r, caller, id)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	deadline := ActionDeadline
	if action == actionStart {
		deadline = StartDeadline
	}
	extendResponse(w, deadline)
	// One action at a time per Process. Two starts of one id would race on
	// its environment file and on its cgroup, and a start racing a remove
	// would leave a container the registry does not describe.
	unlock := s.actions.lock(id)
	defer unlock()
	switch action {
	case actionStart:
		s.startProcess(w, r, p, m)
	case actionStop:
		s.stopProcess(w, r, p, m)
	case actionRemove:
		s.removeProcess(w, r, p, m)
	}
}

// owned resolves one registered Process and the member who owns it. The owner
// is the peer and nobody else, admins included: these calls run a program as
// that member, which is not a thing to do on somebody else's behalf.
func (s *Server) owned(r *http.Request, caller Caller, id string) (store.Process, sysusers.Member, *problem.Problem) {
	p, found, err := s.store.Process(r.Context(), id)
	if err != nil {
		return store.Process{}, sysusers.Member{}, problem.Internal(r.URL.Path, err.Error(), "")
	}
	if !found {
		return store.Process{}, sysusers.Member{}, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("kitbashd has no Process %s registered", id),
			"Register the Process before you ask kitbashd to run it.")
	}
	if p.Owner != caller.User {
		return store.Process{}, sysusers.Member{}, problem.NotPermitted(r.URL.Path,
			fmt.Sprintf("the Process %s belongs to another member", id),
			"Run your own Processes; the member who registered this one runs it.")
	}
	if p.Runner != "" {
		// A Process a run kit owns runs wherever the kit put it, so there is
		// no container here for kitbashd to act on. Saying so is better than
		// the missing container name below, which reads like a client bug.
		return store.Process{}, sysusers.Member{}, problem.NotPermitted(r.URL.Path,
			fmt.Sprintf("the Process %s is owned by the run kit at %s, which kitbashd does not supervise", id, p.Runner),
			"Stop it with proc_stop, which forwards to the kit that runs it.")
	}
	if p.Container == "" {
		return store.Process{}, sysusers.Member{}, problem.BadRequest(r.URL.Path,
			fmt.Sprintf("the Process %s was registered without a container name", id),
			"Register the Process with the container name the runtime holds it under, then run it.")
	}
	m, found, err := s.users.Lookup(r.Context(), p.Owner)
	if err != nil {
		return store.Process{}, sysusers.Member{}, problem.Internal(r.URL.Path, err.Error(), "")
	}
	if !found {
		return store.Process{}, sysusers.Member{}, problem.Internal(r.URL.Path,
			fmt.Sprintf("%s owns this Process and is not a member of this host", p.Owner), "")
	}
	return p, m, nil
}

// startProcess runs the container of one registered Process as its owner,
// inside their cgroup leaf and under their delegated subtree, which is what
// makes the manifest's limits an enforcement, see internal/cgroups.
func (s *Server) startProcess(w http.ResponseWriter, r *http.Request, p store.Process, m sysusers.Member) {
	// A job is started by its ticks and by nothing else. A session that asked
	// for one would run the container at a time nobody declared, and the run
	// would be recorded as a tick that never came, see schedule.go.
	if p.Schedule.Declared() {
		writeProblem(w, problem.NotPermitted(r.URL.Path,
			fmt.Sprintf("the Process %s is a job, and kitbashd starts a job at its schedule", p.ID),
			"Wait for the next tick, or remove deploy.units[0].schedule from this Package and run it again."))
		return
	}
	var req startRequest
	if prob := decodeBody(w, r, &req); prob != nil {
		writeProblem(w, prob)
		return
	}
	// A Package of more than one unit runs as one pod. Everything below is
	// the single unit path and is untouched by it: the pod start makes the
	// same four steps per unit, see pods.go.
	if p.Composition.Declared() {
		s.startPod(w, r, p, m, req)
		return
	}
	if len(req.Units) > 0 {
		writeProblem(w, problem.BadRequest(r.URL.Path,
			"this Process is registered as one container and the start carries units",
			"Register the Process with the units it runs, then start it."))
		return
	}
	opts, prob := runOptions(r.URL.Path, p, req)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	// The mounts of the registration are resolved again, before anything is
	// created: the check at registration says what was true then, and a folder
	// can be removed, replaced by a symlink or lose its manifest in between.
	// The container is not created when one of them no longer checks out, see
	// mounts.go.
	mounted, prob := s.revalidateMounts(r.URL.Path, p, m)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	opts.Mounts = mounts.Podman(mounted)

	// The secrets of the registration are resolved here, before the container
	// is created: a declared name the owner has not set is not-found, and a
	// Process missing its credential is refused rather than started without
	// it. They are resolved again at every start, so the value this container
	// is given is the owner's current one, see secrets.go.
	held, prob := s.resolveSecrets(r.URL.Path, p)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	// And the name the unit declared is proved again, for the same reason a
	// secret is resolved again: what it points at is a fact about the world
	// and not part of the registration. A name that stopped pointing here is a
	// name this host would serve for nobody and, in acme mode, ask a
	// certificate authority for, so the start is refused rather than made and
	// the owner reads why, see hostnames.go.
	if prob := s.proveHostname(r.Context(), r.URL.Path, p); prob != nil {
		writeProblem(w, prob)
		return
	}

	// The Process gets a cgroup of its own, with its ceiling written by root,
	// and the container is created under it. A host that cannot place it runs
	// the Process anyway: the limits are recorded, not enforced, and that is
	// said on this start rather than once for the life of the daemon.
	leaf := s.processCgroup(r.Context(), m, p, limitsOf(opts.Memory, opts.CPUs, opts.PidsLimit))
	if leaf != "" {
		opts.CgroupParent = cgroups.Parent(m.Name, p.ID)
	}

	// The token is minted here and not at registration: the store keeps only
	// the hash of a token, so the one the registration answered cannot be
	// read back, and the container is the only thing that needs this one.
	// Minting again revokes the previous one, which nothing holds. The
	// ceiling is written with it: after a reboot the cgroup filesystem is
	// empty and the registration is the only thing that remembers.
	// Ceiling says the cgroup was made as well as recorded: a restore reads it
	// to tell a Process it has already placed from one registered before
	// kitbashd wrote a ceiling per Process, whose container names its member's
	// cgroup as its parent and cannot start under it, see restore.go.
	p.Limits = store.Limits{Memory: req.Memory, CPU: req.CPU, Pids: opts.PidsLimit, Ceiling: leaf != ""}
	token, err := s.mintToken(r.Context(), p)
	if err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	envFile, err := s.writeEnvFile(p, m, req.Env, token, held)
	if err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	// The file is gone before this answers. The container keeps the
	// environment podman read out of it, and a file holding a live token does
	// not outlive the call that wrote it.
	defer func() {
		if err := os.Remove(envFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Printf("processes: removing the environment file of %s: %v", p.ID, err)
		}
	}()
	opts.EnvFile = envFile

	// The container is made, prepared and only then started. Preparing it is
	// where the runtime makes its bind mounts, and between that and the start
	// kitbashd reads what the container actually got, in its own mount
	// namespace, before the image's entrypoint has executed anything. A
	// source replaced between the check above and podman resolving it is
	// refused there, and the container is removed having run nothing, see
	// mounts.go.
	id, err := s.runner.CreateContainer(r.Context(), m, opts, leaf)
	if err != nil {
		writeProblem(w, s.runProblem(r, err, p, opts))
		return
	}
	if prob := s.prepareAndVerify(r.Context(), r.URL.Path, p, m, leaf, mounted); prob != nil {
		writeProblem(w, prob)
		return
	}
	if err := s.runner.Start(r.Context(), m, p.Container, leaf); err != nil {
		// The container is prepared and was not started, so it is left in the
		// runtime holding a mount namespace and running nothing. Removing it
		// is what keeps a failed start from leaving a Process that proc_list
		// reports as starting for ever, and what makes the next attempt begin
		// from a container this daemon made, see restoreMounted.
		s.tearDownAfterSwap(r.Context(), p, m)
		writeProblem(w, s.runProblem(r, err, p, opts))
		return
	}
	// The Process is running, so whatever the last boot could not do for it is
	// over and proc_list stops reporting it.
	s.clearProcessProblem(p.ID)
	logger.Printf("processes: %s started %s for %s in %s", p.ID, p.Container, m.Name, cgroupOrNone(leaf))
	// The container exists now, so this is where a Process registered before
	// it did gets its probe checked and started, see verifyProbe. The start
	// itself is not refused by a declaration that does not check out: the
	// Process is running, and what a failed check costs is the probe. The
	// registration that declared it was refused to the member's face when the
	// container was already there, and is refused again the next time they
	// run this Process.
	if prob := s.trackProbeAs(r.Context(), m, p, r.URL.Path); prob != nil {
		logger.Printf("health: not probing %s of %s: %s", p.ID, p.Owner, prob.Detail)
	}
	// And this is where the name this Process is served under gets the port to
	// forward to: the container publishes one now, so the routing table reads
	// it off the runtime rather than waiting for the next restore, see
	// proxy.go.
	s.trackRoute(r.Context(), m, p)
	writeJSON(w, r.URL.Path, startResponse{ID: p.ID, Container: p.Container, ContainerID: id})
}

// stopProcess stops the container of one Process as its owner, giving it the
// same seconds proc_stop publishes. The registration is not touched: the
// session unregisters, which is what revokes the token.
func (s *Server) stopProcess(w http.ResponseWriter, r *http.Request, p store.Process, m sysusers.Member) {
	// A Process of several units is one pod, so it is stopped as one: every
	// container in it is given the same grace, and none of them is left
	// running beside a face that is down.
	//
	// And the pod goes with the stop, which is where a pod differs from the
	// single container a Package of one unit is. A stopped container keeps
	// its name for proc_logs and the next run of that Package removes it by
	// name; a stopped pod keeps its name and its published port as well, and
	// podman refuses to create a pod under a name that is taken, so a pod
	// left behind is a Package that cannot be run again. The containers go
	// with it, see PLAN.md section 5.6.
	if p.Composition.Declared() {
		if err := s.runner.StopPod(r.Context(), m, p.Composition.Pod, StopTimeout); err != nil && !isNoPod(err) {
			writeProblem(w, s.runProblem(r, err, p, podman.RunOptions{}))
			return
		}
		if err := s.runner.RemovePod(r.Context(), m, p.Composition.Pod, true); err != nil && !isNoPod(err) {
			writeProblem(w, s.runProblem(r, err, p, podman.RunOptions{}))
			return
		}
		s.trackRoute(r.Context(), m, p)
		writeJSON(w, r.URL.Path, startResponse{ID: p.ID, Container: p.Container})
		return
	}
	if err := s.runner.Stop(r.Context(), m, p.Container, StopTimeout); err != nil {
		writeProblem(w, s.runProblem(r, err, p, podman.RunOptions{}))
		return
	}
	// The registration stands, so the name stands with it and answers 503:
	// what a member reads at the address of a Process they stopped is that it
	// is not running, not that the address is nobody's, see proxy.go.
	s.trackRoute(r.Context(), m, p)
	writeJSON(w, r.URL.Path, startResponse{ID: p.ID, Container: p.Container})
}

// removeProcess removes the container of one Process as its owner, which is
// what a run that replaces a Process does to the one it takes the place of.
func (s *Server) removeProcess(w http.ResponseWriter, r *http.Request, p store.Process, m sysusers.Member) {
	// The pod and every container in it, for a Process that runs as one: a
	// pod left behind is a name the next run of this Process cannot take and
	// a published port nothing answers on.
	if p.Composition.Declared() {
		if err := s.runner.RemovePod(r.Context(), m, p.Composition.Pod, true); err != nil && !isNoPod(err) {
			writeProblem(w, s.runProblem(r, err, p, podman.RunOptions{}))
			return
		}
	} else if err := s.runner.RemoveContainer(r.Context(), m, p.Container, true); err != nil {
		writeProblem(w, s.runProblem(r, err, p, podman.RunOptions{}))
		return
	}
	// The cgroup goes with the container. One that is still busy is a
	// container the runtime has not finished tearing down; it is left for the
	// next removal or the next boot rather than waited on here.
	if err := s.cgroups.RemoveProcess(r.Context(), m.Name, p.ID); err != nil {
		logger.Printf("cgroups: the cgroup of %s could not be removed: %v", p.ID, err)
	}
	// There is no container to forward to any more. The registration may still
	// be there for a moment, because the session unregisters after this, so
	// the name is dropped here rather than left pointing at nothing.
	s.proxy.untrack(p.ID)
	writeJSON(w, r.URL.Path, startResponse{ID: p.ID, Container: p.Container})
}

// StopTimeout is how long a Process is given to exit on its own, the same ten
// seconds spec/mcp-surface.yaml publishes for proc_stop.
const StopTimeout = 10

// The response deadline one action gets. A container start is minutes of work
// in the worst case and a stop waits out the whole grace of a PID 1 that
// ignores SIGTERM, so the write timeout every other path serves under is
// extended for these three. Both are wider than the budget the runner gives
// the child, so the daemon answers before the deadline rather than at it.
const (
	StartDeadline  = 180 * time.Second
	ActionDeadline = 90 * time.Second
)

// sessionResponse is what a join answers: who the session belongs to, which
// process was placed and the cgroup it went into.
type sessionResponse struct {
	User   string `json:"user"`
	PID    int    `json:"pid"`
	Cgroup string `json:"cgroup"`
}

// joinSession puts the calling session in its member's cgroup leaf, which is
// what lets it exec into its own containers: moving a process between two
// cgroups needs write access to the common ancestor's cgroup.procs, and a
// session that sshd started shares only the root cgroup with anything of
// kitbash's, see internal/cgroups.
//
// The process placed is the peer of this connection, read from the kernel. A
// caller cannot name another one: that is the same rule the whole socket
// follows, and here it would move somebody else's process into a cgroup.
func (s *Server) joinSession(w http.ResponseWriter, r *http.Request) {
	caller, prob := s.caller(r)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	if caller.Peer.PID <= 0 {
		writeProblem(w, problem.Internal(r.URL.Path,
			fmt.Sprintf("the connection of %s carries no process id", caller.User), ""))
		return
	}
	// A session placed in the cgroup of a member being removed is a session
	// in a tree the removal is about to take away, see removal.go.
	if prob := s.notRemoving(r, caller.User, "open a session"); prob != nil {
		writeProblem(w, prob)
		return
	}
	m, found, err := s.users.Lookup(r.Context(), caller.User)
	if err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	if !found || !m.IsMember() {
		writeProblem(w, problem.NotPermitted(r.URL.Path,
			fmt.Sprintf("%s is not a member of this host", caller.User),
			"Ask an administrator to create a member for this account."))
		return
	}
	cgroup, err := s.cgroups.JoinSession(r.Context(), m.Name, m.UID, m.GID, int(caller.Peer.PID))
	if errors.Is(err, cgroups.ErrNotOwned) {
		// The process that opened this connection is gone, or the pid names
		// somebody else's by now. Either way it is not this member's to move.
		logger.Printf("cgroups: the session %d of %s was not placed: %v", caller.Peer.PID, m.Name, err)
		writeProblem(w, problem.NotPermitted(r.URL.Path,
			fmt.Sprintf("the process %d is not %s's to place", caller.Peer.PID, m.Name),
			"Open a new session."))
		return
	}
	if err != nil {
		// A session that was not placed still works: everything but exec into
		// a container kitbashd started, which says so when it is tried.
		logger.Printf("cgroups: the session %d of %s was not placed: %v", caller.Peer.PID, m.Name, err)
		writeProblem(w, problem.InternalDetail(r.URL.Path,
			fmt.Sprintf("place the session %d of %s: %v", caller.Peer.PID, m.Name, err),
			"this host could not place the session in its member's cgroup",
			"Your session works; a Process you exec into may not. Ask an administrator to read the daemon log."))
		return
	}
	writeJSON(w, r.URL.Path, sessionResponse{User: m.Name, PID: int(caller.Peer.PID), Cgroup: cgroup})
}

// extendResponse gives one response longer than the write timeout the daemon
// serves every other path under. A server that cannot extend it is not a
// reason to refuse the call: the work still happens and the answer is late,
// which is said in the log rather than to the caller.
func extendResponse(w http.ResponseWriter, deadline time.Duration) {
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(deadline)); err != nil {
		logger.Printf("processes: the response deadline could not be extended to %s: %v", deadline, err)
	}
}

// runProblem turns a runtime failure into the answer the agent reads. The
// runtime's own output is in the error and goes to the daemon log only: it
// carries host paths, and on a run it names the environment file.
func (s *Server) runProblem(r *http.Request, err error, p store.Process, opts podman.RunOptions) *problem.Problem {
	logger.Printf("processes: %s of %s: %v", p.Container, p.Owner, err)
	switch {
	case errors.Is(err, sysusers.ErrNoImage):
		return problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s has no image %s", p.Owner, opts.Image),
			"Call pkg_build for this Package, then run the digest it returns.")
	case errors.Is(err, sysusers.ErrNoContainer):
		return problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s has no container %s", p.Owner, p.Container),
			"Call proc_run to start this Process again.")
	case errors.Is(err, sysusers.ErrTimeout):
		// The runtime was still working when its budget ran out, so what
		// happened to the container is not known here. Saying so is the whole
		// answer: nothing about the Package is wrong and running it again is
		// safe, which is the invariant of PLAN.md 2.6.
		return problem.InternalDetail(r.URL.Path,
			fmt.Sprintf("%s of %s: %v", p.Container, p.Owner, err),
			"the container runtime did not answer in time",
			"Call proc_list to see what state the Process is in, then run or stop it again.")
	case errors.Is(err, sysusers.ErrUsage):
		// Exit 125 is podman refusing to parse the command at all, and every
		// part of it that a caller chose comes from the unit.
		return problem.BadRequest(r.URL.Path,
			"the container runtime refused the options of this unit",
			"Check deploy.units[0]: its limits, restart policy, ports and environment are what this command line is made of.")
	default:
		return problem.Internal(r.URL.Path,
			fmt.Sprintf("the container runtime could not run %s", p.Container), "")
	}
}

// runOptions turns one start request into the command line, holding it to the
// registration: the container name and the image are the ones kitbashd
// recorded, and the identity labels are written here rather than taken from
// the body, so the container's labels and the registry cannot disagree.
func runOptions(instance string, p store.Process, req startRequest) (podman.RunOptions, *problem.Problem) {
	refuse := func(detail, fix string) (podman.RunOptions, *problem.Problem) {
		return podman.RunOptions{}, problem.BadRequest(instance, detail, fix)
	}
	if req.Container != "" && req.Container != p.Container {
		return refuse(fmt.Sprintf("this Process is registered as the container %s, not %s", p.Container, req.Container),
			"Send the container name the Process was registered with, or none at all.")
	}
	image := p.Digest
	if req.Image != "" && req.Image != image {
		// The registration names the image this Process runs. A request that
		// named another one would start something the registry does not
		// describe, and the registry is what an admin reads.
		return refuse(fmt.Sprintf("this Process is registered to run %s, not %s", image, req.Image),
			"Register the Process with the digest you want to run, then start it.")
	}
	if image == "" {
		return refuse("this Process is registered without an image digest",
			"Register the Process with the digest it was built to, then run it.")
	}

	opts := podman.RunOptions{
		Name:  p.Container,
		Image: image,
		// A Process is PID 1 of its container with stdin held open, which is
		// what keeps a stdio MCP server alive between sessions.
		Detach:      true,
		Interactive: true,
		PidsLimit:   DefaultPidsLimit,
	}
	if req.Memory != "" {
		opts.Memory = podman.MemoryLimit(req.Memory)
		if !podman.ValidMemory(opts.Memory) {
			return refuse(fmt.Sprintf("limits.memory %q is not a size the container runtime takes", req.Memory),
				"Write limits.memory as a number with Ki, Mi or Gi, such as 512Mi.")
		}
	}
	if req.CPU != "" {
		if !podman.ValidCPUs(req.CPU) {
			return refuse(fmt.Sprintf("limits.cpu %q is not a number of cores", req.CPU),
				"Write limits.cpu as a number, such as 1 or 0.5.")
		}
		opts.CPUs = req.CPU
	}
	if req.Restart != "" {
		opts.Restart = req.Restart
		if !podman.ValidRestart(opts.Restart) {
			return refuse(fmt.Sprintf("restart %q is not a policy the container runtime takes", req.Restart),
				"Write restart as always, on-failure or never.")
		}
	}
	if req.PidsLimit != 0 {
		if req.PidsLimit < 1 || req.PidsLimit > MaxPidsLimit {
			return refuse(fmt.Sprintf("a Process may hold between 1 and %d processes, not %d", MaxPidsLimit, req.PidsLimit),
				fmt.Sprintf("Ask for between 1 and %d, or none and take the default of %d.", MaxPidsLimit, DefaultPidsLimit))
		}
		opts.PidsLimit = req.PidsLimit
	}
	if len(req.Publish) > MaxPublishPorts {
		return refuse(fmt.Sprintf("a Process may publish at most %d ports", MaxPublishPorts),
			"Publish the one port the unit listens on.")
	}
	for _, port := range req.Publish {
		if port.ContainerPort < 1 || port.ContainerPort > 65535 ||
			port.HostPort < 0 || port.HostPort > 65535 {
			return refuse(fmt.Sprintf("%d:%d is not a port mapping", port.HostPort, port.ContainerPort),
				"Send the container port the unit listens on, and a host port between 1 and 65535 or none.")
		}
		opts.Publish = append(opts.Publish, podman.PortMapping{
			HostPort: port.HostPort, ContainerPort: port.ContainerPort,
		})
	}
	labels, prob := labelsOf(instance, p, req.Labels)
	if prob != nil {
		return podman.RunOptions{}, prob
	}
	opts.Labels = labels
	if prob := checkEnv(instance, req.Env); prob != nil {
		return podman.RunOptions{}, prob
	}
	if prob := checkCommand(instance, req.Command); prob != nil {
		return podman.RunOptions{}, prob
	}
	opts.Command = req.Command
	return opts, nil
}

// checkCommand holds the command a unit declared to a shape that can be
// written after an image on a command line. It is bounded for the reason the
// environment is: a request body is not a place to build an arbitrarily long
// argument list out of.
func checkCommand(instance string, command []string) *problem.Problem {
	if len(command) > MaxCommandWords {
		return problem.BadRequest(instance,
			fmt.Sprintf("a unit may declare at most %d words of command, not %d", MaxCommandWords, len(command)),
			"Put a long command in the image, and name it in deploy.units[].command in a few words.")
	}
	for _, word := range command {
		if word == "" {
			return problem.BadRequest(instance,
				"a word of the command is empty, which is an argument the container cannot be given",
				"Write every word of deploy.units[].command.")
		}
		if len(word) > MaxCommandBytes {
			return problem.BadRequest(instance,
				fmt.Sprintf("a word of the command is longer than the %d bytes this API carries", MaxCommandBytes),
				"Give the Process a file to read instead of a long argument.")
		}
	}
	return nil
}

// labelsOf is the whole Process record as it goes onto the container. The
// caller's labels are kept, and the six that name the Process are written from
// the registration: the container list is the Process record, so it says what
// kitbashd recorded and not what a request body claimed.
func labelsOf(instance string, p store.Process, given map[string]string) (map[string]string, *problem.Problem) {
	if len(given) > MaxLabels {
		return nil, problem.BadRequest(instance,
			fmt.Sprintf("a Process may carry at most %d labels", MaxLabels),
			"Send the labels of the Process and nothing else.")
	}
	labels := make(map[string]string, len(given)+6)
	for k, v := range given {
		if len(k) > MaxLabelBytes || len(v) > MaxLabelBytes {
			return nil, problem.BadRequest(instance,
				fmt.Sprintf("the label %q is longer than the %d bytes this API carries", k, MaxLabelBytes),
				"Send shorter labels.")
		}
		if strings.ContainsAny(k, "= \n\r") {
			return nil, problem.BadRequest(instance,
				fmt.Sprintf("%q is not a label name", k),
				"Send label names without spaces, equals signs or line breaks.")
		}
		labels[k] = v
	}
	labels[podman.LabelID] = p.ID
	labels[podman.LabelUser] = p.Owner
	labels[podman.LabelPackage] = p.Package
	labels[podman.LabelName] = p.Name
	labels[podman.LabelExpose] = p.Expose
	labels[podman.LabelDigest] = p.Digest
	// The endpoint is the registration's too: it is what the fan out delivers
	// to, so a container may not claim one of its own.
	delete(labels, podman.LabelEndpoint)
	if p.Endpoint != "" {
		labels[podman.LabelEndpoint] = p.Endpoint
	}
	return labels, nil
}

// checkEnv holds the manifest's own environment to a shape that can be written
// as KEY=value lines. The variables kitbashd speaks for are not refused here;
// they are dropped when the file is written.
func checkEnv(instance string, env map[string]string) *problem.Problem {
	if len(env) > MaxEnvEntries {
		return problem.BadRequest(instance,
			fmt.Sprintf("a unit may declare at most %d environment variables", MaxEnvEntries),
			"Declare fewer variables in deploy.units[0].environment.")
	}
	for k, v := range env {
		if len(k) > MaxEnvKeyBytes || !envKey.MatchString(k) {
			return problem.BadRequest(instance,
				fmt.Sprintf("%q is not an environment variable name", k),
				"Name variables with letters, digits and underscores, starting with a letter or an underscore.")
		}
		if len(v) > MaxEnvBytes {
			return problem.BadRequest(instance,
				fmt.Sprintf("the value of %s is longer than the %d bytes this API carries", k, MaxEnvBytes),
				"Give the Process a file to read instead of a long variable.")
		}
		if strings.ContainsAny(v, "\n\r") {
			// An env file is a list of KEY=value lines, so a value with a line
			// break has no spelling in one, and kitbashd puts nothing of a
			// Process's environment on a command line.
			return problem.BadRequest(instance,
				fmt.Sprintf("the value of %s carries a line break", k),
				"Give the Process a file to read instead of a variable with line breaks.")
		}
	}
	return nil
}

// mintToken gives the Process a new Telemetry token and records its hash. The
// store keeps hashes only, so the token a registration answered cannot be read
// back here; minting again revokes it, and nothing but this container was ever
// going to use it.
func (s *Server) mintToken(ctx context.Context, p store.Process) (string, error) {
	token, hash, err := store.NewToken()
	if err != nil {
		return "", err
	}
	// The record is written back as it was read, so nothing but the token
	// hash changes: the limit is not applied, because replacing a Process is
	// never a new one.
	if err := s.store.RegisterProcess(ctx, p, hash, store.Quota{}); err != nil {
		return "", err
	}
	return token, nil
}

// writeEnvFile writes the environment one container is started with, into a
// file the member can read and nobody else can. It is the member's file and
// not the daemon's: podman runs as them, and a root owned file in a root owned
// directory is one their runtime cannot open.
//
// The file is named by the Process id, so two runs of one Process do not race
// and two Processes never share a name. The caller removes it once the run has
// returned.
func (s *Server) writeEnvFile(p store.Process, m sysusers.Member, env map[string]string, token string,
	held map[string]string) (string, error) {
	return s.writeUnitEnvFile(p, m, "", env, token, held)
}

// writeUnitEnvFile is writeEnvFile for one unit of a pod. The file is named by
// the Process id and the unit, so the units of one Process do not race on one
// file the way two starts of one Process would. An empty unit is the Process
// of one unit, whose file is named by the id alone, exactly as before.
func (s *Server) writeUnitEnvFile(p store.Process, m sysusers.Member, unit string, env map[string]string,
	token string, held map[string]string) (string, error) {
	dir := s.envDir
	if err := os.MkdirAll(dir, envDirMode); err != nil {
		return "", fmt.Errorf("daemon: the environment directory %s: %w", dir, err)
	}
	// A directory an upgrade left narrower than this would stop every member
	// from opening their own file, so the mode is set rather than assumed.
	if err := os.Chmod(dir, envDirMode); err != nil {
		return "", fmt.Errorf("daemon: the environment directory %s: %w", dir, err)
	}
	name := p.ID
	if unit != "" {
		// A Process id is a UUID and carries no dot, so this names one unit of
		// one Process and can be nothing else's file.
		name = p.ID + "." + unit
	}
	path := filepath.Join(dir, name)
	// Whatever a previous run left is removed rather than truncated: the file
	// is opened exclusively so nothing on the host can have prepared it.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("daemon: the environment file %s: %w", path, err)
	}
	body, _ := podman.EnvFileBody(s.unitEnvironment(p, unit, env, token, held))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, envFileMode)
	if err != nil {
		return "", fmt.Errorf("daemon: the environment file %s: %w", path, err)
	}
	defer f.Close()
	if err := f.Chown(m.UID, m.GID); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("daemon: the environment file %s: %w", path, err)
	}
	if _, err := f.WriteString(body); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("daemon: the environment file %s: %w", path, err)
	}
	return path, nil
}

// environment is what one container is started with: the manifest's own, minus
// everything kitbashd speaks for, plus the secrets its owner holds for the
// names the unit declared, plus the Process's identity and its two
// credentials. Nothing here reaches a command line.
//
// held is what resolveSecrets answered for this start. The values go in after
// the manifest's own environment and before kitbashd's own: a manifest may
// declare neither a name kitbashd speaks for nor one its own env sets, and the
// order here is what makes that true of a registration written before the rule
// existed as well.
func (s *Server) environment(p store.Process, env map[string]string, token string,
	held map[string]string) map[string]string {
	return s.unitEnvironment(p, "", env, token, held)
}

// unitEnvironment is environment with the unit's name added, for a Process
// that runs as a pod. KITBASH_UNIT is what tells one container of a Process
// from another, which is what a record it exports carries and what kitbashd
// holds it to, see PLAN.md section 2.4. A Process of one unit is given none:
// there is one container and the Process names it.
func (s *Server) unitEnvironment(p store.Process, unit string, env map[string]string, token string,
	held map[string]string) map[string]string {
	merged := make(map[string]string, len(env)+len(held)+len(ownedEnv))
	for k, v := range env {
		merged[k] = v
	}
	// The secrets are the member's own values for the names the unit declared,
	// read from root owned files a moment ago. They are written into this file
	// and nowhere else: not onto a command line, not into a request body and
	// not into the span this start records, see PLAN.md section 2.3.
	for name, value := range held {
		merged[name] = value
	}
	// The removal happens before anything is added back, and after the secrets
	// as well as after the manifest's own environment. A manifest that named
	// its own fan out secret would otherwise keep it, and a container that
	// knows its own secret takes records from whoever wrote the manifest
	// rather than from kitbashd alone.
	for _, key := range ownedEnv {
		delete(merged, key)
	}
	merged[EnvTelemetryEndpoint] = s.processEndpoint()
	merged[EnvTelemetryToken] = token
	merged[EnvProcess] = p.ID
	merged[EnvPackage] = p.Package
	merged[EnvUser] = p.Owner
	// The MCP endpoint is given with the token, not before it: without a token
	// there is nothing for a Process to authenticate a session with.
	merged[EnvMCPEndpoint] = strings.TrimSuffix(s.processEndpoint(), "/") + MCPPath
	if p.FanoutSecret != "" {
		merged[EnvFanoutSecret] = p.FanoutSecret
	}
	if unit != "" {
		merged[EnvUnit] = unit
	}
	return merged
}

// processEndpoint is the address this host gives its Processes.
func (s *Server) processEndpoint() string {
	if s.endpoint != "" {
		return s.endpoint
	}
	return DefaultProcessEndpoint
}

// Prepare is what the daemon runs at start: the cgroup tree, one subtree per
// member of the host, and the environment directory swept of whatever a daemon
// that stopped mid start left behind.
func (s *Server) Prepare(ctx context.Context) {
	s.sweepEnvDir()
	// The secrets directory is the daemon's own, root owned and 0700. It is
	// created here rather than by the installer so a host upgraded from a
	// release without secrets gains it at the next start, the same way the
	// store directory is created and narrowed, see internal/secrets. cmd/
	// kitbashd prepares it before it serves anything and refuses to start when
	// it cannot, so what this catches is a tree that changed under a running
	// daemon; every secrets call answers internal until it is put back.
	if err := s.secrets.Prepare(); err != nil {
		logger.Printf("secrets: the directory %s could not be prepared, so every secrets call will fail: %v",
			s.secrets.Dir(), err)
	}
	if err := s.cgroups.EnsureRoot(ctx); err != nil {
		logger.Printf("cgroups: the kitbash cgroup could not be prepared, so the limits of every Process are recorded and not enforced: %v", err)
		return
	}
	members, err := s.users.List(ctx)
	if err != nil {
		logger.Printf("cgroups: the members could not be listed: %v", err)
		return
	}
	for _, m := range members {
		if _, err := s.cgroups.EnsureMember(ctx, m.Name, m.UID, m.GID); err != nil {
			logger.Printf("cgroups: the cgroup of %s could not be prepared: %v", m.Name, err)
		}
	}
}

// sweepEnvDir removes the environment files of starts that did not finish. A
// file there holds a Telemetry token and is removed by the start that wrote
// it; one that survived a daemon that was killed mid start would otherwise sit
// on the host until that Process is run again.
func (s *Server) sweepEnvDir() {
	entries, err := os.ReadDir(s.envDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logger.Printf("processes: the environment directory %s could not be read: %v", s.envDir, err)
		}
		return
	}
	swept := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(s.envDir, entry.Name())); err != nil {
			logger.Printf("processes: %s could not be removed: %v", entry.Name(), err)
			continue
		}
		swept++
	}
	if swept > 0 {
		logger.Printf("processes: swept %d environment file(s) of starts that did not finish", swept)
	}
}

// memberCgroup creates one member's cgroup, which is what an account gains
// when it is created and loses when it goes.
func (s *Server) memberCgroup(ctx context.Context, m sysusers.Member) {
	if _, err := s.cgroups.EnsureMember(ctx, m.Name, m.UID, m.GID); err != nil {
		logger.Printf("cgroups: the cgroup of %s could not be prepared: %v", m.Name, err)
	}
}

// processCgroup creates the ceiling one Process runs under and answers the
// member's leaf, which is where its podman child starts: the child is not the
// workload and must not spend the workload's memory.
//
// An empty leaf is a host that could not place it: the Process still runs, its
// limits are recorded rather than enforced, and this says so on every start
// rather than once, because the next start may be on a host that has since
// been fixed, or for a member whose cgroup is fine.
func (s *Server) processCgroup(ctx context.Context, m sysusers.Member, p store.Process, limits cgroups.Limits) string {
	leaf, err := s.cgroups.EnsureProcess(ctx, m.Name, p.ID, m.UID, m.GID, limits)
	if err != nil {
		logger.Printf("cgroups: %s runs without a cgroup of its own, so its limits are recorded and not enforced: %v",
			p.ID, err)
		return ""
	}
	return leaf
}

// limitsOf is the ceiling of one Process in the spelling the cgroup files
// take. It is the one conversion: a start reads it off the command line that
// was built for the container, and restore reads it off the registration, so
// the ceiling after a reboot is the ceiling the Process was started with.
//
// A limit that has no cgroup spelling is left out rather than guessed at; the
// runtime still gets what the unit declared.
func limitsOf(memory, cpu string, pids int) cgroups.Limits {
	var limits cgroups.Limits
	if value, ok := cgroups.MemoryMax(memory); ok {
		limits.Memory = value
	}
	if value, ok := cgroups.CPUMax(cpu); ok {
		limits.CPU = value
	}
	limits.Pids = pids
	return limits
}

// actionLock serialises the actions of one Process. It is a lock per id rather
// than one for the daemon: two members starting two Processes have nothing to
// wait for from each other.
type actionLock struct {
	mu    sync.Mutex
	held  map[string]*sync.Mutex
	waits map[string]int
}

func newActionLock() *actionLock {
	return &actionLock{held: map[string]*sync.Mutex{}, waits: map[string]int{}}
}

// lock takes the lock of one id and answers what releases it. The entry is
// dropped once nobody is waiting for it, so a daemon that has served a million
// Processes holds a map of the ones running now.
func (a *actionLock) lock(id string) func() {
	a.mu.Lock()
	lock, held := a.held[id]
	if !held {
		lock = &sync.Mutex{}
		a.held[id] = lock
	}
	a.waits[id]++
	a.mu.Unlock()

	lock.Lock()
	return func() {
		lock.Unlock()
		a.mu.Lock()
		a.waits[id]--
		if a.waits[id] <= 0 {
			delete(a.waits, id)
			delete(a.held, id)
		}
		a.mu.Unlock()
	}
}

// cgroupOrNone renders a leaf for the log.
func cgroupOrNone(leaf string) string {
	if leaf == "" {
		return "no cgroup of its own"
	}
	return leaf
}
