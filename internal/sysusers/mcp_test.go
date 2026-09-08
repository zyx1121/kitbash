package sysusers

import (
	"bytes"
	"context"
	"log"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// credentialLine is one caller credential, the shape kitbashd mints: 32 random
// bytes as base64url. It is spelled out rather than generated so the
// environment assertion below is exact.
const credentialLine = "8Sx1p3Nn7Yb2Qh4Vz0Kc9Lm5Rt6Wd8Fg1Jk3Bp7Ns0"

// TestMCPCommandRunsAsTheMemberWithNothingInherited is the identity half of an
// MCP session: the child runs as the owner, with the owner's environment and
// the Process that opened the session, and with nothing of the daemon's.
func TestMCPCommandRunsAsTheMemberWithNothingInherited(t *testing.T) {
	t.Setenv("KITBASH_SOCKET", "/run/kitbash/root-only.sock")
	me, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	uid, err := strconv.Atoi(me.Uid)
	if err != nil {
		t.Fatalf("uid %q: %v", me.Uid, err)
	}
	if uid == 0 {
		t.Skip("the test asserts a drop to a member, which root is not")
	}
	gid, err := strconv.Atoi(me.Gid)
	if err != nil {
		t.Fatalf("gid %q: %v", me.Gid, err)
	}
	member := Member{Name: me.Username, UID: uid, GID: gid, Home: me.HomeDir}

	runner := &Podman{RunUser: t.TempDir()}
	cmd, err := runner.MCPCommand(context.Background(), member, "/usr/bin/kitbash-mcp", credentialLine)
	if err != nil {
		t.Fatalf("MCPCommand: %v", err)
	}
	if cmd.Path != "/usr/bin/kitbash-mcp" {
		t.Errorf("the command is %q, want the kitbash-mcp it was given", cmd.Path)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatal("the command runs with no credential, so it would run as root")
	}
	credential := cmd.SysProcAttr.Credential
	if credential.Uid != uint32(uid) || credential.Gid != uint32(gid) {
		t.Errorf("the command runs as %d:%d, want the member %d:%d",
			credential.Uid, credential.Gid, uid, gid)
	}
	if len(credential.Groups) == 0 {
		t.Error("the command runs with no supplementary groups, so it could not reach the kitbashd socket")
	}
	// The child is in a process group of its own, so a signal to the daemon's
	// group does not reach every member's session behind kitbashd's back.
	if !cmd.SysProcAttr.Setpgid {
		t.Error("the child shares the daemon's process group, want one of its own")
	}
	// The child's output goes into the daemon log named and bounded, not down
	// a file descriptor of the daemon's.
	if _, piped := cmd.Stderr.(*childLog); !piped {
		t.Errorf("stderr is %T, want the bounded child log", cmd.Stderr)
	}

	want := []string{
		"HOME=" + me.HomeDir,
		"USER=" + me.Username,
		"LOGNAME=" + me.Username,
		"PATH=" + RunnerPath,
		"XDG_RUNTIME_DIR=" + filepath.Join(runner.RunUser, strconv.Itoa(uid)),
		EnvCaller + "=" + credentialLine,
	}
	if !slices.Equal(cmd.Env, want) {
		t.Errorf("the environment is\n%v\nwant\n%v", cmd.Env, want)
	}
}

// TestMCPCommandRefusesABinaryItCannotResolve is the refusal a daemon
// configured with no kitbash-mcp, or with a relative one, gets: a path
// resolved against a working directory or a PATH is not a program kitbashd may
// run as one of its members.
func TestMCPCommandRefusesABinaryItCannotResolve(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	uid, _ := strconv.Atoi(me.Uid)
	if uid == 0 {
		t.Skip("the test asserts a drop to a member, which root is not")
	}
	runner := &Podman{RunUser: t.TempDir()}
	member := Member{Name: me.Username, UID: uid, Home: me.HomeDir}
	for _, binary := range []string{"", "kitbash-mcp", "./kitbash-mcp", "bin/kitbash-mcp"} {
		if _, err := runner.MCPCommand(context.Background(), member, binary, credentialLine); err == nil {
			t.Errorf("the binary %q was accepted, want a refusal", binary)
		}
	}
}

// TestChildLogIsBounded keeps one session's output from filling the operator's
// log: the lines are named and there is a cap on how many of them arrive.
func TestChildLogIsBounded(t *testing.T) {
	var written bytes.Buffer
	restore := logger
	logger = log.New(&written, "", 0)
	t.Cleanup(func() { logger = restore })

	child := newChildLog("alice")
	for range ChildLogLines * 2 {
		if _, err := child.Write([]byte("the session said something\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	// A write far over one line is cut rather than logged whole.
	if _, err := child.Write(bytes.Repeat([]byte("a"), 4*ChildLogLineBytes)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(written.String()), "\n")
	if len(lines) != ChildLogLines+1 {
		t.Fatalf("the log holds %d lines, want %d and the notice", len(lines), ChildLogLines)
	}
	if !strings.HasPrefix(lines[0], "kitbash-mcp[alice]: ") {
		t.Errorf("the first line is %q, want the session named", lines[0])
	}
	if !strings.Contains(lines[len(lines)-1], "further output is not logged") {
		t.Errorf("the last line is %q, want the notice that the rest is dropped", lines[len(lines)-1])
	}
	for _, line := range lines {
		if len(line) > ChildLogLineBytes+len("kitbash-mcp[alice]: ") {
			t.Errorf("a line is %d bytes, want it cut at the limit", len(line))
		}
	}
}
