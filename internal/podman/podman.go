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
)

// Container states the runtime reports.
const (
	StateRunning     = "running"
	StateCreated     = "created"
	StateInitialized = "initialized"
	StateExited      = "exited"
)

// Filter selects images or containers by label. An empty filter matches
// everything.
type Filter map[string]string

// Image is one image in the caller's store.
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
type RunOptions struct {
	Name        string
	Image       string
	Labels      map[string]string
	Env         map[string]string
	Restart     string
	CPUs        string
	Memory      string
	Publish     []int
	Detach      bool
	Interactive bool
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
