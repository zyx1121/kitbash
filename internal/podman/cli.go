package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CLI drives the podman binary on PATH as the connecting user, which is what
// makes every image and every container rootless and owned by that member.
type CLI struct{}

// NewCLI returns the runner the server uses on a kitbash host.
func NewCLI() *CLI { return &CLI{} }

// run executes one podman command and returns its standard output. The error
// carries the command line and the runtime's stderr, for the server log.
func (c *CLI) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, Binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return stdout.String(), fmt.Errorf("podman %s: %v: %s", strings.Join(args, " "), err, msg)
	}
	return stdout.String(), nil
}

// Build builds an image and reads its ID back from an --iidfile, which is the
// only way podman reports the ID of a build that used a tag.
func (c *CLI) Build(ctx context.Context, contextDir, containerfile, tag string, labels map[string]string) (string, string, error) {
	dir, err := os.MkdirTemp("", "kitbash-build-")
	if err != nil {
		return "", "", fmt.Errorf("creating the build id file: %w", err)
	}
	defer os.RemoveAll(dir)
	idFile := filepath.Join(dir, "iid")

	args := []string{"build", "--iidfile", idFile, "--file", containerfile, "--tag", tag}
	for _, k := range sortedKeys(labels) {
		args = append(args, "--label", k+"="+labels[k])
	}
	args = append(args, contextDir)

	cmd := exec.CommandContext(ctx, Binary, args...)
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	runErr := cmd.Run()
	log := combined.String()
	if runErr != nil {
		// An exit status means podman ran and the build did not succeed.
		// Anything else means podman itself never got that far.
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			return "", log, fmt.Errorf("podman build %s: %v: %w", contextDir, exit, ErrBuildFailed)
		}
		return "", log, fmt.Errorf("podman build %s: %v: %s", contextDir, runErr, strings.TrimSpace(log))
	}
	id, err := os.ReadFile(idFile)
	if err != nil {
		return "", log, fmt.Errorf("reading the build id file: %w", err)
	}
	return normalizeID(strings.TrimSpace(string(id))), log, nil
}

// imageJSON is the part of podman images --format json kitbash reads.
type imageJSON struct {
	ID      string            `json:"Id"`
	Created int64             `json:"Created"`
	Labels  map[string]string `json:"Labels"`
}

// Images lists the caller's images. Older podman releases omit labels from the
// list, so an image without any is inspected once before it is filtered out.
func (c *CLI) Images(ctx context.Context, filter Filter) ([]Image, error) {
	args := []string{"images", "--format", "json", "--no-trunc"}
	for _, k := range sortedKeys(filter) {
		args = append(args, "--filter", "label="+k+"="+filter[k])
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var decoded []imageJSON
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		return nil, fmt.Errorf("decoding podman images: %w", err)
	}
	var images []Image
	for _, d := range decoded {
		labels := d.Labels
		if len(labels) == 0 {
			labels, _ = c.imageLabels(ctx, d.ID)
		}
		if !matches(labels, filter) {
			continue
		}
		images = append(images, Image{
			ID:      normalizeID(d.ID),
			Created: time.Unix(d.Created, 0).UTC(),
			Labels:  labels,
		})
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Created.After(images[j].Created) })
	return images, nil
}

// inspectJSON is the part of podman image inspect kitbash reads.
type inspectJSON struct {
	Config struct {
		Entrypoint stringList        `json:"Entrypoint"`
		Cmd        stringList        `json:"Cmd"`
		Labels     map[string]string `json:"Labels"`
	} `json:"Config"`
}

func (c *CLI) inspect(ctx context.Context, ref string) (*inspectJSON, error) {
	out, err := c.run(ctx, "image", "inspect", "--format", "json", ref)
	if err != nil {
		return nil, err
	}
	var decoded []inspectJSON
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		return nil, fmt.Errorf("decoding podman image inspect: %w", err)
	}
	if len(decoded) == 0 {
		return nil, fmt.Errorf("podman image inspect %s returned nothing", ref)
	}
	return &decoded[0], nil
}

func (c *CLI) imageLabels(ctx context.Context, ref string) (map[string]string, error) {
	got, err := c.inspect(ctx, ref)
	if err != nil {
		return nil, err
	}
	return got.Config.Labels, nil
}

// ImageEntrypoint reads what the image runs as PID 1.
func (c *CLI) ImageEntrypoint(ctx context.Context, ref string) ([]string, []string, error) {
	got, err := c.inspect(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	return got.Config.Entrypoint, got.Config.Cmd, nil
}

// Run starts one container.
func (c *CLI) Run(ctx context.Context, opts RunOptions) (string, error) {
	args := []string{"run", "--name", opts.Name}
	if opts.Detach {
		args = append(args, "--detach")
	}
	if opts.Interactive {
		args = append(args, "--interactive")
	}
	for _, k := range sortedKeys(opts.Labels) {
		args = append(args, "--label", k+"="+opts.Labels[k])
	}
	for _, k := range sortedKeys(opts.Env) {
		args = append(args, "--env", k+"="+opts.Env[k])
	}
	if opts.Restart != "" {
		args = append(args, "--restart", opts.Restart)
	}
	if opts.CPUs != "" {
		args = append(args, "--cpus", opts.CPUs)
	}
	if opts.Memory != "" {
		args = append(args, "--memory", opts.Memory)
	}
	for _, port := range opts.Publish {
		// The loopback address only: a Process is reachable from this host,
		// never from the network, until a reverse proxy fronts it. An empty
		// host port leaves the choice to the runtime.
		host := ""
		if port.HostPort > 0 {
			host = strconv.Itoa(port.HostPort)
		}
		args = append(args, "--publish", "127.0.0.1:"+host+":"+strconv.Itoa(port.ContainerPort))
	}
	args = append(args, opts.Image)
	out, err := c.run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// containerJSON is the part of podman ps --format json kitbash reads.
type containerJSON struct {
	ID        string            `json:"Id"`
	Names     []string          `json:"Names"`
	Image     string            `json:"Image"`
	State     string            `json:"State"`
	ExitCode  int               `json:"ExitCode"`
	StartedAt int64             `json:"StartedAt"`
	Labels    map[string]string `json:"Labels"`
	Ports     []struct {
		HostIP        string `json:"host_ip"`
		HostPort      int    `json:"host_port"`
		ContainerPort int    `json:"container_port"`
		Protocol      string `json:"protocol"`
	} `json:"Ports"`
}

// Containers lists the caller's containers.
func (c *CLI) Containers(ctx context.Context, filter Filter, all bool) ([]Container, error) {
	args := []string{"ps", "--format", "json", "--no-trunc"}
	if all {
		args = append(args, "--all")
	}
	for _, k := range sortedKeys(filter) {
		args = append(args, "--filter", "label="+k+"="+filter[k])
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var decoded []containerJSON
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		return nil, fmt.Errorf("decoding podman ps: %w", err)
	}
	var containers []Container
	for _, d := range decoded {
		if !matches(d.Labels, filter) {
			continue
		}
		container := Container{
			ID:        d.ID,
			State:     strings.ToLower(d.State),
			ExitCode:  d.ExitCode,
			StartedAt: time.Unix(d.StartedAt, 0).UTC(),
			Labels:    d.Labels,
			Image:     normalizeID(d.Image),
		}
		if len(d.Names) > 0 {
			container.Name = d.Names[0]
		}
		for _, p := range d.Ports {
			container.Ports = append(container.Ports, Port{
				HostIP:        p.HostIP,
				HostPort:      p.HostPort,
				ContainerPort: p.ContainerPort,
				Protocol:      p.Protocol,
			})
		}
		containers = append(containers, container)
	}
	return containers, nil
}

// Stop stops a container, giving it timeout seconds to exit on its own.
func (c *CLI) Stop(ctx context.Context, name string, timeout int) error {
	_, err := c.run(ctx, "stop", "--time", strconv.Itoa(timeout), name)
	return err
}

// Remove removes a container.
func (c *CLI) Remove(ctx context.Context, name string, force bool) error {
	args := []string{"rm"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, name)
	_, err := c.run(ctx, args...)
	return err
}

// Logs returns the tail of a container's output.
func (c *CLI) Logs(ctx context.Context, name string, tail int) ([]string, error) {
	out, err := c.run(ctx, "logs", "--tail", strconv.Itoa(tail), name)
	if err != nil {
		return nil, err
	}
	return SplitLines(out), nil
}

// ExecArgv is the argv that opens one more MCP session inside a container.
//
// There is no double dash before the image's argv. podman stops parsing flags
// at the container name, so everything after it is already positional, and a
// dash dash there is passed to the runtime as the command to run: crun then
// reports "executable file `--` not found in $PATH".
func (c *CLI) ExecArgv(container string, argv []string) []string {
	full := []string{Binary, "exec", "--interactive", container}
	return append(full, argv...)
}

// SplitLines turns runtime output into the line array the surface returns,
// dropping the trailing empty line every stream ends with.
func SplitLines(out string) []string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []string{}
	}
	return lines
}

// normalizeID gives every image reference the sha256: prefix the surface
// publishes, whichever form podman used.
func normalizeID(id string) string {
	if id == "" || strings.Contains(id, ":") {
		return id
	}
	return "sha256:" + id
}

// stringList decodes a field podman writes either as a string or as an array.
type stringList []string

func (s *stringList) UnmarshalJSON(data []byte) error {
	var list []string
	if err := json.Unmarshal(data, &list); err == nil {
		*s = list
		return nil
	}
	var one string
	if err := json.Unmarshal(data, &one); err != nil {
		return err
	}
	if one == "" {
		*s = nil
		return nil
	}
	*s = []string{one}
	return nil
}

// sortedKeys keeps every generated command line deterministic, which is what
// makes a build reproducible and a test readable.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
