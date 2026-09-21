package proc

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// A Package that declares more than one unit runs as one podman pod, see
// PLAN.md section 5.6. It is one Process: one registration, one token, one
// address, one line in every listing. What changes is that the container is
// several, and that the unit which declares the face is the one the Process is
// reached, probed and routed by.
//
// kitbashd makes the pod and every container in it. This file is the session's
// half: it reads the units off the manifest, finds the image of each, names
// the pod and its containers, and sends both the registration and the start.

// UnitContainerName is the runtime name of one unit of a pod: the pod's own
// name and the unit's, so a member reading podman ps sees which Process each
// container belongs to and which part of it each one is.
func UnitContainerName(pod, unit string) string { return pod + "-" + unit }

// runPod starts a Package of more than one unit as one pod. It is the run path
// of a composed Package and follows the same order the single unit one does:
// every check that can be made without the runtime is made before anything is
// removed, the Process is registered before the pod is made, and a start that
// fails unregisters what it registered.
func (s *Service) runPod(ctx context.Context, span *telemetry.Span, m *manifest.Manifest, folder string,
	units []manifest.Unit, digest, name string) (*Process, *problem.Problem) {
	for _, unit := range units {
		if unit.Type != manifest.UnitContainer {
			return nil, problem.InvalidManifestFix(folder,
				fmt.Sprintf("the unit %s is a %s unit, and a Package of several units runs containers",
					unit.Name, unit.Type),
				"Declare every unit of a composed Package as type: container.")
		}
		if unit.Runner != "" {
			// A run kit owns a whole Process, not one container of one. What
			// a kit would be handed here is half a pod.
			return nil, problem.InvalidManifestFix(folder,
				fmt.Sprintf("the unit %s names a run kit, and a Package of several units runs as one pod on this host",
					unit.Name),
				"Name a runner on a Package of one unit, or declare one unit for this Package.")
		}
		if unit.Scheduled() {
			return nil, problem.InvalidManifestFix(folder,
				fmt.Sprintf("the unit %s declares a schedule, and a job is one container kitbashd starts at each tick",
					unit.Name),
				"Declare a schedule on a Package of one unit.")
		}
	}
	face, faceAt, found := faceUnit(units)
	if !found {
		// The manifest layer refuses this already; a Manifest built in code
		// has not been through it, and a pod with no face has no address.
		return nil, problem.InvalidManifest(folder,
			"exactly one unit declares expose as mcp or http, which is the face of the Process, and none of these units does")
	}
	if name == "" {
		name = m.Name
	}
	pod := ContainerName(m.Name, name)
	containers := make([]string, len(units))
	for i, unit := range units {
		containers[i] = UnitContainerName(pod, unit.Name)
	}

	// The image of each unit, which is one per unit: pkg_build builds every
	// unit of a Package and labels each image with the unit it was built for,
	// see internal/pkg. The digest a caller named is the face's, because that
	// is the one digest the surface answers with.
	images := make([]string, len(units))
	for i, unit := range units {
		want := ""
		if i == faceAt {
			want = digest
		}
		image, prob := s.unitImage(ctx, span, folder, unit, want)
		if prob != nil {
			return nil, builtElsewhere(prob, unit)
		}
		images[i] = image
	}

	// The whole command line is built and checked before anything is removed,
	// the same rule the single unit path follows.
	starts := make([]telemetry.StartUnit, 0, len(units))
	registered := make([]telemetry.RegUnit, 0, len(units))
	id := uuid.V7()
	for i, unit := range units {
		opts := podman.RunOptions{
			Name:    containers[i],
			Image:   images[i],
			Env:     unit.Environment,
			Command: unit.Command,
			Restart: restartPolicy(unit.Restart),
			CPUs:    unit.Limits.CPU,
			Memory:  podman.MemoryLimit(unit.Limits.Memory),
		}
		if prob := checkOptions(folder, opts); prob != nil {
			return nil, prob
		}
		if prob := s.checkMounts(folder, unit.Mounts); prob != nil {
			return nil, prob
		}
		starts = append(starts, telemetry.StartUnit{
			Name:    unit.Name,
			Labels:  s.unitLabels(id, folder, name, images[i], unit, i == faceAt, pod),
			Env:     ownEnv(unit.Environment),
			Restart: opts.Restart,
			CPU:     opts.CPUs,
			Memory:  opts.Memory,
			Command: unit.Command,
		})
		registered = append(registered, telemetry.RegUnit{
			Name:      unit.Name,
			Container: containers[i],
			Digest:    images[i],
			Face:      i == faceAt,
			Mounts:    unit.Mounts,
			Secrets:   unit.Secrets,
			Memory:    opts.Memory,
			CPU:       opts.CPUs,
		})
	}

	// The host port is chosen here for the same reason a single unit's is:
	// the endpoint has to be known before the Process is registered. It is
	// published by the pod, because the pod holds the network namespace every
	// unit is in.
	var endpoint string
	var publish []telemetry.PortMapping
	if face.Expose == manifest.ExposeHTTP {
		if face.Port <= 0 {
			return nil, problem.InvalidManifestFix(folder,
				"expose: http declares no port, so the Process has no endpoint to be reached on",
				"Give the unit that declares the face the port its container listens on.")
		}
		mapping := telemetry.PortMapping{ContainerPort: face.Port}
		if host, err := freePort(); err == nil {
			mapping.HostPort = host
			endpoint = "http://127.0.0.1:" + strconv.Itoa(host)
			starts[faceAt].Labels[podman.LabelEndpoint] = endpoint
		} else {
			s.logger.Printf("proc: choosing a host port for %s: %v", folder, err)
		}
		publish = []telemetry.PortMapping{mapping}
	}

	existing, prob := s.byName(ctx, containers[faceAt])
	if prob != nil {
		return nil, prob
	}
	var replaced *Process
	if existing != nil {
		if owner := existing.Labels[podman.LabelPackage]; owner != folder {
			return nil, problem.ConflictFix(folder, fmt.Sprintf(
				"a Process named %s already exists for %s", name, owner),
				"Pass a different name to proc_run, or stop that Process first.")
		}
		if existing.Labels[podman.LabelDigest] == images[faceAt] && existing.State == podman.StateRunning {
			// Already converged, which is the invariant of PLAN.md 2.6.
			process := s.describe(*existing, face)
			return &process, nil
		}
		previous := s.describe(*existing, face)
		replaced = &previous
		// Removing the Process removes the whole pod, because kitbashd knows
		// from the registration that this Process is one, see run.go.
		if prob := s.remove(ctx, previous.ID, containers[faceAt]); prob != nil {
			return nil, prob
		}
		s.unregister(ctx, previous.ID)
	}

	prob = s.register(ctx, telemetry.Registration{
		ID:      id,
		Package: folder,
		Name:    name,
		// The row's own container, image and exposure are the face's, so
		// everything that knew one container per Process reads the one that
		// answers: the proxy, the health probe and proc_logs, see PLAN.md 5.6.
		Container:     containers[faceAt],
		Digest:        images[faceAt],
		Expose:        face.Expose,
		Endpoint:      endpoint,
		Hostname:      face.Hostname,
		Subscriptions: m.Subscriptions(),
		Permits:       m.Permits(),
		Health:        s.health(folder, face),
		// The mounts and the secrets of a composed Package are per unit and
		// travel with the units below, so the Process's own lists are empty:
		// one declaration in two places is two declarations.
		Pod:   pod,
		Units: registered,
	})
	if prob != nil {
		return nil, prob
	}
	orphan := func() {
		s.logger.Printf("proc: Process %s did not start; unregistering it", id)
		s.unregister(ctx, id)
	}

	if _, prob := s.registry.StartProcess(ctx, id, telemetry.StartOptions{
		Container: containers[faceAt],
		Image:     images[faceAt],
		Labels:    starts[faceAt].Labels,
		Publish:   publish,
		Units:     starts,
	}); prob != nil {
		orphan()
		return nil, prob
	}

	started, prob := s.byName(ctx, containers[faceAt])
	if prob != nil {
		orphan()
		return nil, prob
	}
	if started == nil {
		orphan()
		return nil, problem.Internal(folder, "the pod was started and its face is not in the container list", "")
	}
	process := s.describe(*started, face)
	process.Replaced = replaced
	if held, ok := s.registered(ctx)[process.ID]; ok {
		process.Mounts = held.Mounts
		process.URL = processURL(held.Host)
		process.Units = unitLines(held.Units)
		process.State = worstState(process.State, process.Units)
	}
	return &process, nil
}

// faceUnit is the unit that declares the Process's face and where it is in the
// manifest's order. Exactly one unit of a composed Package declares one, which
// the manifest layer is what enforces, see internal/manifest.
func faceUnit(units []manifest.Unit) (manifest.Unit, int, bool) {
	for i, unit := range units {
		if unit.Expose == manifest.ExposeMCP || unit.Expose == manifest.ExposeHTTP {
			return unit, i, true
		}
	}
	return manifest.Unit{}, 0, false
}

// unitLabels are the labels of one unit's container: the Process record, plus
// the two that say which unit of which pod this container is. The digest is
// the unit's own image and the exposure is the unit's own, which is none for
// every unit but the face.
func (s *Service) unitLabels(id, folder, name, digest string, unit manifest.Unit, face bool,
	pod string) map[string]string {
	expose := manifest.ExposeNone
	if face {
		expose = unit.Expose
	}
	return map[string]string{
		podman.LabelID:      id,
		podman.LabelUser:    s.files.User(),
		podman.LabelPackage: folder,
		podman.LabelName:    name,
		podman.LabelDigest:  digest,
		podman.LabelExpose:  expose,
		podman.LabelUnit:    unit.Name,
		podman.LabelPod:     pod,
	}
}

// unitImage is the image of one unit: the digest the caller named for the
// face, or the newest build of that unit. Every unit of a Package is built and
// its image carries the unit it was built for, so the store is asked for that
// unit's images and not for the Package's, see internal/pkg.
func (s *Service) unitImage(ctx context.Context, span *telemetry.Span, folder string, unit manifest.Unit,
	digest string) (string, *problem.Problem) {
	filter := podman.Filter{podman.LabelPath: folder, podman.LabelUnit: unit.Name}
	images, err := s.runner.Images(ctx, filter)
	if err != nil {
		return "", problem.Internal(folder, err.Error(), "")
	}
	if digest != "" {
		for i := range images {
			if images[i].ID == digest {
				return images[i].ID, nil
			}
		}
		if image := s.fetch(ctx, folder, digest); image != nil {
			return image.ID, nil
		}
		return "", problem.NotFoundFix(folder,
			fmt.Sprintf("no build of the unit %s of this Package has the digest %s", unit.Name, digest),
			"Call pkg_inspect to see the digests this Package has been built to.")
	}
	if len(images) == 0 {
		return "", problem.NotFoundFix(folder,
			fmt.Sprintf("the unit %s of this Package has not been built yet", unit.Name),
			"Call pkg_build first, then run the digest it returns.")
	}
	return s.latest(ctx, span, folder, images).ID, nil
}

// unitLines is the units of one Process as the surface publishes them: the
// name each carries and the state it is in, mapped onto the five states the
// surface speaks the way a container's state is.
func unitLines(units []telemetry.UnitState) []UnitLine {
	if len(units) == 0 {
		return nil
	}
	out := make([]UnitLine, 0, len(units))
	for _, u := range units {
		out = append(out, UnitLine{Name: u.Name, State: State(podman.Container{State: u.State})})
	}
	return out
}

// worstState is the state of a Process that runs as a pod: running when every
// unit is running, and otherwise the state of the unit that is furthest from
// it. A member reading proc_list sees one state for the Process and the list
// beside it says which unit is down, see PLAN.md section 5.6.
//
// The order is what a member would act on first: a unit that failed is a
// Process to read the logs of, one that is stopped is a Process to run again,
// one that is unhealthy or starting is a Process to look at again in a moment.
func worstState(face string, units []UnitLine) string {
	worst := face
	for _, u := range units {
		if stateRank(u.State) > stateRank(worst) {
			worst = u.State
		}
	}
	return worst
}

// stateRank orders the states of the surface from running to failed.
func stateRank(state string) int {
	switch state {
	case StateRunning:
		return 0
	case StateScheduled:
		return 1
	case StateStarting:
		return 2
	case StateUnhealthy:
		return 3
	case StateStopped:
		return 4
	case StateFailed:
		return 5
	}
	return 5
}

// podUnits groups the containers of one listing by the Process they belong to,
// and answers the units of each pod by name. A container with no unit label is
// a Process of one unit and is not in it, which is what keeps a host of single
// unit Processes listed exactly as it was.
func podUnits(containers []podman.Container) map[string][]UnitLine {
	grouped := map[string][]UnitLine{}
	for _, container := range containers {
		unit := container.Labels[podman.LabelUnit]
		id := container.Labels[podman.LabelID]
		if unit == "" || id == "" {
			continue
		}
		grouped[id] = append(grouped[id], UnitLine{Name: unit, State: State(container)})
	}
	for id := range grouped {
		sort.Slice(grouped[id], func(i, j int) bool { return grouped[id][i].Name < grouped[id][j].Name })
	}
	return grouped
}

// faceContainers is one container per Process of a listing: the face of a pod,
// and the container itself for a Process of one unit. It is what a listing
// describes, because a pod is one Process and one line, see PLAN.md 5.6.
//
// The face of a pod is the container whose exposure is not none, which is
// exactly one of them. A pod whose face is not in the listing at all is
// described by the unit that sorts first, so a Process whose face container
// was removed by hand is still listed rather than silently gone.
func faceContainers(containers []podman.Container) []podman.Container {
	var faces []podman.Container
	pods := map[string]podman.Container{}
	var order []string
	for _, container := range containers {
		if container.Labels[podman.LabelUnit] == "" {
			faces = append(faces, container)
			continue
		}
		id := container.Labels[podman.LabelID]
		held, seen := pods[id]
		if !seen {
			order = append(order, id)
			pods[id] = container
			continue
		}
		if isFace(container) || (!isFace(held) && container.Name < held.Name) {
			pods[id] = container
		}
	}
	for _, id := range order {
		faces = append(faces, pods[id])
	}
	return faces
}

// unitContainer is the container of one Process a caller asked to read: the
// unit they named, or the face when they named none. proc_logs is what asks,
// and the face is its default because that unit is what the Process answers
// for, see PLAN.md section 5.6.
//
// A name that is not a unit of this Process is not-found saying which units it
// has, so the next call is the right one. A Package of one unit has no unit to
// name: it is the Package itself, and a name given for it is not-found too.
func unitContainer(id string, containers []podman.Container, unit string) (*podman.Container, *problem.Problem) {
	if unit == "" {
		return &faceContainers(containers)[0], nil
	}
	names := make([]string, 0, len(containers))
	for i := range containers {
		name := containers[i].Labels[podman.LabelUnit]
		if name == "" {
			continue
		}
		if name == unit {
			return &containers[i], nil
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, problem.NotFoundFix(id,
			fmt.Sprintf("this Process runs one unit, which is the Package itself, so it has none named %q", unit),
			"Call proc_logs without a unit.")
	}
	sort.Strings(names)
	return nil, problem.NotFoundFix(id,
		fmt.Sprintf("this Process has no unit named %q", unit),
		fmt.Sprintf("Name one of its units: %s.", strings.Join(names, ", ")))
}

// infra reports whether one container is the infra container podman makes for
// a pod: it holds the namespaces and the published port and runs nothing of
// the Package. podman gives it the pod's own labels, so it carries the
// Process's id and the pod's name and is the one container of a pod that
// names no unit. It is not a unit and belongs in no listing of them.
func infra(container podman.Container) bool {
	return container.Labels[podman.LabelPod] != "" && container.Labels[podman.LabelUnit] == ""
}

// isFace reports whether one container of a pod is the unit that declares the
// Process's face, which is the one whose exposure is mcp or http.
func isFace(container podman.Container) bool {
	switch container.Labels[podman.LabelExpose] {
	case manifest.ExposeMCP, manifest.ExposeHTTP:
		return true
	}
	return false
}
