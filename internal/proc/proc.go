// Package proc implements the proc tool family from spec/mcp-surface.yaml: the
// Processes object, a Package running as a rootless container under the
// caller. It holds no MCP types, the way internal/fs holds none.
//
// The container runtime holds the Process state and this package reads it
// back. There is no second record, see PLAN.md section 2.3.
package proc

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/mounts"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// States the surface publishes, see the proc_run output schema.
const (
	StateStarting  = "starting"
	StateRunning   = "running"
	StateUnhealthy = "unhealthy"
	StateStopped   = "stopped"
	StateFailed    = "failed"
)

// StopTimeout is how long a Process is given to exit on its own.
const StopTimeout = 10

// DefaultLogLines and MaxLogLines match spec/mcp-surface.yaml.
const (
	DefaultLogLines = 200
	MaxLogLines     = 5000
)

// Process is one running or stopped Package.
type Process struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Package   string   `json:"package"`
	Digest    string   `json:"digest"`
	State     string   `json:"state"`
	Expose    string   `json:"expose,omitempty"`
	Endpoint  string   `json:"endpoint,omitempty"`
	StartedAt string   `json:"startedAt,omitempty"`
	Tools     []string `json:"tools,omitempty"`
	// Problem is why a Process that is not running did not come back, as
	// kitbashd reported it, and Fix what its owner can do about it. They are
	// empty for every Process kitbashd has nothing to say about. A container
	// the boot restore could not start sits in the runtime looking like one on
	// its way up, so without these proc_list would call it starting forever,
	// see restore in spec/kitbashd-api.yaml.
	Problem string `json:"problem,omitempty"`
	Fix     string `json:"fix,omitempty"`
	// Runner is the Package path of the run kit that owns this Process, empty
	// for one the built in runner started. kitbashd does not supervise a
	// Process a kit owns: it is registered, it is not restored at boot, and
	// proc_stop forwards to the kit, see PLAN.md section 3.
	Runner string `json:"runner,omitempty"`
	// Health is the most recent probe kitbashd ran against this Process,
	// present only for one kitbashd runs itself whose manifest declares a
	// health path and that has been probed at least once. kitbashd holds the
	// readings, like the problems above, so proc_list reads both back from the
	// registry rather than from the runtime, see PLAN.md section 2.4.
	Health *Health `json:"health,omitempty"`
	// Mounts are the folders of Files this Process sees, as kitbashd resolved
	// them at registration, so a member reads what their Process can reach
	// rather than what the manifest asked for. Absent for a Process that
	// declared none, which is every Process written before mounts existed.
	Mounts []mounts.Resolved `json:"mounts,omitempty"`

	// Container is the runtime name the bridge execs into. It is not part of
	// the tool's output: the surface names a Process by its id.
	Container string `json:"-"`
	// Replaced is the Process this run took the place of, if any, so the
	// bridge can unpublish its tools. It is not part of the output either.
	Replaced *Process `json:"-"`
}

// Health is one Process's most recent probe: when kitbashd requested the
// declared path, and whether the answer was healthy.
type Health struct {
	Last    string `json:"last,omitempty"`
	Healthy bool   `json:"healthy"`
}

// ListResult is the output of proc_list.
type ListResult struct {
	Processes []Process `json:"processes"`
}

// StopResult is the output of proc_stop.
type StopResult struct {
	ID    string `json:"id"`
	State string `json:"state"`

	// Process is the Process that was stopped, for the bridge.
	Process *Process `json:"-"`
}

// LogsResult is the output of proc_logs.
type LogsResult struct {
	ID    string   `json:"id"`
	Lines []string `json:"lines"`
}

// Registry is the Process registry of kitbashd, see spec/kitbashd-api.yaml.
// Registering mints the Telemetry token a Process exports with and the secret
// its fan out carries; unregistering revokes the token. It is an interface
// here so this package depends on the daemon's contract rather than on a
// socket.
// It is also the supervisor: kitbashd runs, stops and removes the container
// of a Process as its owner, because a member cannot place their own container
// in a delegated cgroup and the manifest's limits are only enforced in one,
// see PLAN.md section 2.3 and internal/cgroups.
type Registry interface {
	RegisterProcess(ctx context.Context, reg telemetry.Registration) (token, fanoutSecret string, prob *problem.Problem)
	UnregisterProcess(ctx context.Context, id string) *problem.Problem
	ListProcesses(ctx context.Context) ([]telemetry.Registered, *problem.Problem)
	// StartProcess runs the container of a registered Process as its owner
	// and answers the id the runtime gave it.
	StartProcess(ctx context.Context, id string, opts telemetry.StartOptions) (string, *problem.Problem)
	// StopProcess stops it, giving it StopTimeout seconds to exit.
	StopProcess(ctx context.Context, id string) *problem.Problem
	// RemoveProcess removes it, which is what a replacement does to the
	// Process it takes the place of.
	RemoveProcess(ctx context.Context, id string) *problem.Problem
	// Builds is what kitbashd knows about the builds of one Package path,
	// newest first. A digest this member does not have may be one another
	// member built, and the record is what says so.
	Builds(ctx context.Context, path, commit, digest string) ([]telemetry.Build, *problem.Problem)
	// FetchImage asks kitbashd to copy one image into this member's store
	// from the member who built it, which is what makes a digest another
	// member built runnable here, see PLAN.md section 2.2.
	FetchImage(ctx context.Context, digest, path, from string) (*telemetry.FetchResult, *problem.Problem)
}

// Service answers the proc family for one caller.
type Service struct {
	files    *fs.Service
	runner   podman.Runner
	registry Registry
	kits     Kits
	logger   *log.Logger
}

// New builds the Service the server runs with. A nil registry is a host
// without kitbashd: Packages still build and Processes still list, and
// proc_run refuses rather than starting a Process nobody supervises, see
// PLAN.md section 4.5.
func New(files *fs.Service, runner podman.Runner, registry Registry) *Service {
	return &Service{
		files:    files,
		runner:   runner,
		registry: registry,
		logger:   log.New(os.Stderr, "kitbash: ", log.LstdFlags),
	}
}

// SetKits gives the service the MCP bridge, which is how a Process reaches the
// run kit a manifest names. Without one every Package runs through the built
// in runner, and a manifest that names a runner is refused rather than run
// somewhere nobody asked for.
func (s *Service) SetKits(kits Kits) { s.kits = kits }

// SetLogger replaces where the service says what it could not do, such as a
// Process that started untraced. Tests read those lines; the server leaves it
// at stderr, which is the session's own log.
func (s *Service) SetLogger(logger *log.Logger) {
	if logger != nil {
		s.logger = logger
	}
}

// Run answers proc_run: it starts a Process from a Package digest, or
// converges an existing one with the same name onto that digest. Calling it
// twice with the same name and digest returns the same Process.
func (s *Service) Run(ctx context.Context, path, digest, name string) (*Process, *problem.Problem) {
	// Starting a Process is the second operation worth a span of its own, see
	// PLAN.md section 2.4. The Process id and the digest are known only once
	// the container is up, so they land on the span at the end.
	ctx, span := telemetry.Start(ctx, "run")
	defer span.End()
	process, prob := s.run(ctx, span, path, digest, name)
	if prob != nil {
		span.Fail(prob.Slug(), prob.Title)
		return nil, prob
	}
	// The Process is what the whole call is about, so it goes on the tool's
	// span as well as this one: a query by process has to find proc_run.
	telemetry.SetProcess(ctx, process.ID)
	span.SetDigest(process.Digest)
	span.OK()
	return process, nil
}

func (s *Service) run(ctx context.Context, span *telemetry.Span, path, digest, name string) (*Process, *problem.Problem) {
	// kitbashd runs the container, so a host without it starts nothing. The
	// alternative is a Process with no token, no fan out secret and no cgroup,
	// which is the untraced path PLAN.md section 2.6 does not have.
	if s.registry == nil {
		return nil, problem.Internal(path,
			"kitbashd is not reachable from this session, and it is the one that runs a Process",
			telemetry.NotRunningFix)
	}
	m, folder, prob := s.files.Manifest(ctx, path)
	if prob != nil {
		return nil, prob
	}
	telemetry.SetPackage(ctx, folder)
	if manifest.Reserved(m.Name) {
		return nil, problem.InvalidManifest(folder, fmt.Sprintf(
			"the Package name %q is a built in tool family, so its tools would collide with the surface", m.Name))
	}
	unit, ok := m.Unit()
	if !ok || unit.Type != manifest.UnitContainer {
		return nil, problem.InvalidManifest(folder, "version 1 runs the first container unit of a Package")
	}
	// A unit that names a runner is the kit's to start. Dispatch is routing
	// and not a third built in: the built in rootless podman runner below is
	// still what runs every Package that names none, see PLAN.md section 3.
	if unit.Runner != "" {
		return s.runWithKit(ctx, span, m, folder, unit, digest, name)
	}

	image, prob := s.image(ctx, span, folder, digest)
	if prob != nil {
		return nil, builtElsewhere(prob, unit)
	}
	if name == "" {
		name = m.Name
	}
	container := ContainerName(m.Name, name)

	// The whole run command is built and checked before anything is removed.
	// Replacing a Process destroys the old container, so every failure that
	// can be found without the runtime has to be found first.
	opts := podman.RunOptions{
		Name:        container,
		Image:       image.ID,
		Labels:      s.labels(folder, name, image.ID, unit.Expose),
		Env:         unit.Env,
		Restart:     restartPolicy(unit.Restart),
		CPUs:        unit.Limits.CPU,
		Memory:      podman.MemoryLimit(unit.Limits.Memory),
		Detach:      true,
		Interactive: true,
	}
	// The host port is chosen here rather than by the runtime, because the
	// endpoint has to be known before the Process is registered: registering
	// twice would mint a second token and leave the container holding the
	// first, see client_behaviour.processes in spec/kitbashd-api.yaml.
	var endpoint string
	if unit.Expose == manifest.ExposeHTTP {
		if unit.Port <= 0 {
			// Without a port there is nothing to publish and no endpoint to
			// register, so the Process would be exposed in name only.
			return nil, problem.InvalidManifestFix(folder,
				"expose: http declares no port, so the Process has no endpoint to be reached on",
				"Give deploy.units[0] the port the container listens on, or set expose to none.")
		}
		mapping := podman.PortMapping{ContainerPort: unit.Port}
		if host, err := freePort(); err == nil {
			mapping.HostPort = host
			endpoint = "http://127.0.0.1:" + strconv.Itoa(host)
			opts.Labels[podman.LabelEndpoint] = endpoint
		} else {
			// No free port to hold means the runtime picks one and this
			// Process is registered without an endpoint: it produces
			// Telemetry but receives no fan out until it is run again.
			s.logger.Printf("proc: choosing a host port for %s: %v", folder, err)
		}
		opts.Publish = []podman.PortMapping{mapping}
	}
	if prob := checkOptions(folder, opts); prob != nil {
		return nil, prob
	}
	// The mounts are checked here as the member, before anything is removed,
	// so a manifest that names a folder this member cannot mount is refused
	// with the folder named rather than as whatever the daemon answers. This
	// check cannot be trusted and is not meant to be: it runs as the member,
	// who can change the tree under it, and the registration below sends the
	// declaration rather than what this resolved. kitbashd runs the same check
	// as root and its answer is the one that decides, see PLAN.md section 2.3.
	if prob := s.checkMounts(folder, unit.Mounts); prob != nil {
		return nil, prob
	}

	existing, prob := s.byName(ctx, container)
	if prob != nil {
		return nil, prob
	}
	var replaced *Process
	if existing != nil {
		if owner := existing.Labels[podman.LabelPackage]; owner != folder {
			// The container name is readable rather than unique: two folders
			// may carry the same manifest name. The Package path is the
			// identity, so this Process belongs to someone else.
			return nil, problem.ConflictFix(folder, fmt.Sprintf(
				"a Process named %s already exists for %s", name, owner),
				"Pass a different name to proc_run, or stop that Process first.")
		}
		if existing.Labels[podman.LabelDigest] == image.ID && existing.State == podman.StateRunning {
			// Already converged. Repeating the call is safe and changes
			// nothing, which is the invariant in PLAN.md section 2.6.
			process := s.describe(*existing, unit)
			return &process, nil
		}
		previous := s.describe(*existing, unit)
		replaced = &previous
		if prob := s.remove(ctx, previous.ID, container); prob != nil {
			return nil, prob
		}
		// The container is gone, so the token it held has to go with it. This
		// happens after the removal: a Process that is still running keeps
		// exporting until it does not exist.
		s.unregister(ctx, previous.ID)
	}

	// The Process is registered before the container starts: the registration
	// is what kitbashd reads the owner, the container name, the image and the
	// two credentials from when it runs it, see PLAN.md 2.4.
	id := opts.Labels[podman.LabelID]
	prob = s.register(ctx, telemetry.Registration{
		ID:      id,
		Package: folder,
		Name:    name,
		// The container name and the image are what boot restore starts from,
		// so they are registered with the Process rather than read back from a
		// manifest at boot, see registration_fields in spec/kitbashd-api.yaml.
		Container:     container,
		Digest:        image.ID,
		Expose:        unit.Expose,
		Endpoint:      endpoint,
		Subscriptions: m.Subscriptions(),
		// What this Process may call back over /mcp. The manifest is the
		// declaration, so a Package that asks for nothing gets an empty
		// surface rather than its owner's whole one, see PLAN.md section 2.3.
		Permits: m.Permits(),
		// The health path travels with the registration for the same reason
		// the container name does: kitbashd probes it, and after a reboot the
		// registration is the only thing that remembers what to request.
		Health: s.health(folder, unit),
		// The mounts travel with the registration unresolved: kitbashd
		// resolves them as root, records what it resolved, and mounts that on
		// every start and every restore, see mounts in
		// spec/kitbashd-api.yaml.
		Mounts: unit.Mounts,
	})
	if prob != nil {
		return nil, prob
	}
	// A registration whose container never started is worse than no
	// registration: it names an endpoint on this host, so kitbashd would fan
	// the member's records out to whatever takes that loopback port next.
	orphan := func() {
		s.logger.Printf("proc: Process %s did not start; unregistering it", id)
		s.unregister(ctx, id)
	}

	// kitbashd runs it, as this member, inside their delegated cgroup. The
	// environment that crosses the socket is the manifest's own: the token and
	// the fan out secret are written by the daemon into a file only the member
	// can read, so neither is ever in a request body or on a command line.
	if _, prob := s.registry.StartProcess(ctx, id, startOptions(opts)); prob != nil {
		orphan()
		return nil, prob
	}

	started, prob := s.byName(ctx, container)
	if prob != nil {
		orphan()
		return nil, prob
	}
	if started == nil {
		orphan()
		return nil, problem.Internal(folder, "the container was started but is not in the container list", "")
	}
	process := s.describe(*started, unit)
	process.Replaced = replaced
	// The mounts on the answer are kitbashd's, not the manifest's: what the
	// member wants to read is which folders this Process ended up with, and
	// the daemon is the one that resolved them.
	if held, ok := s.registered(ctx)[process.ID]; ok {
		process.Mounts = held.Mounts
	}
	return &process, nil
}

// checkMounts is the member's own run of the check kitbashd makes as root. It
// exists for the problem alone: a member who names another member's home reads
// that in the session that named it, instead of a refusal from a daemon they
// cannot see the roots of. A member this session cannot look up is not a
// refusal here, because kitbashd can look them up and will.
func (s *Service) checkMounts(folder string, declared []manifest.Mount) *problem.Problem {
	if len(declared) == 0 {
		return nil
	}
	checker, err := mounts.NewChecker(s.files.User())
	if err != nil {
		s.logger.Printf("proc: not checking the mounts of %s here: %v", folder, err)
		return nil
	}
	_, prob := checker.Resolve(folder, declared)
	return prob
}

// builtElsewhere says what a Package that names a builder and no runner is
// missing. Its images are made wherever the build kit builds them, so the
// caller's own store has nothing to run and "this Package has not been built
// yet" would send them to pkg_build, which they have already called. A builder
// is paired with a runner in practice, see PLAN.md section 3.
func builtElsewhere(prob *problem.Problem, unit manifest.Unit) *problem.Problem {
	if prob.Slug() != problem.SlugNotFound || unit.Builder == "" || unit.Runner != "" {
		return prob
	}
	answer := *prob
	answer.Detail = fmt.Sprintf(
		"%s: this Package names the build kit at %s, and the image it builds is in that kit's hands rather than in this member's image store",
		prob.Detail, unit.Builder)
	answer.Fix = fmt.Sprintf(
		"Give the unit a runner as well, so the kit that holds the image is the one that runs it, or name the digest to run if this host has the image. A Package built by %s is normally run by a kit too.",
		unit.Builder)
	return &answer
}

// List answers proc_list: every Process of the caller, running or stopped.
//
// The container runtime holds the state and this reads it back, with the three
// things added that the runtime does not know: why a Process is not running,
// what its last health probe saw, and the Processes a run kit owns, which have
// no container here at all. A container the boot restore could not start is
// still in the runtime as created, which reads as starting, so kitbashd is
// asked what it could not bring back and those are reported failed with the
// reason, see restore in spec/kitbashd-api.yaml. All three come from the one
// registry call.
func (s *Service) List(ctx context.Context) (*ListResult, *problem.Problem) {
	containers, prob := s.containers(ctx, podman.Filter{podman.LabelUser: s.files.User()}, true)
	if prob != nil {
		return nil, prob
	}
	known := s.registered(ctx)
	result := &ListResult{Processes: []Process{}}
	for _, container := range containers {
		process := s.describe(container, manifest.Unit{})
		if reported, held := known[process.ID]; held {
			if reported.Problem != "" && process.State != StateRunning {
				process.State = StateFailed
				process.Problem = reported.Problem
				process.Fix = reported.ProblemFix
			}
			if reading := reported.Health; reading != nil && reading.Last != "" && reading.Healthy != nil {
				process.Health = &Health{Last: reading.Last, Healthy: *reading.Healthy}
			}
			// The runtime knows what is bound into a container; the
			// registration knows what kitbash agreed to bind, which is the
			// one a member reads.
			process.Mounts = reported.Mounts
		}
		result.Processes = append(result.Processes, process)
	}
	// The Processes a run kit owns run wherever the kit put them, so the
	// runtime here has nothing to report about them and the registry is what
	// says they exist.
	result.Processes = append(result.Processes, s.kitOwnedProcesses(known, result.Processes)...)
	sort.Slice(result.Processes, func(i, j int) bool {
		return result.Processes[i].Name < result.Processes[j].Name
	})
	return result, nil
}

// registered is what kitbashd holds about the Processes of this member, by id:
// why one did not come back, and what its last health probe saw. Neither is in
// the container runtime, and one call answers both.
//
// A session without kitbashd, or one whose call to it fails, gets none:
// proc_list reads the caller's own runtime and must keep working when the
// daemon does not, see PLAN.md section 4.5.
func (s *Service) registered(ctx context.Context) map[string]telemetry.Registered {
	if s.registry == nil {
		return nil
	}
	known, prob := s.registry.ListProcesses(ctx)
	if prob != nil {
		s.logger.Printf("proc_list: kitbashd did not answer what it holds about these Processes: %s", prob.Detail)
		return nil
	}
	entries := map[string]telemetry.Registered{}
	for _, entry := range known {
		entries[entry.ID] = entry
	}
	return entries
}

// health is the probe one unit declares, or nothing. A unit that declares a
// command probe and no path is said once here: kitbash records health.exec and
// runs it nowhere, see PLAN.md section 2.4.
func (s *Service) health(folder string, unit manifest.Unit) *telemetry.Health {
	path, interval := unit.HealthProbe()
	if path == "" {
		if unit.HealthExec() {
			s.logger.Printf("proc: %s declares health.exec, which kitbash records and does not run; declare health.http for kitbashd to probe it", folder)
		}
		return nil
	}
	return &telemetry.Health{HTTP: path, Interval: interval}
}

// Stop answers proc_stop. The Process stays known and can be run again.
func (s *Service) Stop(ctx context.Context, id string) (*StopResult, *problem.Problem) {
	// A Process a run kit owns has no container here to stop, so the kit is
	// asked first: the registry is the only thing that knows the Process
	// exists at all.
	if reg, owned := s.kitOwned(ctx, id); owned {
		return s.stopWithKit(ctx, id, reg)
	}
	container, prob := s.byID(ctx, id)
	if prob != nil {
		return nil, prob
	}
	process := s.describe(*container, manifest.Unit{})
	if prob := s.stop(ctx, id, container.Name); prob != nil {
		return nil, prob
	}
	// The token outlives nothing: once the container is stopped it cannot
	// export, so the registration goes with it.
	s.unregister(ctx, id)
	process.State = StateStopped
	return &StopResult{ID: id, State: StateStopped, Process: &process}, nil
}

// Reconcile makes kitbashd's Process registry agree with the containers that
// are running here. It registers every running Process the daemon does not
// know and returns those ids, together with the ids the daemon holds that no
// longer run here. A stale registration is left alone: proc_stop removes the
// ones this host retired, and a registration whose endpoint is gone only costs
// kitbashd a failed delivery.
//
// Re-registering mints a new token and a new fan out secret, neither of which
// the running container has, so its exports fail and it refuses the fan out
// until the Process is run again. That gap is accepted in version 1 and this
// says so once per Process, see client_behaviour.processes in
// spec/kitbashd-api.yaml.
func (s *Service) Reconcile(ctx context.Context, running []Process) (registered, stale []string, prob *problem.Problem) {
	if s.registry == nil {
		return nil, nil, nil
	}
	known, prob := s.registry.ListProcesses(ctx)
	if prob != nil {
		return nil, nil, prob
	}
	// An admin's list is the whole machine, so the entries of other members
	// are not this session's to compare against: a Process of theirs is
	// neither missing from the registry nor stale because it is not running
	// here.
	mine := make([]telemetry.Registered, 0, len(known))
	byID := map[string]telemetry.Registered{}
	for _, entry := range known {
		if entry.Owner != "" && entry.Owner != s.files.User() {
			continue
		}
		mine = append(mine, entry)
		byID[entry.ID] = entry
	}
	here := map[string]bool{}
	for i := range running {
		p := running[i]
		if p.State != StateRunning {
			continue
		}
		// Only a running Process makes a registration current. One that is
		// stopped has none of its own, and kitbashd holding one for it is the
		// stale case below.
		here[p.ID] = true
		if _, ok := byID[p.ID]; ok {
			continue
		}
		reg := telemetry.Registration{
			ID:        p.ID,
			Package:   p.Package,
			Name:      p.Name,
			Container: p.Container,
			Digest:    p.Digest,
			Expose:    p.Expose,
			Endpoint:  p.Endpoint,
		}
		if m, folder, prob := s.files.Manifest(ctx, p.Package); prob == nil {
			reg.Package = folder
			reg.Subscriptions = m.Subscriptions()
			// A Process re-registered here is one this host is running
			// already, so it keeps what its manifest declares it may call and
			// the health path it declares.
			reg.Permits = m.Permits()
			if unit, ok := m.Unit(); ok {
				reg.Health = s.health(p.Package, unit)
			}
		}
		if _, _, prob := s.registry.RegisterProcess(ctx, reg); prob != nil {
			s.logger.Printf("proc: registering Process %s at %s: %s", p.ID, p.Package, prob.Detail)
			continue
		}
		// This registration replaced the secret as well as the token. Which of
		// the two states the running container is in cannot be seen from here,
		// and they are not the same problem: a container started under an
		// earlier registration holds a secret this one replaced and now
		// refuses every delivery, while one started while kitbashd was
		// unreachable holds none and takes records from anything that can
		// reach its port. Running it again ends either one. The caller says
		// the same about the token; the secret is said here because nothing
		// else would.
		s.logger.Printf("proc: Process %s was registered again; the running container holds the fan out secret this registration replaced and refuses every delivery, or was started with none and accepts a delivery from anything on the host. Run it again to end either state.", p.ID)
		registered = append(registered, p.ID)
	}
	for _, entry := range mine {
		if !here[entry.ID] {
			stale = append(stale, entry.ID)
		}
	}
	return registered, stale, nil
}

// register records the Process with kitbashd, which is what mints the
// Telemetry token it exports with and the secret its fan out carries. Both are
// returned here and both are dropped: kitbashd puts them in the container
// itself, so this session never holds a credential of a Process it started.
//
// A registration that fails is a Process that does not start. kitbashd is the
// one that runs the container, so there is nothing to fall back to and nothing
// untraced to leave behind, see PLAN.md section 2.6.
func (s *Service) register(ctx context.Context, reg telemetry.Registration) *problem.Problem {
	if _, _, prob := s.registry.RegisterProcess(ctx, reg); prob != nil {
		s.logger.Printf("proc: registering Process %s at %s: %s", reg.ID, reg.Package, prob.Detail)
		return prob
	}
	return nil
}

// startOptions is the command line kitbashd runs, as
// spec/kitbashd-api.yaml carries it. It is the same options this session would
// have run, minus everything the daemon speaks for.
func startOptions(opts podman.RunOptions) telemetry.StartOptions {
	out := telemetry.StartOptions{
		Container: opts.Name,
		Image:     opts.Image,
		Labels:    opts.Labels,
		Env:       ownEnv(opts.Env),
		Restart:   opts.Restart,
		CPU:       opts.CPUs,
		Memory:    opts.Memory,
	}
	for _, port := range opts.Publish {
		out.Publish = append(out.Publish, telemetry.PortMapping{
			HostPort: port.HostPort, ContainerPort: port.ContainerPort,
		})
	}
	return out
}

// ownEnv is a unit's own environment with everything kitbashd speaks for taken
// out. The daemon drops them too; they are dropped here as well so a manifest's
// KITBASH_FANOUT_SECRET does not cross the socket in a body that could be
// logged.
func ownEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	own := make(map[string]string, len(env))
	for k, v := range env {
		own[k] = v
	}
	for _, key := range telemetry.OwnedEnv {
		delete(own, key)
	}
	if len(own) == 0 {
		return nil
	}
	return own
}

// stop stops one container. kitbashd does it, as the owner; a Process the
// daemon has no registration for is stopped through this session's own
// runtime, which is the member's, so a container that outlived its
// registration can still be stopped by the member who runs it.
func (s *Service) stop(ctx context.Context, id, container string) *problem.Problem {
	prob := s.registry.StopProcess(ctx, id)
	if prob == nil {
		return nil
	}
	if prob.Status != http.StatusNotFound {
		return prob
	}
	s.logger.Printf("proc: kitbashd has no registration for Process %s; stopping %s in this session", id, container)
	if err := s.runner.Stop(ctx, container, StopTimeout); err != nil {
		return problem.Internal(id, err.Error(), "")
	}
	return nil
}

// remove removes one container, the same way and for the same reason.
func (s *Service) remove(ctx context.Context, id, container string) *problem.Problem {
	prob := s.registry.RemoveProcess(ctx, id)
	if prob == nil {
		return nil
	}
	if prob.Status != http.StatusNotFound {
		return prob
	}
	s.logger.Printf("proc: kitbashd has no registration for Process %s; removing %s in this session", id, container)
	if err := s.runner.Remove(ctx, container, true); err != nil {
		return problem.Internal(id, err.Error(), "")
	}
	return nil
}

// unregister revokes one Process's token. A daemon that is not there is not a
// failure of the call that stopped the container: the container is gone either
// way and the token dies with the daemon's next start.
func (s *Service) unregister(ctx context.Context, id string) {
	if s.registry == nil || id == "" {
		return
	}
	if prob := s.registry.UnregisterProcess(ctx, id); prob != nil {
		s.logger.Printf("proc: unregistering Process %s: %s", id, prob.Detail)
	}
}

// freePort picks a loopback port nothing is listening on by binding it and
// letting it go. Another program may take it in between; the runtime then
// refuses the run and the caller runs again, which is cheaper than the second
// registration a runtime chosen port would cost.
func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("the loopback listener has no TCP address")
	}
	return addr.Port, nil
}

// Logs answers proc_logs: the raw stdout and stderr of a Process.
func (s *Service) Logs(ctx context.Context, id string, lines int) (*LogsResult, *problem.Problem) {
	if lines <= 0 {
		lines = DefaultLogLines
	}
	if lines > MaxLogLines {
		lines = MaxLogLines
	}
	container, prob := s.byID(ctx, id)
	if prob != nil {
		return nil, prob
	}
	out, err := s.runner.Logs(ctx, container.Name, lines)
	if err != nil {
		return nil, problem.Internal(id, err.Error(), "")
	}
	if out == nil {
		out = []string{}
	}
	return &LogsResult{ID: id, Lines: out}, nil
}

// ContainerName is the runtime name of one Process, which is what makes the
// name unique per Package and per caller.
func ContainerName(pkg, name string) string {
	return "kitbash-" + pkg + "-" + name
}

// labels are the whole Process record, read back off the container.
func (s *Service) labels(folder, name, digest, expose string) map[string]string {
	return map[string]string{
		podman.LabelID:      uuid.V7(),
		podman.LabelUser:    s.files.User(),
		podman.LabelPackage: folder,
		podman.LabelName:    name,
		podman.LabelDigest:  digest,
		podman.LabelExpose:  expose,
	}
}

// image resolves the digest to run: the one the caller asked for, or the
// newest build of this Package.
//
// A digest the caller named and this member's store does not have may be one
// another member built, which kitbashd copies over rather than leaving the
// member to build the same commit again, see PLAN.md section 2.2.
func (s *Service) image(ctx context.Context, span *telemetry.Span, folder, digest string) (*podman.Image, *problem.Problem) {
	images, err := s.runner.Images(ctx, podman.Filter{podman.LabelPath: folder})
	if err != nil {
		return nil, problem.Internal(folder, err.Error(), "")
	}
	if digest == "" {
		if len(images) == 0 {
			return nil, problem.NotFoundFix(folder, "this Package has not been built yet",
				"Call pkg_build first, then run the digest it returns.")
		}
		return s.latest(ctx, span, folder, images), nil
	}
	for i := range images {
		if images[i].ID == digest {
			return &images[i], nil
		}
	}
	if image := s.fetch(ctx, folder, digest); image != nil {
		return image, nil
	}
	if len(images) == 0 {
		return nil, problem.NotFoundFix(folder, "this Package has not been built yet",
			"Call pkg_build first, then run the digest it returns.")
	}
	return nil, problem.NotFoundFix(folder,
		fmt.Sprintf("no build of this Package has the digest %s", digest),
		"Call pkg_inspect to see the digests this Package has been built to.")
}

// latest picks the build a caller who named no digest runs, and says out loud
// which one it picked.
//
// The order is the image's own creation time. podman reports that at one
// second resolution in a listing, so internal/podman refines the images that
// share a second to nanoseconds, which is what tells two builds of one Package
// inside the same second apart, see issue #113.
//
// Two images that still share an instant are decided by the build records
// kitbashd keeps, the other thing that knows when a build happened, and a pair
// neither can separate is decided by the digest. The last step decides nothing
// about time, it only makes the answer the same one on every call: an
// arbitrary pick that changes between calls is the bug this fixes.
func (s *Service) latest(ctx context.Context, span *telemetry.Span, folder string, images []podman.Image) *podman.Image {
	// The listing arrives newest first with the digest as the tie break, so
	// the head is already the answer for everything but a shared instant.
	chosen := 0
	newest := images[0].Created
	var tied []int
	for i := range images {
		if images[i].Created.Equal(newest) {
			tied = append(tied, i)
		}
	}
	how := "it is the newest image in this store, created " + newest.Format(time.RFC3339Nano)
	if len(tied) > 1 {
		how = fmt.Sprintf("%d images of this Package share the instant %s, and this one has the highest digest",
			len(tied), newest.Format(time.RFC3339Nano))
		if i, at, ok := s.recordedLast(ctx, folder, images, tied); ok {
			chosen = i
			how = fmt.Sprintf("%d images of this Package share the instant %s, and kitbashd recorded this one built last, at %s",
				len(tied), newest.Format(time.RFC3339Nano), at)
		}
	}
	image := &images[chosen]
	line := fmt.Sprintf("proc_run on %s named no digest, so it runs the latest build %s: %s",
		folder, image.ID, how)
	s.logger.Printf("proc: %s", line)
	if span != nil {
		span.Info(line)
	}
	return image
}

// recordedLast asks kitbashd which of a set of images it recorded a build for
// most recently, and answers the index of that image together with the time
// the record carries. It answers false for a daemon that is not there, a path
// it has no records of, and a set none of the records name: all three mean the
// caller has to decide the tie without a build record.
func (s *Service) recordedLast(ctx context.Context, folder string, images []podman.Image, tied []int) (int, string, bool) {
	if s.registry == nil {
		return 0, "", false
	}
	builds, prob := s.registry.Builds(ctx, folder, "", "")
	if prob != nil {
		s.logger.Printf("proc: kitbashd did not answer what it recorded about the builds of %s: %s", folder, prob.Detail)
		return 0, "", false
	}
	// kitbashd answers newest first, so the first record naming one of these
	// images is the one that was built last.
	for _, build := range builds {
		for _, i := range tied {
			if images[i].ID == build.Digest {
				return i, build.BuiltAt, true
			}
		}
	}
	return 0, "", false
}

// fetch asks kitbashd for an image another member built. It answers nil for
// everything that is not a copy this member may have, which the caller reports
// as the not-found it would have reported anyway: a digest nobody recorded, a
// builder who no longer has it, and a daemon that is not answering are all
// "this member cannot run that digest".
func (s *Service) fetch(ctx context.Context, folder, digest string) *podman.Image {
	if s.registry == nil {
		return nil
	}
	builds, prob := s.registry.Builds(ctx, folder, "", digest)
	if prob != nil || len(builds) == 0 {
		return nil
	}
	build := builds[0]
	if build.Builder == "" || build.Builder == s.files.User() {
		// The record names this member, so the image was theirs and is gone.
		// Copying it from themselves would answer the same nothing.
		return nil
	}
	if _, prob := s.registry.FetchImage(ctx, digest, folder, build.Builder); prob != nil {
		s.logger.Printf("proc: %s could not be copied from %s: %s", digest, build.Builder, prob.Detail)
		return nil
	}
	// The copy is not believed until this member's own store answers for it,
	// which is also where the labels of the image come from.
	images, err := s.runner.Images(ctx, podman.Filter{podman.LabelPath: folder})
	if err != nil {
		return nil
	}
	for i := range images {
		if images[i].ID == digest {
			return &images[i]
		}
	}
	return nil
}

// describe turns a container into the Process the surface publishes. The unit
// fills in what only the manifest knows; a listing that did not read the
// manifest passes the zero unit and falls back to the labels.
func (s *Service) describe(container podman.Container, unit manifest.Unit) Process {
	process := Process{
		ID:        container.Labels[podman.LabelID],
		Name:      container.Labels[podman.LabelName],
		Package:   container.Labels[podman.LabelPackage],
		Digest:    container.Labels[podman.LabelDigest],
		State:     State(container),
		Expose:    container.Labels[podman.LabelExpose],
		Container: container.Name,
	}
	if unit.Expose != "" {
		process.Expose = unit.Expose
	}
	if !container.StartedAt.IsZero() {
		process.StartedAt = container.StartedAt.UTC().Format(time.RFC3339)
	}
	if process.Expose == manifest.ExposeHTTP {
		// The label is the endpoint this Process was registered with, so a
		// later session re-registers the same one. A container started before
		// the label existed still answers through its published port.
		process.Endpoint = container.Labels[podman.LabelEndpoint]
		for _, port := range container.Ports {
			if process.Endpoint == "" && port.HostPort > 0 {
				process.Endpoint = "http://127.0.0.1:" + strconv.Itoa(port.HostPort)
				break
			}
		}
	}
	return process
}

// State maps a container state onto the five states the surface publishes.
// Health probing arrives with the daemon, so paused is the only state reported
// unhealthy today, see PLAN.md section 5.5. A state the runtime has never
// spoken is reported failed rather than healthy: not knowing is not fine.
func State(container podman.Container) string {
	switch container.State {
	case podman.StateRunning:
		return StateRunning
	case podman.StateCreated, podman.StateConfigured:
		return StateStarting
	case podman.StateStopped, podman.StateStopping, podman.StateRemoving:
		return StateStopped
	case podman.StatePaused:
		return StateUnhealthy
	case podman.StateExited:
		if container.ExitCode == 0 {
			return StateStopped
		}
		return StateFailed
	default:
		return StateFailed
	}
}

// ToolName is how a Package tool appears on the caller's surface. MCP tool
// names allow no dots, so the dot form in PLAN.md is spelled with underscore.
func ToolName(pkg, tool string) string { return pkg + "_" + tool }

// checkOptions is the second belt under spec/manifest.schema.json: it catches a
// run command the runtime would refuse, before a replacement removes the
// Process that is running now.
func checkOptions(folder string, opts podman.RunOptions) *problem.Problem {
	if opts.Memory != "" && !podman.ValidMemory(opts.Memory) {
		return problem.InvalidManifestFix(folder,
			fmt.Sprintf("limits.memory %q is not a size the container runtime takes", opts.Memory),
			"Write limits.memory as a number with Ki, Mi or Gi, such as 512Mi.")
	}
	if opts.CPUs != "" && !podman.ValidCPUs(opts.CPUs) {
		return problem.InvalidManifestFix(folder,
			fmt.Sprintf("limits.cpu %q is not a number of cores", opts.CPUs),
			"Write limits.cpu as a number, such as 1 or 0.5.")
	}
	if !podman.ValidRestart(opts.Restart) {
		return problem.InvalidManifestFix(folder,
			fmt.Sprintf("restart %q is not a policy the container runtime takes", opts.Restart),
			"Write restart as always, on-failure or never.")
	}
	return nil
}

// restartPolicy maps the manifest's spelling onto the runtime's.
func restartPolicy(restart string) string {
	if restart == manifest.RestartNever {
		return podman.RestartNo
	}
	return restart
}

// byName finds one container by its runtime name.
func (s *Service) byName(ctx context.Context, name string) (*podman.Container, *problem.Problem) {
	containers, prob := s.containers(ctx, podman.Filter{podman.LabelUser: s.files.User()}, true)
	if prob != nil {
		return nil, prob
	}
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i], nil
		}
	}
	return nil, nil
}

// byID finds one container by the kitbash.id label, which is the Process id.
func (s *Service) byID(ctx context.Context, id string) (*podman.Container, *problem.Problem) {
	if id == "" {
		return nil, problem.NotFoundFix(id, "no Process id was given",
			"Call proc_list to see your Processes and their ids.")
	}
	containers, prob := s.containers(ctx, podman.Filter{
		podman.LabelUser: s.files.User(),
		podman.LabelID:   id,
	}, true)
	if prob != nil {
		return nil, prob
	}
	if len(containers) == 0 {
		return nil, problem.NotFoundFix(id, "no Process of yours has this id",
			"Call proc_list to see your Processes and their ids.")
	}
	return &containers[0], nil
}

// containers reads the container list, mapping a runtime failure onto the
// surface. The runtime's own output goes to the server log only.
func (s *Service) containers(ctx context.Context, filter podman.Filter, all bool) ([]podman.Container, *problem.Problem) {
	containers, err := s.runner.Containers(ctx, filter, all)
	if err != nil {
		return nil, problem.Internal("proc", err.Error(), "")
	}
	return containers, nil
}
