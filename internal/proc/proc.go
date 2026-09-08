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
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/manifest"
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

	// Container is the runtime name the bridge execs into. It is not part of
	// the tool's output: the surface names a Process by its id.
	Container string `json:"-"`
	// Replaced is the Process this run took the place of, if any, so the
	// bridge can unpublish its tools. It is not part of the output either.
	Replaced *Process `json:"-"`
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
// Registering mints the Telemetry token a Process exports with; unregistering
// revokes it. It is an interface here so this package depends on the daemon's
// contract rather than on a socket.
type Registry interface {
	RegisterProcess(ctx context.Context, reg telemetry.Registration) (string, *problem.Problem)
	UnregisterProcess(ctx context.Context, id string) *problem.Problem
	ListProcesses(ctx context.Context) ([]telemetry.Registered, *problem.Problem)
}

// Service answers the proc family for one caller.
type Service struct {
	files    *fs.Service
	runner   podman.Runner
	registry Registry
	logger   *log.Logger
}

// New builds the Service the server runs with. The registry may be nil, in
// which case every Process starts untraced: the container runtime does not
// depend on kitbashd and a host without it still runs Packages.
func New(files *fs.Service, runner podman.Runner, registry Registry) *Service {
	return &Service{
		files:    files,
		runner:   runner,
		registry: registry,
		logger:   log.New(os.Stderr, "kitbash: ", log.LstdFlags),
	}
}

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

	image, prob := s.image(ctx, folder, digest)
	if prob != nil {
		return nil, prob
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
		Memory:      memoryLimit(unit.Limits.Memory),
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
		if err := s.runner.Remove(ctx, container, true); err != nil {
			return nil, problem.Internal(folder, err.Error(), "")
		}
		// The container is gone, so the token it held has to go with it. This
		// happens after the removal: a Process that is still running keeps
		// exporting until it does not exist.
		s.unregister(ctx, previous.ID)
	}

	// The Process is registered before the container starts, so the token is
	// in the environment the container is created with, see PLAN.md 2.4.
	id := opts.Labels[podman.LabelID]
	env, registered := s.telemetryEnv(ctx, unit.Env, telemetry.Registration{
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
	})
	opts.Env = env
	// A registration whose container never started is worse than no
	// registration: it names an endpoint on this host, so kitbashd would fan
	// the member's records out to whatever takes that loopback port next.
	orphan := func() {
		if !registered {
			return
		}
		s.logger.Printf("proc: Process %s did not start; unregistering it", id)
		s.unregister(ctx, id)
	}

	if _, err := s.runner.Run(ctx, opts); err != nil {
		// The runtime says only that it refused the command: the same exit
		// status covers an image that is gone and a name taken since the
		// lookup. The manifest cases are caught by checkOptions above, before
		// anything is removed, so what is left is not the caller's to fix.
		orphan()
		return nil, problem.Internal(folder, err.Error(), "")
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
	return &process, nil
}

// List answers proc_list: every Process of the caller, running or stopped.
func (s *Service) List(ctx context.Context) (*ListResult, *problem.Problem) {
	containers, prob := s.containers(ctx, podman.Filter{podman.LabelUser: s.files.User()}, true)
	if prob != nil {
		return nil, prob
	}
	result := &ListResult{Processes: []Process{}}
	for _, container := range containers {
		result.Processes = append(result.Processes, s.describe(container, manifest.Unit{}))
	}
	sort.Slice(result.Processes, func(i, j int) bool {
		return result.Processes[i].Name < result.Processes[j].Name
	})
	return result, nil
}

// Stop answers proc_stop. The Process stays known and can be run again.
func (s *Service) Stop(ctx context.Context, id string) (*StopResult, *problem.Problem) {
	container, prob := s.byID(ctx, id)
	if prob != nil {
		return nil, prob
	}
	process := s.describe(*container, manifest.Unit{})
	if err := s.runner.Stop(ctx, container.Name, StopTimeout); err != nil {
		return nil, problem.Internal(id, err.Error(), "")
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
// Re-registering mints a new token, which the running container does not have,
// so its exports fail until the Process is run again. That gap is accepted in
// version 1 and the caller logs it, see client_behaviour.processes in
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
		}
		if _, prob := s.registry.RegisterProcess(ctx, reg); prob != nil {
			s.logger.Printf("proc: registering Process %s at %s: %s", p.ID, p.Package, prob.Detail)
			continue
		}
		registered = append(registered, p.ID)
	}
	for _, entry := range mine {
		if !here[entry.ID] {
			stale = append(stale, entry.ID)
		}
	}
	return registered, stale, nil
}

// telemetryEnv registers the Process and returns the environment its container
// is started with: the manifest's own, plus the six variables of
// spec/kitbashd-api.yaml. The manifest cannot override them; they are the
// Process's identity, not its configuration. The second return says whether
// the registration happened, so a container that never starts can be taken
// back off the registry.
//
// A registration that fails is not a reason to refuse to start the Process.
// The container runtime does not depend on kitbashd, so the Process starts
// with no token and produces no Telemetry of its own, and the session says so
// once in the server log.
func (s *Service) telemetryEnv(ctx context.Context, env map[string]string, reg telemetry.Registration) (map[string]string, bool) {
	merged := make(map[string]string, len(env)+6)
	for k, v := range env {
		merged[k] = v
	}
	if s.registry == nil {
		s.logger.Printf("proc: kitbashd is not running; Process %s starts untraced", reg.ID)
		return merged, false
	}
	token, prob := s.registry.RegisterProcess(ctx, reg)
	if prob != nil {
		s.logger.Printf("proc: kitbashd is not running; Process %s starts untraced: %s", reg.ID, prob.Detail)
		return merged, false
	}
	merged[telemetry.EnvEndpoint] = telemetry.EndpointForProcesses()
	merged[telemetry.EnvToken] = token
	merged[telemetry.EnvProcess] = reg.ID
	merged[telemetry.EnvPackage] = reg.Package
	merged[telemetry.EnvUser] = s.files.User()
	// The MCP endpoint is given with the token, not before it: without a
	// token there is nothing for a Process to authenticate a session with.
	merged[telemetry.EnvMCPEndpoint] = telemetry.MCPEndpointForProcesses()
	return merged, true
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
func (s *Service) image(ctx context.Context, folder, digest string) (*podman.Image, *problem.Problem) {
	images, err := s.runner.Images(ctx, podman.Filter{podman.LabelPath: folder})
	if err != nil {
		return nil, problem.Internal(folder, err.Error(), "")
	}
	if len(images) == 0 {
		return nil, problem.NotFoundFix(folder, "this Package has not been built yet",
			"Call pkg_build first, then run the digest it returns.")
	}
	if digest == "" {
		return &images[0], nil
	}
	for i := range images {
		if images[i].ID == digest {
			return &images[i], nil
		}
	}
	return nil, problem.NotFoundFix(folder,
		fmt.Sprintf("no build of this Package has the digest %s", digest),
		"Call pkg_inspect to see the digests this Package has been built to.")
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

// memorySuffixes maps the manifest's Kubernetes style memory suffix onto the
// runtime's own. spec/manifest.schema.json allows Ki, Mi and Gi only, so the
// table is the whole conversion.
var memorySuffixes = map[string]string{"Ki": "k", "Mi": "m", "Gi": "g"}

// memoryLimit converts one limits.memory value. An unknown suffix is passed
// through for the runtime to reject, which the manifest schema already
// prevents from happening.
func memoryLimit(memory string) string {
	if len(memory) < 3 {
		return memory
	}
	suffix, ok := memorySuffixes[memory[len(memory)-2:]]
	if !ok {
		return memory
	}
	return memory[:len(memory)-2] + suffix
}

// checkOptions is the second belt under spec/manifest.schema.json: it catches a
// run command the runtime would refuse, before a replacement removes the
// Process that is running now.
func checkOptions(folder string, opts podman.RunOptions) *problem.Problem {
	if opts.Memory != "" && !memoryValue.MatchString(opts.Memory) {
		return problem.InvalidManifestFix(folder,
			fmt.Sprintf("limits.memory %q is not a size the container runtime takes", opts.Memory),
			"Write limits.memory as a number with Ki, Mi or Gi, such as 512Mi.")
	}
	if opts.CPUs != "" && !cpuValue.MatchString(opts.CPUs) {
		return problem.InvalidManifestFix(folder,
			fmt.Sprintf("limits.cpu %q is not a number of cores", opts.CPUs),
			"Write limits.cpu as a number, such as 1 or 0.5.")
	}
	switch opts.Restart {
	case "always", "on-failure", "no":
	default:
		return problem.InvalidManifestFix(folder,
			fmt.Sprintf("restart %q is not a policy the container runtime takes", opts.Restart),
			"Write restart as always, on-failure or never.")
	}
	return nil
}

var (
	memoryValue = regexp.MustCompile(`^[0-9]+[kmg]?$`)
	cpuValue    = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
)

// restartPolicy maps the manifest's spelling onto the runtime's.
func restartPolicy(restart string) string {
	if restart == manifest.RestartNever {
		return "no"
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
