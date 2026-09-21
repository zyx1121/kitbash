package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/zyx1121/kitbash/internal/cgroups"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/mounts"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// A Package that declares more than one unit runs as one podman pod, see
// PLAN.md section 5.6. One Process, one registration, one token, one address:
// what changes is that the container a Process was is now several, sharing the
// pod's network namespace and reaching each other on localhost.
//
// A Package of one unit runs as the bare container it always did. Nothing in
// this file is on that path: a single unit Process is created, prepared,
// verified and started by exactly the command line it was before pods existed,
// which is what keeps every boot restore of a Process registered by an earlier
// release the restore it was.
//
// The pod owns what the units may not: the published port of the unit that is
// the Process's face, the network namespace, and the cgroup, which is the
// Process's ceiling as it is for a single unit. Each unit carries its own
// limits under that ceiling, its own mounts, its own secrets and its own
// command. podman refuses --publish and --hostname on a container created with
// --pod, verified against podman 5.7.0.

// startUnit is one unit's command line in a start request: everything its
// container is created with that the registration does not already say. The
// container and the image are the registration's, matched by this name, so a
// request cannot start an image the registry does not describe.
type startUnit struct {
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Restart   string            `json:"restart,omitempty"`
	CPU       string            `json:"cpu,omitempty"`
	Memory    string            `json:"memory,omitempty"`
	PidsLimit int               `json:"pidsLimit,omitempty"`
	Command   []string          `json:"command,omitempty"`
}

// MaxUnits is how many container units one Package may run. It is far above
// what composing two or three services needs and bounds what one proc_run
// costs this host: every unit is a container, an environment file and a mount
// check of its own.
const MaxUnits = 8

// preparedUnit is one unit of a pod with everything resolved that has to be
// resolved before anything is created: its command line, the mounts as they
// check out now, and the owner's current value for each secret it declares.
type preparedUnit struct {
	unit    store.Unit
	opts    podman.RunOptions
	mounted []mounts.Resolved
	held    map[string]string
}

// startPod runs every unit of one Process as one pod, as its owner. It is
// startProcess for a Package of more than one unit and follows the same order:
// everything that can be refused without the runtime is refused first, the
// cgroup is made, the token is minted, and only then is anything created.
//
// The four steps of PLAN.md section 2.3 run per unit inside the pod: create
// with the pod named, prepare, read the mounts in that container's own
// namespace, start. Nothing of any unit has executed until every unit has been
// verified, and a failure at any point takes the whole pod down: a Process is
// all of its units or none of them on this host.
func (s *Server) startPod(w http.ResponseWriter, r *http.Request, p store.Process, m sysusers.Member,
	req startRequest) {
	instance := r.URL.Path
	prepared, prob := s.preparePod(instance, p, m, req)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	// The name this Process is served under is proved again here, exactly as
	// a single unit start proves it: it is the face unit's name and a fact
	// about the world rather than part of the registration, see hostnames.go.
	if prob := s.proveHostname(r.Context(), instance, p); prob != nil {
		writeProblem(w, prob)
		return
	}

	// The ceiling is the Process's, as it is for a single unit, and what is
	// written into it is what the units together asked for. A unit that
	// declares nothing leaves that limit unwritten for the whole Process:
	// summing with an unbounded part would be a ceiling that is not one.
	ceiling := podCeiling(prepared)
	leaf := s.processCgroup(r.Context(), m, p, limitsOf(ceiling.Memory, ceiling.CPU, ceiling.Pids))
	parent := ""
	if leaf != "" {
		parent = cgroups.Parent(m.Name, p.ID)
	}
	ceiling.Ceiling = leaf != ""
	p.Limits = ceiling

	token, err := s.mintToken(r.Context(), p)
	if err != nil {
		writeProblem(w, problem.Internal(instance, err.Error(), ""))
		return
	}

	pod := podman.PodOptions{
		Name:         p.Composition.Pod,
		Labels:       podLabels(p),
		CgroupParent: parent,
	}
	// The port the face unit declared is published by the pod, because the
	// pod holds the network namespace every unit is in. The face unit's own
	// command line carries none, see unitOptions.
	for _, port := range req.Publish {
		pod.Publish = append(pod.Publish, podman.PortMapping{
			HostPort: port.HostPort, ContainerPort: port.ContainerPort,
		})
	}
	if _, err := s.runner.CreatePod(r.Context(), m, pod, leaf); err != nil {
		// Whatever half a pod the runtime made goes, so the next attempt
		// begins from a host with no pod of this Process on it.
		s.tearDownPod(r.Context(), p, m)
		writeProblem(w, s.runProblem(r, err, p, podman.RunOptions{}))
		return
	}

	// Every unit is created, prepared and verified before any of them is
	// started: the entrypoint of the first unit must not be running while the
	// mounts of the second are still being read.
	for i := range prepared {
		unit := prepared[i]
		envFile, err := s.writeUnitEnvFile(p, m, unit.unit.Name, req.unitEnv(unit.unit.Name), token, unit.held)
		if err != nil {
			s.tearDownPod(r.Context(), p, m)
			writeProblem(w, problem.Internal(instance, err.Error(), ""))
			return
		}
		unit.opts.EnvFile = envFile
		_, createErr := s.runner.CreateContainer(r.Context(), m, unit.opts, leaf)
		// The file is gone as soon as the container has been made with it. It
		// is not deferred to the end of the start: a pod writes one file per
		// unit, and deferring would leave the first unit's file, holding this
		// Process's live Telemetry token, on the host for as long as the rest
		// of the pod takes to come up.
		s.removeEnvFile(p.ID, envFile)
		if createErr != nil {
			s.tearDownPod(r.Context(), p, m)
			writeProblem(w, s.runProblem(r, createErr, p, unit.opts))
			return
		}
		if prob := s.prepareAndVerifyIn(r.Context(), instance, p, m,
			unit.unit.Container, leaf, unit.mounted); prob != nil {
			s.tearDownPod(r.Context(), p, m)
			writeProblem(w, prob)
			return
		}
	}

	// The face goes last. A sidecar a face talks to is up before the face
	// runs, which is the order a Package of two services is written for, and
	// nothing outside this host can reach the address until the face is up
	// anyway, see PLAN.md section 5.6.
	for _, unit := range startOrder(prepared) {
		if err := s.runner.Start(r.Context(), m, unit.unit.Container, leaf); err != nil {
			s.tearDownPod(r.Context(), p, m)
			writeProblem(w, s.runProblem(r, err, p, unit.opts))
			return
		}
	}

	s.clearProcessProblem(p.ID)
	logger.Printf("processes: %s started the pod %s with %d unit(s) for %s in %s",
		p.ID, p.Composition.Pod, len(prepared), m.Name, cgroupOrNone(leaf))
	if prob := s.trackProbeAs(r.Context(), m, p, instance); prob != nil {
		logger.Printf("health: not probing %s of %s: %s", p.ID, p.Owner, prob.Detail)
	}
	s.trackRoute(r.Context(), m, p)
	writeJSON(w, instance, startResponse{ID: p.ID, Container: p.Container})
}

// removeEnvFile removes one unit's environment file, which the start that
// wrote it owns. A file that is already gone is not a failure.
func (s *Server) removeEnvFile(id, path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		logger.Printf("processes: removing the environment file of %s: %v", id, err)
	}
}

// preparePod resolves every unit of one start before anything is created: the
// command line, the mounts as they check out now, and the owner's current
// value for each declared secret. A refusal here is a Process that was not
// touched, which is what the four step start is for in the first place.
func (s *Server) preparePod(instance string, p store.Process, m sysusers.Member,
	req startRequest) ([]preparedUnit, *problem.Problem) {
	sent := map[string]bool{}
	for _, u := range req.Units {
		if sent[u.Name] {
			return nil, problem.BadRequest(instance,
				fmt.Sprintf("the start names the unit %s twice", u.Name),
				"Send one command line per unit of this Package.")
		}
		sent[u.Name] = true
	}
	declared := map[string]bool{}
	for _, u := range p.Composition.Units {
		declared[u.Name] = true
	}
	for name := range sent {
		if !declared[name] {
			// The registration is what says which units this Process has. A
			// start that named another one would create a container the
			// registry does not describe, which is the rule the container
			// name and the image already follow, see runOptions.
			return nil, problem.BadRequest(instance,
				fmt.Sprintf("this Process has no unit %s registered", name),
				"Register the Process with the units it runs, then start it.")
		}
	}

	prepared := make([]preparedUnit, 0, len(p.Composition.Units))
	for _, unit := range p.Composition.Units {
		if !sent[unit.Name] {
			return nil, problem.BadRequest(instance,
				fmt.Sprintf("the start carries no command line for the unit %s", unit.Name),
				"Send one command line per unit of this Package.")
		}
		opts, prob := s.unitOptions(instance, p, unit, req.unit(unit.Name))
		if prob != nil {
			return nil, prob
		}
		// The mounts of this unit are resolved again, before anything is
		// created, for the reason a single unit Process resolves its own: the
		// check at registration says what was true then, see mounts.go.
		mounted, prob := s.revalidateResolved(instance, unit.Mounts, m)
		if prob != nil {
			return nil, prob
		}
		opts.Mounts = mounts.Podman(mounted)
		held, prob := s.resolveSecretNames(instance, p.Owner, unit.Secrets)
		if prob != nil {
			return nil, prob
		}
		prepared = append(prepared, preparedUnit{unit: unit, opts: opts, mounted: mounted, held: held})
	}
	return prepared, nil
}

// unitOptions is the command line of one unit: the registration's container
// and image, the pod it joins, and the limits, labels and command the start
// sent for it. It is runOptions per unit, and it publishes nothing: the port
// is the pod's, because the pod holds the network namespace.
func (s *Server) unitOptions(instance string, p store.Process, unit store.Unit,
	req startUnit) (podman.RunOptions, *problem.Problem) {
	refuse := func(detail, fix string) (podman.RunOptions, *problem.Problem) {
		return podman.RunOptions{}, problem.BadRequest(instance, detail, fix)
	}
	if unit.Digest == "" {
		return refuse(fmt.Sprintf("the unit %s is registered without an image digest", unit.Name),
			"Register the Process with the digest each unit was built to, then run it.")
	}
	opts := podman.RunOptions{
		Name:  unit.Container,
		Image: unit.Digest,
		Pod:   p.Composition.Pod,
		// Every unit is PID 1 of its own container with stdin held open, the
		// same two flags a single unit Process is started with.
		Detach:      true,
		Interactive: true,
		PidsLimit:   DefaultPidsLimit,
	}
	if req.Memory != "" {
		opts.Memory = podman.MemoryLimit(req.Memory)
		if !podman.ValidMemory(opts.Memory) {
			return refuse(fmt.Sprintf("limits.memory %q of the unit %s is not a size the container runtime takes",
				req.Memory, unit.Name),
				"Write limits.memory as a number with Ki, Mi or Gi, such as 512Mi.")
		}
	}
	if req.CPU != "" {
		if !podman.ValidCPUs(req.CPU) {
			return refuse(fmt.Sprintf("limits.cpu %q of the unit %s is not a number of cores", req.CPU, unit.Name),
				"Write limits.cpu as a number, such as 1 or 0.5.")
		}
		opts.CPUs = req.CPU
	}
	if req.Restart != "" {
		opts.Restart = req.Restart
		if !podman.ValidRestart(opts.Restart) {
			return refuse(fmt.Sprintf("restart %q of the unit %s is not a policy the container runtime takes",
				req.Restart, unit.Name),
				"Write restart as always, on-failure or never.")
		}
	}
	if req.PidsLimit != 0 {
		if req.PidsLimit < 1 || req.PidsLimit > MaxPidsLimit {
			return refuse(fmt.Sprintf("a unit may hold between 1 and %d processes, not %d",
				MaxPidsLimit, req.PidsLimit),
				fmt.Sprintf("Ask for between 1 and %d, or none and take the default of %d.",
					MaxPidsLimit, DefaultPidsLimit))
		}
		opts.PidsLimit = req.PidsLimit
	}
	labels, prob := labelsOf(instance, p, req.Labels)
	if prob != nil {
		return podman.RunOptions{}, prob
	}
	// The three labels that are this unit's rather than the Process's. The
	// digest is the unit's own image, the expose is what this unit declared,
	// which is none for every unit but the face, and the endpoint belongs to
	// the face alone because it is the address the fan out delivers to.
	labels[podman.LabelUnit] = unit.Name
	labels[podman.LabelPod] = p.Composition.Pod
	labels[podman.LabelDigest] = unit.Digest
	if !unit.Face {
		labels[podman.LabelExpose] = ExposeNone
		delete(labels, podman.LabelEndpoint)
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

// podLabels are the labels of the pod itself: the Process record, and the pod
// name, so the runtime can be asked for the parts of one Process at once. The
// endpoint is not among them, because the pod is not what the fan out delivers
// to; the face unit is.
func podLabels(p store.Process) map[string]string {
	labels := map[string]string{
		podman.LabelID:      p.ID,
		podman.LabelUser:    p.Owner,
		podman.LabelPackage: p.Package,
		podman.LabelName:    p.Name,
		podman.LabelExpose:  p.Expose,
		podman.LabelDigest:  p.Digest,
		podman.LabelPod:     p.Composition.Pod,
	}
	return labels
}

// startOrder is the units of a pod in the order they are started: everything
// that is not the face first, in the order the manifest declared them, and the
// face last.
func startOrder(prepared []preparedUnit) []preparedUnit {
	out := make([]preparedUnit, 0, len(prepared))
	for _, u := range prepared {
		if !u.unit.Face {
			out = append(out, u)
		}
	}
	for _, u := range prepared {
		if u.unit.Face {
			out = append(out, u)
		}
	}
	return out
}

// podCeiling is what the units of one Process together asked for, which is
// what goes into the Process's cgroup. A limit no unit leaves unbounded is the
// sum of what each of them declared; one that any unit leaves unbounded is not
// written at all, because a ceiling under an unbounded part is not a ceiling.
//
// Memory is summed in bytes and cpu in cores, which is a spelling the runtime
// and the cgroup files both take: internal/podman reads a plain number of
// bytes and internal/cgroups reads it as bytes too, see limitsOf.
func podCeiling(prepared []preparedUnit) store.Limits {
	var ceiling store.Limits
	var memory, pids int64
	var cpu float64
	memoryBounded, cpuBounded, pidsBounded := true, true, true
	for _, u := range prepared {
		bytes, ok := memoryBytes(u.opts.Memory)
		if !ok {
			memoryBounded = false
		}
		memory += bytes
		cores, err := strconv.ParseFloat(u.opts.CPUs, 64)
		if u.opts.CPUs == "" || err != nil {
			cpuBounded = false
		}
		cpu += cores
		if u.opts.PidsLimit <= 0 {
			pidsBounded = false
		}
		pids += int64(u.opts.PidsLimit)
	}
	if memoryBounded && memory > 0 {
		ceiling.Memory = strconv.FormatInt(memory, 10)
	}
	if cpuBounded && cpu > 0 {
		ceiling.CPU = strconv.FormatFloat(cpu, 'f', -1, 64)
	}
	if pidsBounded && pids > 0 && pids <= int64(MaxPidsLimit)*int64(MaxUnits) {
		ceiling.Pids = int(pids)
	}
	return ceiling
}

// memoryBytes reads a memory limit in the spelling podman takes, which is a
// number with an optional k, m or g, and answers it in bytes.
func memoryBytes(memory string) (int64, bool) {
	if memory == "" {
		return 0, false
	}
	scale := int64(1)
	digits := memory
	switch memory[len(memory)-1] {
	case 'k':
		scale, digits = 1<<10, memory[:len(memory)-1]
	case 'm':
		scale, digits = 1<<20, memory[:len(memory)-1]
	case 'g':
		scale, digits = 1<<30, memory[:len(memory)-1]
	}
	value, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value * scale, true
}

// tearDownPod removes the pod of one Process and every container in it. It is
// what a start that failed on one unit calls, so a Process is the whole pod or
// nothing of it on this host: a half pod would be a registration naming
// containers that never ran and an address nothing answers on.
//
// A removal that fails is logged rather than returned: the answer to the
// caller is the refusal that got here, and the next start of this Process
// begins by making the pod again.
func (s *Server) tearDownPod(ctx context.Context, p store.Process, m sysusers.Member) {
	if p.Composition.Pod == "" {
		return
	}
	if err := s.runner.RemovePod(ctx, m, p.Composition.Pod, true); err != nil &&
		!isNoPod(err) {
		logger.Printf("processes: removing the pod %s of %s: %v", p.Composition.Pod, p.Owner, err)
	}
}

// isNoPod reports a pod the runtime does not have, which is what a teardown of
// a pod that was never created finds and is not a failure of anything.
func isNoPod(err error) bool {
	return err != nil && strings.Contains(err.Error(), sysusers.ErrNoPod.Error())
}

// podStates is what each unit of one Process is doing, as the runtime reports
// it, in the order the registration declares the units. It is what
// processes_list answers for a pod and what the Process's own state is derived
// from: running when every unit is, see PLAN.md section 5.6.
func (s *Server) podStates(ctx context.Context, m sysusers.Member, p store.Process) []unitState {
	if !p.Composition.Declared() {
		return nil
	}
	states := make([]unitState, 0, len(p.Composition.Units))
	for _, unit := range p.Composition.Units {
		config, err := s.runner.ContainerConfig(ctx, m, unit.Container)
		state := ""
		if err == nil {
			state = config.State
		}
		states = append(states, unitState{Name: unit.Name, State: state})
	}
	return states
}

// unitState is one unit's name and the state the runtime calls it, in the
// runtime's own spelling. The surface's five states are kitbash-mcp's to map
// onto, the same way it maps a container's, see internal/proc.
type unitState struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// unit is the command line one start request carries for one unit, or the zero
// one for a unit it does not name, which the caller has already refused.
func (r startRequest) unit(name string) startUnit {
	for _, u := range r.Units {
		if u.Name == name {
			return u
		}
	}
	return startUnit{}
}

// unitEnv is the environment one start request carries for one unit.
func (r startRequest) unitEnv(name string) map[string]string {
	return r.unit(name).Env
}

// validateUnits holds the units of one registration to what a pod can be made
// of: two or more, each with a name, a container of the shape the runner
// writes and an image digest, exactly one of them the face, and the pod itself
// named the way a container is. A Package of one unit sends none of this.
func validateUnits(instance string, req processRequest) *problem.Problem {
	if req.Pod == "" && len(req.Units) == 0 {
		return nil
	}
	if req.Pod == "" {
		return problem.BadRequest(instance,
			"this registration carries units and no pod for them to run in",
			"Register the pod the units share, or register one unit as the Process itself.")
	}
	if !containerName.MatchString(req.Pod) {
		return problem.BadRequest(instance,
			fmt.Sprintf("%q is not a kitbash pod name", req.Pod),
			"Name the pod the way kitbash names a container.")
	}
	if len(req.Units) < 2 {
		return problem.BadRequest(instance,
			"a pod is what a Package of more than one unit runs as, and this registration carries fewer",
			"Register a Package of one unit as the container it is, without a pod.")
	}
	if len(req.Units) > MaxUnits {
		return problem.BadRequest(instance,
			fmt.Sprintf("a Package may run at most %d units, not %d", MaxUnits, len(req.Units)),
			fmt.Sprintf("Declare at most %d units in deploy.units.", MaxUnits))
	}
	names := map[string]bool{}
	containers := map[string]bool{}
	faces := 0
	for _, unit := range req.Units {
		if !manifest.ValidUnitName(unit.Name) {
			return problem.BadRequest(instance,
				fmt.Sprintf("%q is not a unit name", unit.Name),
				"Name every unit in lower case letters, digits and dashes.")
		}
		if names[unit.Name] {
			return problem.BadRequest(instance,
				fmt.Sprintf("two units of this Package are named %s", unit.Name),
				"Give every unit of a Package a name of its own.")
		}
		names[unit.Name] = true
		if !containerName.MatchString(unit.Container) {
			return problem.BadRequest(instance,
				fmt.Sprintf("%q is not a kitbash container name", unit.Container),
				"Register each unit with the container name the runtime holds it under.")
		}
		if containers[unit.Container] {
			return problem.BadRequest(instance,
				fmt.Sprintf("two units of this Package are the container %s", unit.Container),
				"Register each unit with a container name of its own.")
		}
		containers[unit.Container] = true
		if !imageDigest.MatchString(unit.Digest) {
			return problem.BadRequest(instance,
				fmt.Sprintf("%q is not an OCI image digest", unit.Digest),
				"Build the Package and register each unit with the digest it was built to.")
		}
		if unit.Face {
			faces++
		}
		if prob := validateSecrets(instance, unit.Secrets); prob != nil {
			return prob
		}
	}
	if faces != 1 {
		return problem.BadRequest(instance,
			fmt.Sprintf("%d of the %d units of this Package declare a face, and exactly one may",
				faces, len(req.Units)),
			"Declare expose as mcp or http on one unit of the Package and on no other.")
	}
	// The row's own container is the face's, because every reader that knew
	// one container per Process reads the one that answers. A registration
	// that says otherwise is one this daemon would start under two names.
	for _, unit := range req.Units {
		if unit.Face && unit.Container != req.Container {
			return problem.BadRequest(instance,
				fmt.Sprintf("this Process is registered as the container %s and its face is %s",
					req.Container, unit.Container),
				"Register the Process with the container of the unit that declares the face.")
		}
	}
	return nil
}

// composition is the pod of one registration as it goes into the store, with
// every unit's mounts resolved as root. A registration of one unit answers the
// empty composition, which is the Process it has always been.
func (s *Server) composition(ctx context.Context, instance string, owner string,
	req processRequest) (store.Composition, *problem.Problem) {
	if req.Pod == "" {
		return store.Composition{}, nil
	}
	composed := store.Composition{Pod: req.Pod, Units: make([]store.Unit, 0, len(req.Units))}
	for _, unit := range req.Units {
		// Resolved before the row is written, the same rule the Process's own
		// mounts follow: a registration never holds a unit naming a folder the
		// daemon would refuse to mount, see mounts.go.
		resolved, prob := s.resolveMounts(ctx, instance, owner, unit.Mounts)
		if prob != nil {
			return store.Composition{}, prob
		}
		composed.Units = append(composed.Units, store.Unit{
			Name:      unit.Name,
			Container: unit.Container,
			Digest:    unit.Digest,
			Face:      unit.Face,
			Mounts:    resolved,
			Secrets:   unit.Secrets,
			Limits:    store.Limits{Memory: unit.Memory, CPU: unit.CPU},
		})
	}
	return composed, nil
}

// restoredUnit is one unit of a pod as restore found it: what the registration
// says, the mounts as they check out now, the owner's current value for each
// secret, and the container's own configuration, which is where the
// environment and the command it was made with are read back from.
type restoredUnit struct {
	unit    store.Unit
	mounted []mounts.Resolved
	held    map[string]string
	config  sysusers.ContainerConfig
}

// restorePod brings one Process that runs as a pod back after a boot, and
// answers what it came to. It is restoreOwner's path for a Package of more
// than one unit and follows the same rules as the single unit one.
//
// A pod the runtime no longer has is a registration that names nothing, which
// the caller unregisters. A pod whose units are all running is read where it
// stands, because the namespaces of those containers are the only witness of
// what they hold and this daemon did not make them. Anything else is made
// again, whole: the pod and every container in it, through the four steps of
// PLAN.md section 2.3, because a start of a container that has no namespace
// yet makes the bind mounts again and the entrypoint would be running before
// anything could read them.
func (s *Server) restorePod(ctx context.Context, m sysusers.Member, p store.Process) int {
	failed := func(report restoreProblem) int {
		logger.Printf("restore: not bringing the pod %s of %s back: %s",
			p.Composition.Pod, p.Owner, report.Detail)
		s.processFailed(p.ID, report.Detail, report.Fix)
		return restoredFailed
	}
	if _, err := s.runner.PodState(ctx, m, p.Composition.Pod); err != nil {
		if isNoPod(err) {
			logger.Printf("restore: the pod %s of %s no longer exists, unregistering the Process %s",
				p.Composition.Pod, p.Owner, p.ID)
			return restoredMissing
		}
		return failed(restoreProblem{
			Detail: fmt.Sprintf("kitbashd could not read the pod %s of this Process: %v", p.Composition.Pod, err),
			Fix:    "Run the Package again, which makes the pod and every unit of it again.",
		})
	}

	// Everything is resolved before anything is started or taken apart, the
	// same order a start follows: a unit whose mount stopped being legal or
	// whose secret is gone is a Process that does not come back, and its
	// owner reads why through proc_list.
	found := make([]restoredUnit, 0, len(p.Composition.Units))
	for _, unit := range p.Composition.Units {
		mounted, prob := s.revalidateResolved("", unit.Mounts, m)
		if prob != nil {
			return failed(mountProblem(prob))
		}
		held, prob := s.resolveSecretNames("", p.Owner, unit.Secrets)
		if prob != nil {
			return failed(unresolvedSecret(prob))
		}
		config, err := s.runner.ContainerConfig(ctx, m, unit.Container)
		if err != nil {
			// A pod that has lost one of its containers cannot be made again
			// from what is left: the environment and the command of that unit
			// were the container's, and nothing else on this host remembers
			// them. The owner runs the Package again, which makes all of it.
			return failed(restoreProblem{
				Detail: fmt.Sprintf("the pod %s of this Process no longer has its unit %s, so kitbashd could not bring the Process back whole",
					p.Composition.Pod, unit.Name),
				Fix: "Run the Package again, which makes the pod and every unit of it again.",
			})
		}
		found = append(found, restoredUnit{unit: unit, mounted: mounted, held: held, config: config})
	}

	// The Process's cgroup is made again with its ceiling, before anything
	// runs: the cgroup filesystem does not survive a reboot and a pod whose
	// parent is gone does not start at all.
	leaf := s.processCgroup(ctx, m, p, limitsOf(p.Limits.Memory, p.Limits.CPU, p.Limits.Pids))

	if podRunning(found) {
		// The daemon restarted and the host did not. Every unit is read where
		// it stands, through the process it already has.
		for _, u := range found {
			if prob := s.verifyConfig("", p, u.config, u.mounted); prob != nil {
				s.tearDownPod(ctx, p, m)
				return failed(mountProblem(prob))
			}
		}
		s.clearProcessProblem(p.ID)
		return restoredRunning
	}

	if err := s.remakePod(ctx, m, p, found, leaf); err != nil {
		logger.Printf("restore: making the pod %s of %s again: %v", p.Composition.Pod, p.Owner, err)
		return failed(restoreProblem{
			Detail: fmt.Sprintf("kitbashd could not make the pod %s of this Process again: %v",
				p.Composition.Pod, err),
			Fix: "Read proc_logs for this Process and run the Package again.",
		})
	}
	s.clearProcessProblem(p.ID)
	return restoredPlaced
}

// podRunning reports whether every unit of a pod is running, with a process to
// read it through. A pid alone is not enough: a container podman init prepared
// has one and has executed nothing.
func podRunning(found []restoredUnit) bool {
	for _, u := range found {
		if u.config.PID <= 0 || u.config.State != podman.StateRunning {
			return false
		}
	}
	return len(found) > 0
}

// remakePod makes the pod of one Process and every container in it again, from
// the registration and from what each container was made with, and starts a
// unit only once its mounts have been read in its own namespace.
//
// The pod is removed first, which removes its containers: they are not
// running, their names are what the registration holds, and starting one of
// them would put its bind mounts back without anything reading them.
func (s *Server) remakePod(ctx context.Context, m sysusers.Member, p store.Process,
	found []restoredUnit, leaf string) error {
	// The token is minted again, because the containers that held the
	// previous one are about to be removed and the store keeps only its hash.
	token, hash, err := store.NewToken()
	if err != nil {
		return err
	}
	type madeUnit struct {
		unit    store.Unit
		opts    podman.RunOptions
		mounted []mounts.Resolved
	}
	made := make([]madeUnit, 0, len(found))
	var publish []podman.PortMapping
	for _, u := range found {
		image := u.unit.Digest
		if image == "" {
			image = u.config.Image
		}
		if image == "" {
			return fmt.Errorf("daemon: the unit %s of %s names no image to make %s from",
				u.unit.Name, p.ID, u.unit.Container)
		}
		opts := podman.RunOptions{
			Name:        u.unit.Container,
			Image:       image,
			Pod:         p.Composition.Pod,
			Detach:      true,
			Interactive: true,
			Restart:     u.config.Restart,
			Mounts:      mounts.Podman(u.mounted),
			Memory:      podman.MemoryLimit(u.unit.Limits.Memory),
			CPUs:        u.unit.Limits.CPU,
			PidsLimit:   u.unit.Limits.Pids,
		}
		// What the container was made to run comes back with it, but only
		// onto the image it was read off: on another digest those words are
		// as likely to be the old image's own CMD as anything the unit
		// declared, the same rule the single unit remake follows.
		if u.config.Image == image {
			opts.Command = u.config.Command
		}
		labels, prob := labelsOf("", p, healedLabels(u.config.Labels))
		if prob != nil {
			return fmt.Errorf("daemon: the labels of %s: %s", u.unit.Container, prob.Detail)
		}
		labels[podman.LabelUnit] = u.unit.Name
		labels[podman.LabelPod] = p.Composition.Pod
		labels[podman.LabelDigest] = image
		if !u.unit.Face {
			labels[podman.LabelExpose] = ExposeNone
			delete(labels, podman.LabelEndpoint)
		} else {
			// The port the pod publishes is the face's, read back off the
			// runtime rather than remembered: what a Process answers on is
			// what its container says it answers on, see routePort.
			publish = append(publish, u.config.Publish...)
		}
		opts.Labels = labels
		envFile, err := s.writeUnitEnvFile(p, m, u.unit.Name, healedEnv(u.config.Env), token, u.held)
		if err != nil {
			return err
		}
		defer s.removeEnvFile(p.ID, envFile)
		opts.EnvFile = envFile
		made = append(made, madeUnit{unit: u.unit, opts: opts, mounted: u.mounted})
	}

	if err := s.runner.RemovePod(ctx, m, p.Composition.Pod, true); err != nil && !isNoPod(err) {
		return err
	}
	remake, cancel := context.WithTimeout(ctx, RestoreTimeout)
	defer cancel()
	parent := ""
	if leaf != "" {
		parent = cgroups.Parent(m.Name, p.ID)
	}
	if _, err := s.runner.CreatePod(remake, m, podman.PodOptions{
		Name:         p.Composition.Pod,
		Labels:       podLabels(p),
		Publish:      publish,
		CgroupParent: parent,
	}, leaf); err != nil {
		s.tearDownPod(ctx, p, m)
		return err
	}
	for _, u := range made {
		if _, err := s.runner.CreateContainer(remake, m, u.opts, leaf); err != nil {
			s.tearDownPod(ctx, p, m)
			return err
		}
		if prob := s.prepareAndVerifyIn(remake, "", p, m, u.unit.Container, leaf, u.mounted); prob != nil {
			s.tearDownPod(ctx, p, m)
			return errors.New(prob.Detail)
		}
	}
	// The face goes last here too: a sidecar is up before the unit that
	// answers the address is.
	for _, face := range []bool{false, true} {
		for _, u := range made {
			if u.unit.Face != face {
				continue
			}
			if err := s.runner.Start(remake, m, u.unit.Container, leaf); err != nil {
				s.tearDownPod(ctx, p, m)
				return err
			}
		}
	}
	if err := s.store.RegisterProcess(ctx, p, hash, store.Quota{}); err != nil {
		logger.Printf("restore: recording the token of %s: %v", p.ID, err)
	}
	return nil
}
