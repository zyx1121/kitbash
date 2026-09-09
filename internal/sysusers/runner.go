package sysusers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zyx1121/kitbash/internal/podman"
)

// logger writes where the operator reads, the same shape internal/problem
// uses. Nothing this package logs ever reaches a member: it carries host
// paths and the output of the host's own tools.
var logger = log.New(os.Stderr, "kitbashd: ", log.LstdFlags)

// ContainerTimeout bounds one podman call that only looks something up or
// starts an existing container, which is what spec/kitbashd-api.yaml gives
// restore per container.
const ContainerTimeout = RemoveTimeout

// RunTimeout is what one podman run gets. Creating a container is more work
// than starting one, and the runtime may still have to unpack an image layer,
// so it is the widest budget here.
const RunTimeout = 120 * time.Second

// StopBudget is what one podman stop or one forced removal gets. podman is
// given StopGrace seconds to let the container exit on its own, and a PID 1
// that ignores SIGTERM takes every one of them, so the child needs that grace
// plus room to kill the container and tear it down. A budget shorter than the
// grace kills the child in the middle of the stop, which is a container left
// half down and a caller told nothing useful.
const StopBudget = 40 * time.Second

// usageExit is the status podman exits with when it will not run the command
// at all: the options are wrong, or the image is not there. kitbashd checks
// the image itself, so what is left of this status is the options, which the
// member can change in their manifest.
const usageExit = 125

// RunnerPath is the PATH a member's podman is started with. kitbashd runs as
// root and inherits root's environment, which is not the member's, so every
// variable a rootless podman reads is set here and nothing is inherited.
const RunnerPath = "/usr/local/bin:/usr/bin:/bin"

// Podman is the real Runner: it starts and removes containers as the member
// who owns them, by dropping to their uid, gid and group list before exec.
// Running them as root would put the containers in root's store, where the
// member cannot see them and where nothing is rootless any more.
type Podman struct {
	// Binary is the runtime on PATH. Empty means podman.
	Binary string
	// RunUser is where a member's XDG runtime directory goes.
	RunUser string
}

// NewPodman returns the Runner kitbashd uses on a kitbash host.
func NewPodman() *Podman { return &Podman{Binary: podman.Binary, RunUser: DefaultRunUser} }

// Run starts one container as the member, inside their cgroup leaf, and
// returns the runtime id it printed. The options are the whole command line:
// kitbashd built them, wrote the env file the member can read, and named the
// cgroup parent the container's own cgroup goes under.
//
// The image is checked first, because an image the member does not have and a
// command line the runtime will not parse are the same exit status, and only
// the first of the two is worth telling the caller to build.
func (p *Podman) Run(ctx context.Context, m Member, opts podman.RunOptions, cgroup string) (string, error) {
	if err := ensureRuntimeDir(p.runUser(), m); err != nil {
		return "", err
	}
	if _, err := p.run(ctx, m, "image", "exists", opts.Image); err != nil {
		if exitCode(err, 1) {
			return "", fmt.Errorf("%w: %s", ErrNoImage, opts.Image)
		}
		return "", err
	}
	// Nothing of the environment is on this command line: the values are in
	// the file opts.EnvFile names, which the daemon wrote 0600 for this
	// member, see internal/daemon/run.go.
	out, err := p.runFor(ctx, m, cgroup, RunTimeout, podman.RunArgs(opts, nil)...)
	if err != nil {
		if exitCode(err, usageExit) {
			return "", fmt.Errorf("%w: %v", ErrUsage, err)
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Start creates the member's runtime directory and starts one container as
// them. A container the runtime does not have is ErrNoContainer, which restore
// answers by unregistering the Process rather than by trying again.
//
// The container keeps the cgroup parent it was created with, so a restored
// Process holds the limits it was started with as long as the child that
// starts it is placed in the member's leaf.
func (p *Podman) Start(ctx context.Context, m Member, container, cgroup string) error {
	if err := ensureRuntimeDir(p.runUser(), m); err != nil {
		return err
	}
	if err := p.exists(ctx, m, container); err != nil {
		return err
	}
	_, err := p.runIn(ctx, m, cgroup, "start", container)
	return err
}

// Stop stops one container as the member. The runtime is given the same
// timeout the surface publishes, and the call itself the daemon's.
func (p *Podman) Stop(ctx context.Context, m Member, container string, timeout int) error {
	if err := ensureRuntimeDir(p.runUser(), m); err != nil {
		return err
	}
	if err := p.exists(ctx, m, container); err != nil {
		return err
	}
	// The budget is the runtime's grace plus room to finish: the container is
	// given timeout seconds to exit on its own and only then killed.
	_, err := p.runFor(ctx, m, "", StopBudget, "stop", "--time", strconv.Itoa(timeout), container)
	return err
}

// RemoveContainer removes one container as the member, which is what a
// replacement run does to the Process it takes the place of.
func (p *Podman) RemoveContainer(ctx context.Context, m Member, container string, force bool) error {
	if err := ensureRuntimeDir(p.runUser(), m); err != nil {
		return err
	}
	if err := p.exists(ctx, m, container); err != nil {
		return err
	}
	args := []string{"rm"}
	if force {
		args = append(args, "--force")
	}
	// A forced removal stops the container first, on the same grace a stop
	// gets, so it gets the same budget.
	_, err := p.runFor(ctx, m, "", StopBudget, append(args, container)...)
	return err
}

// exists answers ErrNoContainer for a container this member's runtime does not
// have. podman container exists answers 0 for one it has and 1 for one it does
// not, which is the one distinction the callers act on.
func (p *Podman) exists(ctx context.Context, m Member, container string) error {
	if _, err := p.run(ctx, m, "container", "exists", container); err != nil {
		if exitCode(err, 1) {
			return fmt.Errorf("%w: %s", ErrNoContainer, container)
		}
		return err
	}
	return nil
}

// MCPCommand builds the kitbash-mcp one MCP session of a Process runs as its
// owner. It is the same drop to the member's uid, gid and group list as a
// container start, with the member's environment and nothing of the daemon's:
// kitbashd runs as root, and a child that inherited root's environment would
// read root's kitbash socket variables and root's PATH.
//
// The command is returned unstarted. The caller connects an MCP client to its
// stdin and stdout, and closing that connection is what ends the child, see
// mcp_for_processes in spec/kitbashd-api.yaml.
func (p *Podman) MCPCommand(ctx context.Context, m Member, binary, credential string) (*exec.Cmd, error) {
	if binary == "" {
		return nil, errors.New("sysusers: no kitbash-mcp binary to run")
	}
	// A relative path would be resolved against the working directory or the
	// PATH of whoever configured it, which is not a program kitbashd may run
	// as one of its members.
	if !filepath.IsAbs(binary) {
		return nil, fmt.Errorf("sysusers: %q is not an absolute path to kitbash-mcp", binary)
	}
	account, err := credentialOf(m)
	if err != nil {
		return nil, err
	}
	// The runtime directory is the member's, and a session that builds a
	// Package reaches the same rootless podman a container start does.
	if err := ensureRuntimeDir(p.runUser(), m); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, binary)
	// Setpgid puts the child in a process group of its own, so a signal sent
	// to the daemon's group, which is what a terminal or a service manager
	// sends, does not reach every member's session behind kitbashd's back.
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: account, Setpgid: true}
	cmd.Dir = "/"
	cmd.Env = MCPEnvironment(m, p.runUser(), credential)
	// The child's own log lines go through a pipe into the daemon log, named
	// and bounded. Handing it the daemon's stderr would give a member's
	// session a file descriptor of the daemon's and let it write as much as
	// it liked into the operator's log.
	cmd.Stderr = newChildLog(m.Name)
	return cmd, nil
}

// ChildLogLines is how many lines of one child's stderr reach the daemon log
// before the rest is dropped. A kitbash-mcp that is working writes none; one
// that is broken writes the same line forever, and the log is the operator's,
// not the child's.
const ChildLogLines = 100

// ChildLogLineBytes bounds one line, so a child cannot spend the cap in one
// write either.
const ChildLogLineBytes = 2 << 10

// childLog turns one child's stderr into log lines, named by the member the
// session runs as and bounded in both directions.
type childLog struct {
	member  string
	pending []byte
	written int
	capped  bool
}

func newChildLog(member string) *childLog { return &childLog{member: member} }

func (c *childLog) Write(p []byte) (int, error) {
	n := len(p)
	if c.capped {
		return n, nil
	}
	c.pending = append(c.pending, p...)
	for {
		cut := bytes.IndexByte(c.pending, '\n')
		if cut < 0 {
			break
		}
		c.line(c.pending[:cut])
		c.pending = c.pending[cut+1:]
	}
	// A child that never writes a newline is still bounded: the buffer is cut
	// at one line's worth and reported as it stands.
	if len(c.pending) > ChildLogLineBytes {
		c.line(c.pending[:ChildLogLineBytes])
		c.pending = c.pending[:0]
	}
	return n, nil
}

// line writes one line of a child's output, or the notice that the rest of it
// will not be written at all.
func (c *childLog) line(text []byte) {
	if c.capped {
		return
	}
	if c.written >= ChildLogLines {
		c.capped = true
		logger.Printf("kitbash-mcp[%s]: further output is not logged; the session wrote over %d lines",
			c.member, ChildLogLines)
		return
	}
	c.written++
	if len(text) > ChildLogLineBytes {
		text = text[:ChildLogLineBytes]
	}
	if len(bytes.TrimSpace(text)) == 0 {
		return
	}
	logger.Printf("kitbash-mcp[%s]: %s", c.member, text)
}

// RemoveAll force removes every container labelled with this member. It is the
// first step of removing a member: their Processes stop before their account
// and their home go, see spec/kitbashd-api.yaml.
func (p *Podman) RemoveAll(ctx context.Context, m Member) error {
	out, err := p.run(ctx, m, "ps", "--all", "--filter", podman.LabelUser+"="+m.Name, "--format", "{{.Names}}")
	if err != nil {
		return err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	_, err = p.run(ctx, m, append([]string{"rm", "--force"}, names...)...)
	return err
}

// run executes one podman command as the member and returns its standard
// output. The error carries the runtime's standard error for the server log.
func (p *Podman) run(ctx context.Context, m Member, args ...string) (string, error) {
	return p.runIn(ctx, m, "", args...)
}

// runIn is run with the child placed in one cgroup. An empty cgroup runs it
// where the daemon is: the host enforces no limits and the command is
// otherwise the same one, see internal/cgroups.
func (p *Podman) runIn(ctx context.Context, m Member, cgroup string, args ...string) (string, error) {
	return p.runFor(ctx, m, cgroup, ContainerTimeout, args...)
}

// runFor is runIn with the budget this command needs. A command that runs out
// of it is ErrTimeout rather than a runtime failure: the runtime was working,
// it did not finish, and only that distinction tells an operator whether to
// look at the Package or at the host.
//
// The caller's cancellation does not reach the child. A podman stop that has
// begun is tearing a container down, and a session that hung up, or a request
// whose response deadline passed, must not leave one half stopped: the budget
// is what bounds this, not the client.
func (p *Podman) runFor(ctx context.Context, m Member, cgroup string, budget time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.binary(), args...)
	credential, err := credentialOf(m)
	if err != nil {
		return "", err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	// The descriptor is held until the child has run: the kernel reads it at
	// clone time, and there is nothing to keep afterwards.
	closer, err := place(cmd.SysProcAttr, cgroup)
	defer closer()
	if err != nil {
		return "", err
	}
	cmd.Dir = "/"
	cmd.Env = p.environment(m)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return stdout.String(), fmt.Errorf("%w: %s %s as %s after %s: %s",
				ErrTimeout, p.binary(), strings.Join(args, " "), m.Name, budget, msg)
		}
		return stdout.String(), fmt.Errorf("sysusers: %s %s as %s: %w: %s",
			p.binary(), strings.Join(args, " "), m.Name, err, msg)
	}
	return stdout.String(), nil
}

// MCPEnvironment is what one kitbash-mcp session runs with: the member's own
// environment and the credential of the session, and nothing else.
// kitbashd runs as root, so a child that inherited its environment would read
// root's socket overrides and root's PATH. It is exported because the daemon
// tests the child against the environment the host would give it.
func MCPEnvironment(m Member, runUser, credential string) []string {
	p := &Podman{RunUser: runUser}
	return append(p.environment(m), EnvCaller+"="+credential)
}

// environment is what a rootless podman needs and nothing more. Inheriting
// root's environment would hand a member's runtime root's XDG_RUNTIME_DIR and
// root's container store.
func (p *Podman) environment(m Member) []string {
	return []string{
		"HOME=" + m.Home,
		"USER=" + m.Name,
		"LOGNAME=" + m.Name,
		"PATH=" + RunnerPath,
		"XDG_RUNTIME_DIR=" + filepath.Join(p.runUser(), strconv.Itoa(m.UID)),
	}
}

func (p *Podman) binary() string {
	if p.Binary == "" {
		return podman.Binary
	}
	return p.Binary
}

func (p *Podman) runUser() string {
	if p.RunUser == "" {
		return DefaultRunUser
	}
	return p.RunUser
}

// credentialOf is the uid, gid and supplementary groups one member's process
// runs with. It is the account a child runs as, not the caller credential of
// an MCP session, which is minted by kitbashd. The supplementary list matters: kitbash-users is what the socket
// is grouped to, and a container started without it cannot reach kitbashd.
func credentialOf(m Member) (*syscall.Credential, error) {
	if m.UID <= 0 {
		return nil, fmt.Errorf("sysusers: %s has no uid to run as", m.Name)
	}
	u, err := user.Lookup(m.Name)
	if err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, m.Name)
		}
		return nil, fmt.Errorf("sysusers: look up %s: %w", m.Name, err)
	}
	ids, err := u.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("sysusers: read the groups of %s: %w", m.Name, err)
	}
	groups := make([]uint32, 0, len(ids))
	for _, id := range ids {
		n, err := strconv.Atoi(id)
		if err != nil {
			continue
		}
		groups = append(groups, uint32(n))
	}
	return &syscall.Credential{Uid: uint32(m.UID), Gid: uint32(m.GID), Groups: groups}, nil
}
