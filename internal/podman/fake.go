package podman

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Fake is an in memory Runner. It keeps the state a real runtime would keep,
// so a test asserts on images and containers rather than on command lines, and
// it records every build and run for the assertions that are about the command
// line after all, such as build labels.
//
// It lives beside the CLI rather than in a test file because internal/pkg,
// internal/proc and internal/bridge all test against it, and because no
// machine that runs the unit tests is required to have podman installed.
type Fake struct {
	mu sync.Mutex

	// Log is the build log every Build returns.
	Log string
	// BuildErr, RunErr and StopErr make the runtime fail on demand.
	BuildErr error
	RunErr   error
	StopErr  error
	// Entrypoints answers ImageEntrypoint per image reference.
	Entrypoints map[string][]string
	// LogLines answers Logs per container name.
	LogLines map[string][]string

	// Builds and Runs record what the caller asked for, newest last.
	Builds []BuildCall
	Runs   []RunOptions
	// Stopped and Removed record container names.
	Stopped []string
	Removed []string

	images     []Image
	containers []Container
	seq        int
	now        time.Time
}

// BuildCall is one recorded build.
type BuildCall struct {
	ContextDir    string
	Containerfile string
	Tag           string
	Labels        map[string]string
}

// NewFake returns a runtime with an empty image store.
func NewFake() *Fake {
	return &Fake{
		Log:         "STEP 1: FROM alpine\nCOMMIT\n",
		Entrypoints: map[string][]string{},
		LogLines:    map[string][]string{},
		now:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// tick returns a time that advances with every call, so newest first ordering
// is deterministic.
func (f *Fake) tick() time.Time {
	f.seq++
	return f.now.Add(time.Duration(f.seq) * time.Minute)
}

// AddImage seeds the image store.
func (f *Fake) AddImage(image Image) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if image.Created.IsZero() {
		image.Created = f.tick()
	}
	f.images = append(f.images, image)
}

// AddContainer seeds the container list.
func (f *Fake) AddContainer(container Container) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if container.ID == "" {
		container.ID = f.id("container")
	}
	if container.StartedAt.IsZero() {
		container.StartedAt = f.tick()
	}
	f.containers = append(f.containers, container)
}

// id returns a stable pseudo random hex identifier.
func (f *Fake) id(kind string) string {
	f.seq++
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", kind, f.seq)))
	return hex.EncodeToString(sum[:])
}

// Build records the build and adds the image it would have produced.
func (f *Fake) Build(_ context.Context, contextDir, containerfile, tag string, labels map[string]string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Builds = append(f.Builds, BuildCall{
		ContextDir:    contextDir,
		Containerfile: containerfile,
		Tag:           tag,
		Labels:        labels,
	})
	if f.BuildErr != nil {
		return "", f.Log, f.BuildErr
	}
	id := "sha256:" + f.id("image")
	f.images = append(f.images, Image{ID: id, Created: f.tick(), Labels: copyLabels(labels)})
	return id, f.Log, nil
}

// Images answers from the seeded and built images, newest first.
func (f *Fake) Images(_ context.Context, filter Filter) ([]Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Image
	for _, image := range f.images {
		if matches(image.Labels, filter) {
			out = append(out, image)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// ImageEntrypoint answers from Entrypoints, defaulting to a stdio server.
func (f *Fake) ImageEntrypoint(_ context.Context, ref string) ([]string, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if argv, ok := f.Entrypoints[ref]; ok {
		return argv, nil, nil
	}
	return []string{"/usr/local/bin/server"}, nil, nil
}

// Run adds a running container, publishing every requested port on a host port
// derived from the sequence so the endpoint a test reads is predictable.
func (f *Fake) Run(_ context.Context, opts RunOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Runs = append(f.Runs, opts)
	if f.RunErr != nil {
		return "", f.RunErr
	}
	container := Container{
		ID:        f.id("container"),
		Name:      opts.Name,
		State:     StateRunning,
		StartedAt: f.tick(),
		Labels:    copyLabels(opts.Labels),
		Image:     opts.Image,
	}
	for i, port := range opts.Publish {
		container.Ports = append(container.Ports, Port{
			HostIP:        "127.0.0.1",
			HostPort:      34000 + i,
			ContainerPort: port,
			Protocol:      "tcp",
		})
	}
	f.containers = append(f.containers, container)
	return container.ID, nil
}

// Containers answers from the container list.
func (f *Fake) Containers(_ context.Context, filter Filter, all bool) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Container
	for _, container := range f.containers {
		if !all && container.State != StateRunning {
			continue
		}
		if matches(container.Labels, filter) {
			out = append(out, container)
		}
	}
	return out, nil
}

// Stop moves a container to exited with code 0.
func (f *Fake) Stop(_ context.Context, name string, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StopErr != nil {
		return f.StopErr
	}
	f.Stopped = append(f.Stopped, name)
	for i := range f.containers {
		if f.containers[i].Name == name {
			f.containers[i].State = StateExited
			f.containers[i].ExitCode = 0
			return nil
		}
	}
	return fmt.Errorf("no such container %s", name)
}

// Remove deletes a container.
func (f *Fake) Remove(_ context.Context, name string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Removed = append(f.Removed, name)
	kept := f.containers[:0]
	for _, container := range f.containers {
		if container.Name != name {
			kept = append(kept, container)
		}
	}
	f.containers = kept
	return nil
}

// Logs answers from LogLines.
func (f *Fake) Logs(_ context.Context, name string, tail int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lines := f.LogLines[name]
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	return append([]string{}, lines...), nil
}

// ExecArgv matches the CLI so a test reads the argv the bridge would run.
func (f *Fake) ExecArgv(container string, argv []string) []string {
	return (&CLI{}).ExecArgv(container, argv)
}

// HostPort is the port Run published for a container, for assertions.
func (f *Fake) HostPort(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, container := range f.containers {
		if container.Name == name && len(container.Ports) > 0 {
			return strconv.Itoa(container.Ports[0].HostPort)
		}
	}
	return ""
}

func copyLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}
