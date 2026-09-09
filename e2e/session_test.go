package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The job's contract with these tests. Everything else is a default, so a
// developer reading the workflow reads the whole environment.
const (
	gateEnv    = "KITBASH_E2E"       // set to 1 by the job; unset skips every test
	binaryEnv  = "KITBASH_E2E_MCP"   // the kitbash-mcp a session runs
	adminEnv   = "KITBASH_E2E_ADMIN" // the member in kitbash-admin
	memberEnv  = "KITBASH_E2E_MEMBER"
	secondEnv  = "KITBASH_E2E_SECOND" // the member users_create adds
	keyEnv     = "KITBASH_E2E_SSH_KEY"
	logDirEnv  = "KITBASH_E2E_LOGS" // where session stderr is kept for the artifact
	socketPath = "/run/kitbash/kitbashd.sock"
)

// How long one call may take. A build pulls a base image the first time, so it
// gets the whole budget; everything else answers in seconds.
const (
	callTimeout  = 90 * time.Second
	buildTimeout = 5 * time.Minute
	closeTimeout = 30 * time.Second
)

// path is the environment the surface runs with, the same PATH a login shell
// would give a member. A session starts with env -i, so this is all of it.
const envPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// requireHost skips a test that is not running inside the job.
func requireHost(t *testing.T) {
	t.Helper()
	if os.Getenv(gateEnv) == "" {
		t.Skipf("%s is not set: this test needs the host the end to end job builds", gateEnv)
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func mcpBinary() string  { return envOr(binaryEnv, "/usr/bin/kitbash-mcp") }
func adminName() string  { return envOr(adminEnv, "admin1") }
func memberName() string { return envOr(memberEnv, "member1") }
func secondName() string { return envOr(secondEnv, "member2") }

// sshKey is the public key users_create is given. The job generates one; the
// member it belongs to never connects, because these tests run kitbash-mcp
// through sudo rather than through sshd.
func sshKey(t *testing.T) string {
	t.Helper()
	if inline := os.Getenv(keyEnv); inline != "" {
		if line, err := os.ReadFile(inline); err == nil {
			return strings.TrimSpace(string(line))
		}
		return strings.TrimSpace(inline)
	}
	t.Fatalf("%s names neither a key nor a file holding one", keyEnv)
	return ""
}

func logDir(t *testing.T) string {
	t.Helper()
	dir := envOr(logDirEnv, t.TempDir())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("log directory %s: %v", dir, err)
	}
	return dir
}

// asUser is a command run as one member, with the environment sshd would give
// them and nothing of the job's. Rootless podman needs HOME and
// XDG_RUNTIME_DIR; kitbash-mcp reads neither, it asks the kernel who it is.
//
// This is how a session starts, so it is started the way sshd starts one:
// outside the member's cgroup. Placing it there is kitbash-mcp's own job
// through POST /kitbash/v1/sessions/join, and a job that placed it by hand
// would be testing itself.
func asUser(t *testing.T, name string, argv ...string) *exec.Cmd {
	t.Helper()
	u, err := user.Lookup(name)
	if err != nil {
		t.Fatalf("looking up %s: %v", name, err)
	}
	env := []string{
		"HOME=" + u.HomeDir,
		"USER=" + name,
		"LOGNAME=" + name,
		"PATH=" + envPath,
		"XDG_RUNTIME_DIR=/run/user/" + u.Uid,
		"TMPDIR=/tmp",
	}
	// setpriv rather than sudo -u, which is what kitbashd does for the
	// sessions of a Process: it changes user without opening a PAM session,
	// and a PAM session here would hand the member to logind, which removes
	// their runtime directory when it ends and gives rootless podman a systemd
	// to place its pause process in, outside the cgroups kitbashd delegates.
	args := []string{"-n", "setpriv", "--reuid", u.Uid, "--regid", u.Gid, "--init-groups", "env", "-i"}
	args = append(args, env...)
	cmd := exec.Command("sudo", append(args, argv...)...)
	// The session inherits its working directory, and podman's children chdir
	// to it: the job's own directory is under a home the member cannot reach,
	// so the session starts where every member can stand. The home itself is
	// not it: this process is still the job's user when it changes directory,
	// and a member's home is theirs alone.
	cmd.Dir = "/"
	return cmd
}

// runAs runs one command to completion as a member and returns its output. It
// is how the job reaches podman, which is a member's own runtime, and it goes
// through as-member.sh because podman exec into a Process only works from
// inside that member's cgroup.
func runAs(t *testing.T, name string, argv ...string) (string, error) {
	t.Helper()
	script, err := filepath.Abs("as-member.sh")
	if err != nil {
		t.Fatalf("resolving as-member.sh: %v", err)
	}
	args := append([]string{"-n", "/bin/sh", script, name}, argv...)
	out, err := exec.Command("sudo", args...).CombinedOutput()
	return string(out), err
}

// ---------------------------------------------------------------- the client

// session is one MCP session over the stdio of kitbash-mcp, run as a member.
// It is deliberately not the SDK: the protocol is newline delimited JSON-RPC,
// and writing it out is what makes this a second implementation.
type session struct {
	t       *testing.T
	user    string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  *os.File
	replies chan reply
	fatal   chan error
	writeMu sync.Mutex
	nextID  int
	closed  bool
}

type reply struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// incoming is one message from the server, which is a reply, a notification or
// a request the server makes of its client.
type incoming struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method"`
	Result json.RawMessage  `json:"result"`
	Error  *rpcError        `json:"error"`
}

// dial starts kitbash-mcp as one member and completes the handshake.
func dial(t *testing.T, name string) *session {
	t.Helper()
	stderr, err := os.Create(filepath.Join(logDir(t),
		fmt.Sprintf("kitbash-mcp-%s-%d.stderr", name, time.Now().UnixNano())))
	if err != nil {
		t.Fatalf("session log for %s: %v", name, err)
	}
	cmd := asUser(t, name, mcpBinary())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin for %s: %v", name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout for %s: %v", name, err)
	}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s as %s: %v", mcpBinary(), name, err)
	}
	s := &session{
		t: t, user: name, cmd: cmd, stdin: stdin, stderr: stderr,
		replies: make(chan reply, 16),
		fatal:   make(chan error, 1),
	}
	go s.read(stdout)

	var handshake struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	s.request("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "kitbash-e2e", "version": "1"},
	}, &handshake)
	if handshake.ProtocolVersion == "" {
		t.Fatalf("%s: initialize answered no protocol version", name)
	}
	s.notify("notifications/initialized", map[string]any{})
	t.Cleanup(s.close)
	return s
}

// read turns the server's stream into replies. A notification is dropped; a
// request the server makes of this client is answered, because a client that
// leaves one open would hold the session's own reply behind it.
func (s *session) read(stdout io.ReadCloser) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var message incoming
		if err := json.Unmarshal(line, &message); err != nil {
			s.fail(fmt.Errorf("decoding %q: %w", truncate(string(line)), err))
			return
		}
		switch {
		case message.Method != "" && message.ID != nil:
			s.answer(*message.ID, message.Method)
		case message.Method != "":
			// A notification, tools/list_changed among them.
		default:
			var id int
			if message.ID != nil {
				if err := json.Unmarshal(*message.ID, &id); err != nil {
					s.fail(fmt.Errorf("reply with id %s: %w", *message.ID, err))
					return
				}
			}
			s.replies <- reply{ID: id, Result: message.Result, Error: message.Error}
		}
	}
	if err := scanner.Err(); err != nil {
		s.fail(fmt.Errorf("reading the session: %w", err))
		return
	}
	s.fail(io.EOF)
}

// answer replies to a request the server made. ping is the one this client
// serves; anything else is refused in the protocol's own words.
func (s *session) answer(id json.RawMessage, method string) {
	if method == "ping" {
		s.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": map[string]any{}})
		return
	}
	s.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id),
		"error": map[string]any{"code": -32601, "message": "this client serves no " + method}})
}

func (s *session) fail(err error) {
	select {
	case s.fatal <- err:
	default:
	}
}

// write sends one message. It is called from the reader goroutine as well as
// from the test's, so a failure is recorded rather than reported: only the
// goroutine running the test may end it.
func (s *session) write(message any) {
	encoded, err := json.Marshal(message)
	if err != nil {
		s.fail(fmt.Errorf("encoding a message: %w", err))
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.stdin.Write(append(encoded, '\n')); err != nil {
		s.fail(fmt.Errorf("writing to the session: %w", err))
	}
}

func (s *session) notify(method string, params any) {
	s.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// request sends one call and decodes its result. A JSON-RPC error is a failure
// of the session rather than an answer: the surface reports what a member did
// wrong as problem details inside a result, never as a protocol error.
func (s *session) request(method string, params any, out any) {
	s.t.Helper()
	s.requestWithin(callTimeout, method, params, out)
}

func (s *session) requestWithin(limit time.Duration, method string, params any, out any) {
	s.t.Helper()
	s.nextID++
	id := s.nextID
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	deadline := time.After(limit)
	for {
		select {
		case got := <-s.replies:
			if got.ID != id {
				continue
			}
			if got.Error != nil {
				s.t.Fatalf("%s: %s answered a protocol error %d: %s",
					s.user, method, got.Error.Code, got.Error.Message)
			}
			if out != nil {
				if err := json.Unmarshal(got.Result, out); err != nil {
					s.t.Fatalf("%s: decoding the result of %s: %v", s.user, method, err)
				}
			}
			return
		case err := <-s.fatal:
			s.t.Fatalf("%s: the session ended during %s: %v (see %s)",
				s.user, method, err, s.stderr.Name())
		case <-deadline:
			s.t.Fatalf("%s: %s did not answer within %s (see %s)",
				s.user, method, limit, s.stderr.Name())
		}
	}
}

// tools lists the surface this session is served.
func (s *session) tools() []string {
	s.t.Helper()
	var out struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	s.request("tools/list", map[string]any{}, &out)
	names := make([]string, 0, len(out.Tools))
	for _, tool := range out.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// result is what one tools/call answered: an output or a problem.
type result struct {
	IsError bool `json:"isError"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Structured json.RawMessage `json:"structuredContent"`
}

// problemDetails is RFC 9457 as the surface writes it, read here rather than
// imported so these tests check the wire and not kitbash's own struct.
type problemDetails struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance"`
	Fix      string `json:"fix"`
}

func (s *session) call(name string, args map[string]any) result {
	s.t.Helper()
	return s.callWithin(callTimeout, name, args)
}

func (s *session) callWithin(limit time.Duration, name string, args map[string]any) result {
	s.t.Helper()
	var out result
	s.requestWithin(limit, "tools/call", map[string]any{"name": name, "arguments": args}, &out)
	return out
}

// ok decodes the structured content of a call that must have succeeded.
func (s *session) ok(name string, args map[string]any, out any) {
	s.t.Helper()
	res := s.call(name, args)
	res.mustSucceed(s.t, name)
	if out == nil {
		return
	}
	if len(res.Structured) == 0 {
		s.t.Fatalf("%s: %s returned no structured content", s.user, name)
	}
	if err := json.Unmarshal(res.Structured, out); err != nil {
		s.t.Fatalf("%s: decoding %s: %v", s.user, name, err)
	}
}

func (r result) text() string {
	var parts []string
	for _, content := range r.Content {
		if content.Type == "text" {
			parts = append(parts, content.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (r result) mustSucceed(t *testing.T, name string) {
	t.Helper()
	if r.IsError {
		t.Fatalf("%s failed: %s", name, truncate(r.text()))
	}
}

// mustProblem requires a call to have failed with one problem type, and
// returns it so a test can read its instance.
func (r result) mustProblem(t *testing.T, name, slug string) problemDetails {
	t.Helper()
	if !r.IsError {
		t.Fatalf("%s succeeded, want the %s problem: %s", name, slug, truncate(string(r.Structured)))
	}
	var p problemDetails
	if err := json.Unmarshal([]byte(r.text()), &p); err != nil {
		t.Fatalf("%s answered an error that is not problem details: %s", name, truncate(r.text()))
	}
	want := "https://kitbash.zyx.tw/errors/" + slug
	if p.Type != want {
		t.Fatalf("%s answered %s, want %s: %s", name, p.Type, want, p.Detail)
	}
	return p
}

// close ends the session the way a disconnecting agent does, by closing stdin,
// and waits for the exit. Telemetry is flushed at exit, so a test that queries
// what a session recorded closes it first.
func (s *session) close() {
	if s.closed {
		return
	}
	s.closed = true
	_ = s.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			s.t.Errorf("%s: the session exited with %v (see %s)", s.user, err, s.stderr.Name())
		}
	case <-time.After(closeTimeout):
		_ = s.cmd.Process.Kill()
		s.t.Errorf("%s: the session did not exit within %s (see %s)", s.user, closeTimeout, s.stderr.Name())
	}
	_ = s.stderr.Close()
}

func truncate(text string) string {
	const limit = 2000
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "... (truncated)"
}
