// Package podman is the rootless container runtime kitbash builds and runs
// Packages with. It is one of the two things PLAN.md section 3 builds into the
// daemon, so it is an interface with one CLI implementation rather than a kit.
//
// The runner holds no MCP types and returns plain errors. Every error carries
// the runtime's own output, which is for the server log only: callers turn it
// into problem.Internal, never into a detail the agent reads.
package podman

import (
	"context"
	"errors"
	"time"
)

// ErrBuildFailed reports a build that ran and failed on its own terms: the
// Containerfile or the build context is wrong. It is told apart from a runtime
// that could not be started at all, because the first is the caller's to fix
// and the second is the operator's.
var ErrBuildFailed = errors.New("the build failed")

// Binary is the container runtime kitbash shells out to, found on PATH.
const Binary = "podman"

// The labels every image kitbash builds and every container it runs carries.
// The image store and the container list are the build and process records, so
// these labels are the whole schema, see PLAN.md sections 2.2 and 2.3.
const (
	LabelPath    = "kitbash.path"
	LabelName    = "kitbash.name"
	LabelCommit  = "kitbash.commit"
	LabelUser    = "kitbash.user"
	LabelID      = "kitbash.id"
	LabelPackage = "kitbash.package"
	LabelDigest  = "kitbash.digest"
	LabelExpose  = "kitbash.expose"
	// LabelEndpoint is the internal URL of a Process with expose: http. It is
	// on the container because the host port is chosen before the container
	// exists, so a later session re-registers the Process with the endpoint it
	// was started on rather than guessing one.
	LabelEndpoint = "kitbash.endpoint"
)

// Container states the runtime reports, lowercased.
const (
	StateRunning    = "running"
	StateCreated    = "created"
	StateConfigured = "configured"
	StateExited     = "exited"
	StateStopped    = "stopped"
	StateStopping   = "stopping"
	StateRemoving   = "removing"
	StatePaused     = "paused"
)

// Filter selects images or containers by label. An empty filter matches
// everything.
type Filter map[string]string

// Image is one image in the caller's store.
//
// Created is when the image was built. podman images reports it at one second
// resolution, which is not enough to order two builds of one Package that
// landed in the same second, so a listing refines the images that share a
// second with an inspect, which reports nanoseconds. A runner that cannot
// report more than a second leaves those images sharing an instant, and the
// caller breaks the tie itself.
type Image struct {
	ID      string
	Created time.Time
	Labels  map[string]string
}

// Port is one published port of a container.
type Port struct {
	HostIP        string
	HostPort      int
	ContainerPort int
	Protocol      string
}

// PortMapping is one port to publish. HostPort 0 leaves the choice to the
// runtime; a caller that has to know the endpoint before the container exists
// names the host port itself.
type PortMapping struct {
	HostPort      int
	ContainerPort int
}

// Container is one container in the caller's runtime, running or not.
type Container struct {
	ID        string
	Name      string
	State     string
	ExitCode  int
	StartedAt time.Time
	Labels    map[string]string
	Image     string
	Ports     []Port
}

// RunOptions is one container start. Detach and Interactive together are what
// keeps a stdio MCP server alive as PID 1: the container runs detached with
// stdin held open, and every session execs another instance beside it.
//
// The last four fields are kitbashd's, not a session's: a member cannot place
// their own container in a delegated cgroup, so the daemon runs podman for
// them, see internal/cgroups and PLAN.md section 2.3.
type RunOptions struct {
	Name        string
	Image       string
	Labels      map[string]string
	Env         map[string]string
	Restart     string
	CPUs        string
	Memory      string
	PidsLimit   int
	Publish     []PortMapping
	Detach      bool
	Interactive bool
	// CgroupParent is the cgroup the container's own one is created under,
	// as an absolute path below the mount point. Empty leaves the runtime's
	// default in place, which is a host that enforces no limits.
	CgroupParent string
	// EnvFile is a file of KEY=value lines podman reads the environment out
	// of. A caller that sets it has written the file itself, so Run passes it
	// through and removes nothing; one that leaves it empty gets a file
	// written and removed around the call.
	EnvFile string
}

// Runner is the container runtime kitbash drives. The CLI implementation talks
// to podman; tests use Fake.
type Runner interface {
	// Build builds contextDir with containerfile and returns the image ID as
	// sha256:<64 hex> together with the combined build log.
	Build(ctx context.Context, contextDir, containerfile, tag string, labels map[string]string) (string, string, error)
	// Images lists the images whose labels match every entry of the filter.
	Images(ctx context.Context, filter Filter) ([]Image, error)
	// ImageEntrypoint reads the entrypoint and command of an image, which is
	// what the bridge execs to open one more MCP session.
	ImageEntrypoint(ctx context.Context, ref string) ([]string, []string, error)
	// Tag names an image the caller already has. A build tags what it builds;
	// this is for an image that arrived some other way, which is one kitbashd
	// copied out of another member's store, see PLAN.md section 2.2.
	Tag(ctx context.Context, image, tag string) error
	// Run starts one container and returns its ID.
	Run(ctx context.Context, opts RunOptions) (string, error)
	// Containers lists containers whose labels match the filter. With all set,
	// stopped containers are listed too.
	Containers(ctx context.Context, filter Filter, all bool) ([]Container, error)
	// Stop stops a container by name, waiting timeout seconds before killing it.
	Stop(ctx context.Context, name string, timeout int) error
	// Remove removes a container by name.
	Remove(ctx context.Context, name string, force bool) error
	// Logs returns the last tail lines of a container's stdout and stderr.
	Logs(ctx context.Context, name string, tail int) ([]string, error)
	// ExecArgv is the full argv that runs argv inside a container with stdin
	// held open. The bridge hands it to a command transport.
	ExecArgv(container string, argv []string) []string
}

// matches reports whether a label set satisfies every entry of a filter.
func matches(labels map[string]string, filter Filter) bool {
	for k, v := range filter {
		if labels[k] != v {
			return false
		}
	}
	return true
}
