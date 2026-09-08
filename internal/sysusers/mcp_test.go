package sysusers

import (
	"context"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

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
	cmd, err := runner.MCPCommand(context.Background(), member, "/usr/bin/kitbash-mcp", "01a07f95-df01-7073-add4-29e769a336f4")
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

	want := []string{
		"HOME=" + me.HomeDir,
		"USER=" + me.Username,
		"LOGNAME=" + me.Username,
		"PATH=" + RunnerPath,
		"XDG_RUNTIME_DIR=" + filepath.Join(runner.RunUser, strconv.Itoa(uid)),
		EnvCaller + "=01a07f95-df01-7073-add4-29e769a336f4",
	}
	if !slices.Equal(cmd.Env, want) {
		t.Errorf("the environment is\n%v\nwant\n%v", cmd.Env, want)
	}
}

// TestMCPCommandWithoutABinary is the refusal a daemon configured with no
// kitbash-mcp gets, rather than a command that runs whatever is on PATH.
func TestMCPCommandWithoutABinary(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	uid, _ := strconv.Atoi(me.Uid)
	if uid == 0 {
		t.Skip("the test asserts a drop to a member, which root is not")
	}
	runner := &Podman{RunUser: t.TempDir()}
	if _, err := runner.MCPCommand(context.Background(),
		Member{Name: me.Username, UID: uid, Home: me.HomeDir}, "", "01a07f95-df01-7073-add4-29e769a336f4"); err == nil {
		t.Error("a session without a binary was accepted, want a refusal")
	}
}
