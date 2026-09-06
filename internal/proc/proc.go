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
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
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

// Service answers the proc family for one caller.
type Service struct {
	files  *fs.Service
	runner podman.Runner
}

// New builds the Service the server runs with.
func New(files *fs.Service, runner podman.Runner) *Service {
	return &Service{files: files, runner: runner}
}

// Run answers proc_run: it starts a Process from a Package digest, or
// converges an existing one with the same name onto that digest. Calling it
// twice with the same name and digest returns the same Process.
func (s *Service) Run(ctx context.Context, path, digest, name string) (*Process, *problem.Problem) {
	m, folder, prob := s.files.Manifest(ctx, path)
	if prob != nil {
		return nil, prob
	}
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
	if unit.Expose == manifest.ExposeHTTP && unit.Port > 0 {
		opts.Publish = []int{unit.Port}
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
	}

	if _, err := s.runner.Run(ctx, opts); err != nil {
		// The runtime says only that it refused the command: the same exit
		// status covers an image that is gone and a name taken since the
		// lookup. The manifest cases are caught by checkOptions above, before
		// anything is removed, so what is left is not the caller's to fix.
		return nil, problem.Internal(folder, err.Error(), "")
	}

	started, prob := s.byName(ctx, container)
	if prob != nil {
		return nil, prob
	}
	if started == nil {
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
	process.State = StateStopped
	return &StopResult{ID: id, State: StateStopped, Process: &process}, nil
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
		for _, port := range container.Ports {
			if port.HostPort > 0 {
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
