package podman

import "regexp"

// The restart policies the runtime takes. The manifest spells "never" and the
// runtime spells "no", which restartPolicy in internal/proc converts.
const (
	RestartAlways    = "always"
	RestartOnFailure = "on-failure"
	RestartNo        = "no"
)

// memorySuffixes maps the manifest's Kubernetes style memory suffix onto the
// runtime's own. spec/manifest.schema.json allows Ki, Mi and Gi only, so the
// table is the whole conversion.
var memorySuffixes = map[string]string{"Ki": "k", "Mi": "m", "Gi": "g"}

// MemoryLimit converts one limits.memory value onto the spelling the runtime
// takes. An unknown suffix is passed through for the runtime to reject, which
// the manifest schema already prevents from happening, and a value that is
// already converted is returned as it is: the session converts before it sends
// the value to kitbashd, and kitbashd converts again for a client that did
// not.
func MemoryLimit(memory string) string {
	if len(memory) < 3 {
		return memory
	}
	suffix, ok := memorySuffixes[memory[len(memory)-2:]]
	if !ok {
		return memory
	}
	return memory[:len(memory)-2] + suffix
}

// ValidMemory and ValidCPUs report whether a converted limit is one the
// runtime takes. They are the second belt under spec/manifest.schema.json:
// kitbash-mcp checks before it removes a running Process, and kitbashd checks
// again before it builds a command line, because a request body is not a
// manifest.
func ValidMemory(memory string) bool { return memoryValue.MatchString(memory) }

// ValidCPUs reports whether a limits.cpu value is a number of cores.
func ValidCPUs(cpus string) bool { return cpuValue.MatchString(cpus) }

// ValidRestart reports whether a restart policy is one the runtime takes. The
// manifest's "never" is not: it is converted first.
func ValidRestart(restart string) bool {
	switch restart {
	case RestartAlways, RestartOnFailure, RestartNo:
		return true
	}
	return false
}

var (
	memoryValue = regexp.MustCompile(`^[0-9]+[kmg]?$`)
	cpuValue    = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
)
