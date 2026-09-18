package manifest

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

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
	"fs": true, "pkg": true, "proc": true, "tel": true, "users": true,
	"approvals": true, "secrets": true,
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

// MaxSecrets is how many names one unit may declare, see PLAN.md section 2.3.
// A container that needs more than sixteen credentials is a container that
// should be reading a file its unit mounts.
const MaxSecrets = 16

// OwnedEnvPrefix is what kitbashd speaks for in a Process's environment: the
// Process's identity and its two credentials are all KITBASH_ names, so a
// secret may not be one, see PLAN.md section 2.3 and internal/daemon's
// ownedEnv, which this prefix covers one for one.
const OwnedEnvPrefix = "KITBASH_"

// secretName is the shape of a declared secret: an environment variable name
// in the spelling a credential is written in, which is also what
// spec/manifest.schema.json holds the array to.
var secretName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// ValidSecretName reports whether a name is one kitbash carries as a secret.
// It is spelled here, beside Mount and Permits, because it is manifest
// vocabulary: a unit declares the name, internal/secrets holds the value under
// it, and two spellings of the rule would be two rules.
func ValidSecretName(name string) bool { return secretName.MatchString(name) }

// checkSecrets is the part of the secrets rule no JSON Schema can express: a
// name the unit's own env also sets, and a name kitbashd speaks for. The shape
// of a name, the count and the duplicates are in spec/manifest.schema.json,
// which has already run when this does.
//
// A collision is refused rather than resolved because either resolution is a
// surprise: dropping the secret starts a Process without the credential it
// declared, and dropping the env entry drops a line of the manifest that is
// written in front of the member. Both names are theirs to change.
func checkSecrets(raw map[string]any) []string {
	var messages []string
	for i, unit := range units(raw) {
		env := map[string]bool{}
		if declared, ok := unit["env"].(map[string]any); ok {
			for key := range declared {
				env[key] = true
			}
		}
		list, ok := unit["secrets"].([]any)
		if !ok {
			continue
		}
		for j, entry := range list {
			name, ok := entry.(string)
			if !ok {
				continue
			}
			where := fmt.Sprintf("/deploy/units/%d/secrets/%d", i, j)
			if strings.HasPrefix(name, OwnedEnvPrefix) {
				messages = append(messages, fmt.Sprintf(
					"%s: %s is a name kitbashd speaks for, so it is not one a member sets", where, name))
			}
			if env[name] {
				messages = append(messages, fmt.Sprintf(
					"%s: %s is also set by deploy.units[%d].env, so the unit declares it twice", where, name, i))
			}
		}
	}
	sort.Strings(messages)
	return messages
}

// units is deploy.units as the document carries it, for a check that reads
// every unit rather than the first one version 1 runs: a manifest is refused
// as a whole, so a second unit with a name it may not declare is refused too.
func units(raw map[string]any) []map[string]any {
	deploy, ok := raw["deploy"].(map[string]any)
	if !ok {
		return nil
	}
	list, ok := deploy["units"].([]any)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, entry := range list {
		if unit, ok := entry.(map[string]any); ok {
			out = append(out, unit)
		}
	}
	return out
}

// Mount is one entry of deploy.units[].mounts: a folder of Files the unit
// asks to see, where the container sees it, and whether it may be written. An
// empty mode is ro, which is the schema's default.
//
// It lives here because it is what a manifest declares, the same reason
// Permits does. Deciding whether a member may have it is somebody else's, see
// internal/mounts: that package resolves this one as root and answers a
// resolved mount, and it reads this type rather than declaring its own so a
// manifest and a registration cannot drift apart.
type Mount struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Mode   string `json:"mode,omitempty"`
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
	// Mounts are the folders of Files this unit asks to see, at most four,
	// see PLAN.md section 2.3. They are read as the manifest wrote them:
	// kitbashd resolves them as root and is the one that decides.
	Mounts []Mount
	// Secrets are the environment variable names this unit needs and does not
	// get from the image, from Env or from kitbashd, at most MaxSecrets of
	// them. Only the names are here and only the names ever travel: the
	// values live with kitbashd, and a start resolves each name to the
	// owner's current value, see PLAN.md section 2.3.
	Secrets []string
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
	if list, ok := raw["mounts"].([]any); ok {
		for _, entry := range list {
			declared, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			mount := Mount{}
			mount.Source, _ = declared["source"].(string)
			mount.Target, _ = declared["target"].(string)
			mount.Mode, _ = declared["mode"].(string)
			unit.Mounts = append(unit.Mounts, mount)
		}
	}
	if list, ok := raw["secrets"].([]any); ok {
		for _, entry := range list {
			if name, ok := entry.(string); ok {
				unit.Secrets = append(unit.Secrets, name)
			}
		}
	}
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
