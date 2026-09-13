package proc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// Kit is a running Process that implements one lifecycle hook, with the tool
// schemas of its manifest resolved. Dispatch binds on those schemas, never on
// a registry of kit names, see PLAN.md section 3.
//
// The type lives here rather than in internal/bridge because both this package
// and internal/pkg ask for a kit, and the bridge imports this one: a Kit that
// named a bridge type could not be asked for from here.
type Kit struct {
	Process *Process
	// Tool is the hook's own tool, the one the contract names: import, build
	// or run.
	Tool manifest.Tool
	// Tools is every tool the kit's manifest declares, so a caller can find an
	// optional one such as a run kit's stop.
	Tools []manifest.Tool
}

// Declares returns one tool of this kit by name, which is how an optional part
// of a hook contract is found.
func (k Kit) Declares(name string) (manifest.Tool, bool) {
	for _, tool := range k.Tools {
		if tool.Name == name {
			return tool, true
		}
	}
	return manifest.Tool{}, false
}

// Kits is what proc_run and proc_stop need from the MCP bridge: the kit a
// manifest named, and a way to call one of its tools. internal/bridge
// implements it, which keeps every MCP type out of this package the way
// internal/pkg keeps them out of pkg_import.
type Kits interface {
	// KitAt is the caller's running kit at one Package path. A path where
	// nothing of the caller's runs is not-found; a Package running there that
	// does not implement the hook is invalid-manifest.
	KitAt(ctx context.Context, path, hook, tool string) (*Kit, *problem.Problem)
	// CallTool calls one tool of a Process and returns its structured content.
	CallTool(ctx context.Context, p *Process, tool string, args map[string]any) (json.RawMessage, *problem.Problem)
}

// runOutput is what a run kit answers, see $package_tools.run_hook in
// spec/mcp-surface.yaml. The kit owns the Process, so all three fields are its
// own: kitbash registers what it is told and supervises none of it.
type runOutput struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Endpoint string `json:"endpoint"`
}

// runWithKit answers proc_run for a unit whose manifest names a runner. The
// kit starts the Process wherever it runs, and kitbashd registers what the kit
// answers with: the registration is what puts the Process on proc_list, mints
// its Telemetry token and carries the runner, and the runner is what keeps
// boot restore and the daemon's own start, stop and remove off a container
// this host does not have, see PLAN.md section 3.
func (s *Service) runWithKit(ctx context.Context, span *telemetry.Span, m *manifest.Manifest, folder string, unit manifest.Unit, digest, name string) (*Process, *problem.Problem) {
	if s.kits == nil {
		return nil, problem.Internal(folder,
			"this session has no MCP bridge, and a run kit is a Process reached over it",
			"Open a new session and run this Package again.")
	}
	kit, prob := s.kits.KitAt(ctx, unit.Runner, manifest.HookRun, manifest.ToolRun)
	if prob != nil {
		return nil, prob
	}
	if name == "" {
		name = m.Name
	}
	digest, prob = s.kitDigest(ctx, span, folder, digest)
	if prob != nil {
		return nil, prob
	}
	// The unit travels to the kit as the manifest wrote it: what image,
	// expose, port, env, health, limits and restart mean is the kit's to
	// decide where it runs a Process, and kitbash translates none of it.
	args := map[string]any{
		"package": folder,
		"digest":  digest,
		"name":    name,
		"unit":    unit.Raw,
	}
	if err := kit.Tool.AcceptsArgs(args); err != nil {
		return nil, problem.InvalidManifestFix(unit.Runner, fmt.Sprintf(
			"the run tool of the kit at %s does not accept what the hook is called with: %s", unit.Runner, err),
			"Give the kit's run tool an input schema that accepts package, digest, name and unit.")
	}
	raw, prob := s.kits.CallTool(ctx, kit.Process, manifest.ToolRun, args)
	if prob != nil {
		return nil, prob
	}
	out, prob := kitRun(unit.Runner, raw)
	if prob != nil {
		return nil, prob
	}

	// The endpoint is registered only when it is one kitbashd can deliver the
	// fan out to. A runner on another host answers an address the daemon must
	// never POST to, so that endpoint is the kit's to publish and is reported
	// to the caller without being registered, see validateEndpoint in
	// internal/daemon.
	endpoint := out.Endpoint
	registered := endpoint
	if !loopbackEndpoint(endpoint) {
		registered = ""
		if endpoint != "" {
			s.logger.Printf("proc: the run kit at %s answered the endpoint %s, which is not on this host's loopback address; kitbashd delivers no fan out to it",
				unit.Runner, endpoint)
		}
	}
	// The kit has started the Process by now, so a registration that fails
	// leaves one running that kitbashd has no record of. The kit is told to
	// stop it again, and what could not be undone is said in the problem, see
	// orphaned below.
	if prob := s.register(ctx, telemetry.Registration{
		ID:      out.ID,
		Package: folder,
		Name:    name,
		// No container: there is none on this host. The runner is what says
		// so, so restore skips this Process instead of unregistering it as a
		// registration that names nothing, see internal/daemon/restore.go.
		Digest:        digest,
		Expose:        unit.Expose,
		Endpoint:      registered,
		Subscriptions: m.Subscriptions(),
		Permits:       m.Permits(),
		Runner:        unit.Runner,
	}); prob != nil {
		return nil, s.orphaned(ctx, kit, unit.Runner, out.ID, prob)
	}
	// A kit converges on its own: it is called with the same Package and the
	// same name every time, and one that answers with the Process it already
	// runs has replaced nothing. One that answers with a new id has replaced
	// the Process of that name, so the registration it replaced goes: leaving
	// it would keep a live token and a proc_list entry for a Process nothing
	// runs any more.
	s.forgetReplaced(ctx, folder, name, out.ID)
	return &Process{
		ID:       out.ID,
		Name:     name,
		Package:  folder,
		Digest:   digest,
		State:    out.State,
		Expose:   unit.Expose,
		Endpoint: endpoint,
		Runner:   unit.Runner,
	}, nil
}

// stopWithKit answers proc_stop for a Process a run kit owns. A kit that
// declares no stop tool owns a Process kitbash cannot stop, which is said as
// not-permitted rather than answered with a state nothing produced.
func (s *Service) stopWithKit(ctx context.Context, id string, reg telemetry.Registered) (*StopResult, *problem.Problem) {
	if s.kits == nil {
		return nil, problem.Internal(id,
			"this session has no MCP bridge, and a run kit is a Process reached over it",
			"Open a new session and stop this Process again.")
	}
	kit, prob := s.kits.KitAt(ctx, reg.Runner, manifest.HookRun, manifest.ToolRun)
	if prob != nil {
		return nil, prob
	}
	tool, declared := kit.Declares(manifest.ToolStop)
	if !declared {
		return nil, problem.NotPermitted(id,
			fmt.Sprintf("the run kit at %s owns this Process and declares no stop tool", reg.Runner),
			fmt.Sprintf("Stop it through the kit at %s, whose runner owns it; kitbash does not supervise a Process a run kit started.", reg.Runner))
	}
	args := map[string]any{"id": id}
	if err := tool.AcceptsArgs(args); err != nil {
		return nil, problem.InvalidManifestFix(reg.Runner, fmt.Sprintf(
			"the stop tool of the kit at %s does not accept what the hook is called with: %s", reg.Runner, err),
			"Give the kit's stop tool an input schema that accepts id.")
	}
	if _, prob := s.kits.CallTool(ctx, kit.Process, manifest.ToolStop, args); prob != nil {
		return nil, prob
	}
	// The registration goes with the Process, the same as a container one:
	// what it held was a token for a Process that no longer runs.
	s.unregister(ctx, id)
	process := kitProcess(reg)
	process.State = StateStopped
	return &StopResult{ID: id, State: StateStopped, Process: &process}, nil
}

// orphaned is the answer to a registration that failed after the kit had
// already started the Process. The Process is running and nothing knows about
// it: no token, no proc_list entry, and no id proc_stop could be called with,
// so the kit is asked to stop it again and the caller is told which of the two
// happened.
//
// The problem the registration failed with is kept as it is, type and status
// and all, because that is what the caller has to fix: too many Processes, a
// daemon that is not answering, an id another member holds. What is added is
// what became of the Process the kit had started.
func (s *Service) orphaned(ctx context.Context, kit *Kit, runner, id string, prob *problem.Problem) *problem.Problem {
	answer := *prob
	tool, declared := kit.Declares(manifest.ToolStop)
	if declared {
		args := map[string]any{"id": id}
		if err := tool.AcceptsArgs(args); err != nil {
			declared = false
			s.logger.Printf("proc: the stop tool of the kit at %s does not accept an id, so the Process %s it started could not be stopped again: %v",
				runner, id, err)
		} else if _, stopProb := s.kits.CallTool(ctx, kit.Process, manifest.ToolStop, args); stopProb != nil {
			declared = false
			s.logger.Printf("proc: the kit at %s would not stop the Process %s it had just started: %s",
				runner, id, stopProb.Detail)
		}
	}
	if !declared {
		answer.Detail = fmt.Sprintf(
			"%s; the run kit at %s had already started the Process %s, which is still running with no registration",
			prob.Detail, runner, id)
		answer.Fix = fmt.Sprintf(
			"Tell the kit at %s to stop %s yourself, then fix what refused the registration and run this Package again.",
			runner, id)
		return &answer
	}
	answer.Detail = fmt.Sprintf(
		"%s; the run kit at %s had already started the Process %s, which was stopped again through the kit",
		prob.Detail, runner, id)
	answer.Fix = fmt.Sprintf("%s Nothing of this Package is left running.", registrationFix(prob))
	return &answer
}

// registrationFix is the advice the failed registration carried, so the fix
// that names what became of the Process still says how to make the next run
// work.
func registrationFix(prob *problem.Problem) string {
	if prob.Fix == "" {
		return "Fix what refused the registration and run this Package again."
	}
	return prob.Fix
}

// forgetReplaced unregisters the caller's other kit owned Processes of one
// Package and name, which is what a run that answered a new id replaced.
func (s *Service) forgetReplaced(ctx context.Context, folder, name, id string) {
	if s.registry == nil {
		return
	}
	known, prob := s.registry.ListProcesses(ctx)
	if prob != nil {
		return
	}
	for _, entry := range known {
		if entry.ID == id || entry.Runner == "" || !s.ownedByCaller(entry) {
			continue
		}
		if entry.Package == folder && entry.Name == name {
			s.logger.Printf("proc: the run kit at %s answered %s for %s, which replaces the Process %s",
				entry.Runner, id, name, entry.ID)
			s.unregister(ctx, entry.ID)
		}
	}
}

// kitOwned is the registration of one Process a run kit owns, and false for
// every id that is a container on this host or nothing at all.
func (s *Service) kitOwned(ctx context.Context, id string) (telemetry.Registered, bool) {
	if s.registry == nil {
		return telemetry.Registered{}, false
	}
	known, prob := s.registry.ListProcesses(ctx)
	if prob != nil {
		// The registry is not answering, so this is a Process the runtime
		// answers for or one nothing answers for, which is what the caller
		// reports next.
		return telemetry.Registered{}, false
	}
	for _, entry := range known {
		if entry.ID == id && entry.Runner != "" && s.ownedByCaller(entry) {
			return entry, true
		}
	}
	return telemetry.Registered{}, false
}

// kitOwnedProcesses are the caller's Processes that a run kit owns, out of
// what the registry answered. They have no container here, so the registry is
// the only thing that knows they exist: proc_list reads them from it and
// merges them with the runtime's own.
//
// The listing is passed in rather than fetched again: one proc_list is one
// call to kitbashd, which also answers why a Process did not come back and
// what its last health probe saw, see Service.registered.
func (s *Service) kitOwnedProcesses(known map[string]telemetry.Registered, listed []Process) []Process {
	here := map[string]bool{}
	for _, p := range listed {
		here[p.ID] = true
	}
	var out []Process
	for _, entry := range known {
		if entry.Runner == "" || here[entry.ID] || !s.ownedByCaller(entry) {
			continue
		}
		out = append(out, kitProcess(entry))
	}
	return out
}

// ownedByCaller reports whether a registration is this member's. An admin's
// list is the whole machine, and another member's Process is not one this
// session may name.
func (s *Service) ownedByCaller(entry telemetry.Registered) bool {
	return entry.Owner == "" || entry.Owner == s.files.User()
}

// kitProcess is one registration read back as the Process the surface
// publishes. It is listed running for as long as it is registered: the kit
// answered that it started it, kitbashd does not supervise it and asks it
// nothing afterwards, and proc_stop is what takes the registration away.
func kitProcess(entry telemetry.Registered) Process {
	return Process{
		ID:        entry.ID,
		Name:      entry.Name,
		Package:   entry.Package,
		Digest:    entry.Digest,
		State:     StateRunning,
		Expose:    entry.Expose,
		Endpoint:  entry.Endpoint,
		Runner:    entry.Runner,
		StartedAt: entry.RegisteredAt,
	}
}

// kitDigest is the version the kit is asked to run. A digest the caller named
// is passed on as it stands: the image of a Package a kit runs may live
// wherever the kit runs it, and this member's own store is not the record of
// what exists there. Without one the newest local build is the answer, which
// is what the built in runner does.
func (s *Service) kitDigest(ctx context.Context, span *telemetry.Span, folder, digest string) (string, *problem.Problem) {
	if digest != "" {
		return digest, nil
	}
	image, prob := s.image(ctx, span, folder, "")
	if prob != nil {
		return "", prob
	}
	return image.ID, nil
}

// kitRun decodes what a run kit answered and applies the rules of the hook
// before the Process is registered.
func kitRun(path string, raw json.RawMessage) (runOutput, *problem.Problem) {
	var out runOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, problem.InternalDetail(path, err.Error(),
			"the run kit returned output that is not a Process",
			"Ask the kit's author to return the id, state and endpoint its manifest declares.")
	}
	// The id is the Process id everywhere else in the system: kitbashd
	// registers it, Telemetry carries it and proc_stop names it. A kit that
	// invents another shape would register a Process no other tool could
	// address.
	if !uuid.Valid(out.ID) {
		return out, problem.InternalDetail(path,
			fmt.Sprintf("the run kit returned the id %q", out.ID),
			"the run kit returned an id that is not a version 7 UUID",
			"Ask the kit's author to answer with a UUIDv7 as the Process id.")
	}
	switch out.State {
	case StateStarting, StateRunning, StateUnhealthy, StateStopped, StateFailed:
	default:
		return out, problem.InternalDetail(path,
			fmt.Sprintf("the run kit returned the state %q", out.State),
			"the run kit returned a state the surface does not publish",
			"Ask the kit's author to answer with one of starting, running, unhealthy, stopped or failed.")
	}
	return out, nil
}

// loopbackEndpoint reports whether an endpoint is one kitbashd would accept on
// a registration: http on this host's loopback address and nothing else, which
// is the only place the fan out is ever POSTed to.
func loopbackEndpoint(endpoint string) bool {
	if endpoint == "" {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host, _, err := net.SplitHostPort(u.Host)
	return err == nil && host == "127.0.0.1"
}
