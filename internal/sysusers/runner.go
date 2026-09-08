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

	"github.com/zyx1121/kitbash/internal/podman"
)

// logger writes where the operator reads, the same shape internal/problem
// uses. Nothing this package logs ever reaches a member: it carries host
// paths and the output of the host's own tools.
var logger = log.New(os.Stderr, "kitbashd: ", log.LstdFlags)

// ContainerTimeout bounds one podman call made as a member, which is what
// spec/kitbashd-api.yaml gives restore per container.
const ContainerTimeout = RemoveTimeout

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

// Start creates the member's runtime directory and starts one container as
// them. A container the runtime does not have is ErrNoContainer, which restore
// answers by unregistering the Process rather than by trying again.
func (p *Podman) Start(ctx context.Context, m Member, container string) error {
	if err := ensureRuntimeDir(p.runUser(), m); err != nil {
		return err
	}
	// podman container exists answers 0 for a container it has and 1 for one
	// it does not, which is the one distinction restore acts on.
	if _, err := p.run(ctx, m, "container", "exists", container); err != nil {
		if exitCode(err, 1) {
			return fmt.Errorf("%w: %s", ErrNoContainer, container)
		}
		return err
	}
	_, err := p.run(ctx, m, "start", container)
	return err
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
	ctx, cancel := context.WithTimeout(ctx, ContainerTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.binary(), args...)
	credential, err := credentialOf(m)
	if err != nil {
		return "", err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
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
		return stdout.String(), fmt.Errorf("sysusers: %s %s as %s: %w: %s",
			p.binary(), strings.Join(args, " "), m.Name, err, msg)
	}
	return stdout.String(), nil
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
// runs with. The supplementary list matters: kitbash-users is what the socket
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
