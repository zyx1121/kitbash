package manifest

// Exposures a container unit declares, see PLAN.md section 2.3.
const (
	ExposeMCP  = "mcp"
	ExposeHTTP = "http"
	ExposeNone = "none"
)

// The lifecycle hooks a manifest declares in provides.kit, and the tool each
// hook binds to, see PLAN.md section 3. They are spelled here because they are
// manifest vocabulary: a kit declares the hook, a manifest names the kit, and
// both the bridge and the tool families read the same words.
const (
	HookImport = "import"
	HookBuild  = "build"
	HookRun    = "run"

	ToolImport = "import"
	ToolBuild  = "build"
	ToolRun    = "run"
	// ToolStop is the optional tool a run kit declares to stop a Process it
	// owns. A kit without one owns a Process kitbash cannot stop, which
	// proc_stop says rather than pretending to have stopped it.
	ToolStop = "stop"
)

// Unit types spec/manifest.schema.json allows.
const (
	UnitContainer = "container"
	UnitFiles     = "files"
)

// Restart policies, with the manifest's spelling of "do not restart". The
// container runtime spells that one "no".
const (
	RestartAlways    = "always"
	RestartOnFailure = "on-failure"
	RestartNever     = "never"
)

// reserved are the built in tool families. A Package whose name is one of them
// cannot be run, because its tools would collide with the surface itself, see
// PLAN.md section 2.3.
var reserved = map[string]bool{
	"fs": true, "pkg": true, "proc": true, "tel": true, "users": true, "approvals": true,
}

// Reserved reports whether a Package name collides with a built in tool family.
func Reserved(name string) bool { return reserved[name] }

// Tool is one entry of provides.tools. Input and Output are the raw JSON
// schemas, either inline or still a {"$ref": "schemas/x.json"} until
// ResolveSchemas has read the file.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Input       map[string]any `json:"input"`
	Output      map[string]any `json:"output"`
}

// Limits is deploy.units[].limits. Version 1 records them and passes them to
// the runtime; enforcement waits for cgroup delegation, see PLAN.md 5.5.
type Limits struct {
	CPU    string
	Memory string
}

// Unit is one entry of deploy.units, read as a container unit. Version 1 runs
// the first unit of a Package.
//
// Builder and Runner name the Package folder of a kit that builds or runs this
// unit in place of the built in path, see PLAN.md section 3. They sit beside
// Build rather than under it because build is the build context, a string, and
// a folder that names its builder still names the context that builder reads.
// A unit that names neither is built and run by kitbash itself, which is every
// manifest written before this existed.
//
// Raw is the unit as the manifest wrote it, which is what a run kit is handed:
// the kit decides what image, expose, port, env, health, limits and restart
// mean where it runs a Process, and kitbash does not translate them for it.
type Unit struct {
	Type    string
	Build   string
	Image   string
	Builder string
	Runner  string
	Expose  string
	Port    int
	Env     map[string]string
	Health  map[string]any
	Limits  Limits
	Restart string
	Raw     map[string]any
}

// HealthProbe is the HTTP probe this unit declares: the path kitbashd requests
// on the Process's endpoint, and how often it requests it, both as the
// manifest spells them. A unit that declares no health.http answers two empty
// strings, which is a Process nothing probes.
//
// health.exec is read by HealthExec and run by nobody. Version 1 probes over
// HTTP and records the answer as Telemetry; a command probe is a second
// mechanism with none of that, so the key stays in the schema, a manifest that
// declares one stays valid, and the Package is told once that it is not
// probed, see PLAN.md section 2.4.
func (u Unit) HealthProbe() (path, interval string) {
	path, _ = u.Health["http"].(string)
	interval, _ = u.Health["interval"].(string)
	return path, interval
}

// HealthExec reports whether the unit declares a command probe, which kitbash
// records and does not run.
func (u Unit) HealthExec() bool {
	command, ok := u.Health["exec"].([]any)
	return ok && len(command) > 0
}

// Tools returns provides.tools in manifest order.
func (m *Manifest) Tools() []Tool {
	provides, ok := m.Raw["provides"].(map[string]any)
	if !ok {
		return nil
	}
	list, ok := provides["tools"].([]any)
	if !ok {
		return nil
	}
	var tools []Tool
	for _, entry := range list {
		raw, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		tool := Tool{}
		tool.Name, _ = raw["name"].(string)
		tool.Description, _ = raw["description"].(string)
		tool.Input, _ = raw["input"].(map[string]any)
		tool.Output, _ = raw["output"].(map[string]any)
		tools = append(tools, tool)
	}
	return tools
}

// Tool returns one declared tool by name.
func (m *Manifest) Tool(name string) (Tool, bool) {
	for _, tool := range m.Tools() {
		if tool.Name == name {
			return tool, true
		}
	}
	return Tool{}, false
}

// Kit returns provides.kit, the lifecycle hooks this Package implements.
func (m *Manifest) Kit() []string {
	provides, ok := m.Raw["provides"].(map[string]any)
	if !ok {
		return nil
	}
	list, ok := provides["kit"].([]any)
	if !ok {
		return nil
	}
	var hooks []string
	for _, entry := range list {
		if hook, ok := entry.(string); ok {
			hooks = append(hooks, hook)
		}
	}
	return hooks
}

// Subscriptions returns provides.subscriptions, the streams this Package wants
// delivered. Only telemetry exists today, see PLAN.md section 2.4.
func (m *Manifest) Subscriptions() []string {
	provides, ok := m.Raw["provides"].(map[string]any)
	if !ok {
		return nil
	}
	list, ok := provides["subscriptions"].([]any)
	if !ok {
		return nil
	}
	var streams []string
	for _, entry := range list {
		if stream, ok := entry.(string); ok {
			streams = append(streams, stream)
		}
	}
	return streams
}

// HasKit reports whether the Package implements one lifecycle hook.
func (m *Manifest) HasKit(hook string) bool {
	for _, got := range m.Kit() {
		if got == hook {
			return true
		}
	}
	return false
}

// Unit returns deploy.units[0], the unit version 1 builds and runs.
func (m *Manifest) Unit() (Unit, bool) {
	deploy, ok := m.Raw["deploy"].(map[string]any)
	if !ok {
		return Unit{}, false
	}
	units, ok := deploy["units"].([]any)
	if !ok || len(units) == 0 {
		return Unit{}, false
	}
	raw, ok := units[0].(map[string]any)
	if !ok {
		return Unit{}, false
	}
	unit := Unit{Expose: ExposeNone, Restart: RestartAlways, Raw: raw}
	unit.Type, _ = raw["type"].(string)
	unit.Build, _ = raw["build"].(string)
	unit.Image, _ = raw["image"].(string)
	unit.Builder, _ = raw["builder"].(string)
	unit.Runner, _ = raw["runner"].(string)
	if expose, ok := raw["expose"].(string); ok && expose != "" {
		unit.Expose = expose
	}
	if restart, ok := raw["restart"].(string); ok && restart != "" {
		unit.Restart = restart
	}
	unit.Port = intOf(raw["port"])
	if env, ok := raw["env"].(map[string]any); ok {
		unit.Env = map[string]string{}
		for k, v := range env {
			if s, ok := v.(string); ok {
				unit.Env[k] = s
			}
		}
	}
	unit.Health, _ = raw["health"].(map[string]any)
	if limits, ok := raw["limits"].(map[string]any); ok {
		unit.Limits.CPU, _ = limits["cpu"].(string)
		unit.Limits.Memory, _ = limits["memory"].(string)
	}
	return unit, true
}

// intOf reads a JSON number, which the validator hands back as a json.Number.
func intOf(value any) int {
	switch n := value.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case interface{ Int64() (int64, error) }:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return 0
}
