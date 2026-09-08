package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"google.golang.org/protobuf/proto"

	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/zyx1121/kitbash/internal/otlp"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// childMarker is how the test binary knows it was started as the kitbash-mcp
// of a session rather than as the test run. It travels in the arguments and
// not in the environment, so the assertion that a child inherits nothing of
// the daemon's environment stays exact.
const childMarker = "--kitbash-mcp-child"

// TestMCPChildHelper is the fake kitbash-mcp. Under an ordinary test run it
// does nothing; started with the marker it serves MCP over stdio, records the
// environment it was given and notes when it exits, which is how the daemon
// tests are able to see the child at all.
func TestMCPChildHelper(t *testing.T) {
	marker := slices.Index(os.Args, childMarker)
	if marker < 0 || marker+1 >= len(os.Args) {
		return
	}
	dump := os.Args[marker+1]
	if err := os.WriteFile(dump, []byte(strings.Join(os.Environ(), "\n")), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "child: writing the environment: %v\n", err)
		os.Exit(1)
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "kitbash-mcp-fake", Version: "test"}, nil)
	srv.AddTool(&mcp.Tool{
		Name:         "fs_list",
		Description:  "List the folders of the caller",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"folders":{"type":"array","items":{"type":"string"}}}}`),
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Path string `json:"path"`
		}
		if req.Params != nil && len(req.Params.Arguments) > 0 {
			json.Unmarshal(req.Params.Arguments, &args)
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: "listed " + args.Path}},
			StructuredContent: map[string]any{"folders": []string{args.Path}},
		}, nil
	})
	srv.AddTool(&mcp.Tool{
		Name:        "proc_run",
		Description: "Run a Package, which puts one more tool on the surface",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// A Process the owner starts while the session is open publishes its
		// tools, which is the notification the daemon has to relay.
		srv.AddTool(&mcp.Tool{
			Name:        "ffmpeg_transcode",
			Description: "A tool of a Process started after the session opened",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "transcoded"}}}, nil
		})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ran"}}}, nil
	})

	err := srv.Run(context.Background(), &mcp.StdioTransport{})
	os.WriteFile(dump+".exit", []byte("exited\n"), 0o600)
	if err != nil && err != io.EOF {
		os.Exit(1)
	}
	os.Exit(0)
}

// sessionCall is one recorded request for a child, with the member it was to
// run as.
type sessionCall struct {
	Member sysusers.Member
	Binary string
	Caller string
	Dump   string
}

// fakeSessions starts the test binary as the child rather than kitbash-mcp,
// with the environment the host would give it and nothing more.
type fakeSessions struct {
	dir string

	mu    sync.Mutex
	calls []sessionCall
	fail  error
}

func (f *fakeSessions) MCPCommand(ctx context.Context, m sysusers.Member, binary, caller string) (*exec.Cmd, error) {
	f.mu.Lock()
	if f.fail != nil {
		err := f.fail
		f.mu.Unlock()
		return nil, err
	}
	dump := filepath.Join(f.dir, fmt.Sprintf("child-%d.env", len(f.calls)))
	f.calls = append(f.calls, sessionCall{Member: m, Binary: binary, Caller: caller, Dump: dump})
	f.mu.Unlock()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestMCPChildHelper", "--", childMarker, dump)
	// The environment is the one the real runner builds, so what the child
	// reports having is what a kitbash host would have given it.
	cmd.Env = sysusers.MCPEnvironment(m, sysusers.DefaultRunUser, caller)
	cmd.Dir = "/"
	return cmd, nil
}

func (f *fakeSessions) recorded() []sessionCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sessionCall(nil), f.calls...)
}

// mcpHarness is a daemon serving the Process receiver with one registered
// Process, its token, and the fake that starts the children.
type mcpHarness struct {
	*harness
	base     string
	token    string
	process  string
	sessions *fakeSessions
	members  *sysusers.Fake
}

// serveMCPDaemon starts a daemon whose MCP sessions run the test binary, and
// registers one Process of the current user.
func serveMCPDaemon(t *testing.T, idle time.Duration) *mcpHarness {
	t.Helper()
	members := sysusers.NewFake()
	sessions := &fakeSessions{dir: t.TempDir()}
	h := serveWith(t, Options{
		Admin:     func(*user.User) (bool, error) { return false, nil },
		Users:     members,
		Runner:    members,
		Sessions:  sessions,
		MCPBinary: "/usr/bin/kitbash-mcp",
		MCPIdle:   idle,
	})
	me, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	uid, err := strconv.Atoi(me.Uid)
	if err != nil {
		t.Fatalf("uid %q: %v", me.Uid, err)
	}
	members.Add(sysusers.Member{Name: h.user, UID: uid, GID: uid, Home: t.TempDir()})

	req := registration("")
	req.Expose = ExposeMCP
	token, res, body := h.register(req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("register: status %d, body %s", res.StatusCode, body)
	}
	return &mcpHarness{
		harness:  h,
		base:     h.serveTCP(),
		token:    token,
		process:  req.ID,
		sessions: sessions,
		members:  members,
	}
}

// bearerHeader adds the Process token to every request the MCP client makes, which
// is the only identity the receiver reads.
type bearerHeader struct {
	token string
	next  http.RoundTripper
}

func (b bearerHeader) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	if b.token != "" {
		clone.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.next.RoundTrip(clone)
}

// connect opens one MCP session over streamable HTTP, the way a Process would.
func (m *mcpHarness) connect(t *testing.T, changed chan<- struct{}) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "process", Version: "test"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			if changed != nil {
				select {
				case changed <- struct{}{}:
				default:
				}
			}
		},
	})
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   m.base + MCPPath,
		HTTPClient: &http.Client{Transport: bearerHeader{token: m.token, next: http.DefaultTransport}, Timeout: 10 * time.Second},
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return session
}

// initialize sends one raw initialize, which is what a client that has no
// session yet sends. It is how the tests read the status and the problem of a
// refusal, which the SDK client hides behind its own error.
func (m *mcpHarness) initialize(t *testing.T, token string) (*http.Response, []byte) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18",` +
		`"capabilities":{},"clientInfo":{"name":"process","version":"test"}}}`
	req, err := http.NewRequest(http.MethodPost, m.base+MCPPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer res.Body.Close()
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return res, got
}

// sessions is how many MCP sessions health reports.
func (m *mcpHarness) live(t *testing.T) int {
	t.Helper()
	res, body := m.do(http.MethodGet, healthPath, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("health: status %d, body %s", res.StatusCode, body)
	}
	var got healthResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("health body %q: %v", body, err)
	}
	return got.MCPSessions
}

// waitFor polls until the condition holds or the test gives up, which is how
// the tests wait for a child to exit or a session to be swept.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// exited reports whether the child of one session has exited.
func exited(call sessionCall) bool {
	_, err := os.Stat(call.Dump + ".exit")
	return err == nil
}

// toolNames are the names one session publishes, sorted.
func toolNames(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func TestMCPRefusesARequestWithoutAToken(t *testing.T) {
	m := serveMCPDaemon(t, 0)

	res, body := m.initialize(t, "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body %s", res.StatusCode, body)
	}
	if got := res.Header.Get("WWW-Authenticate"); got != BearerChallenge {
		t.Errorf("WWW-Authenticate = %q, want the bearer challenge", got)
	}
	p := m.problemOf(res, body)
	if p.Slug() != problem.SlugNotPermitted {
		t.Errorf("problem is %s, want not-permitted", p.Slug())
	}
	if calls := m.sessions.recorded(); len(calls) != 0 {
		t.Errorf("a request without a token started %d children, want none", len(calls))
	}

	res, body = m.initialize(t, "not-a-token")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown token status = %d, want 401, body %s", res.StatusCode, body)
	}
}

func TestMCPSessionPublishesTheChildTools(t *testing.T) {
	m := serveMCPDaemon(t, 0)
	changed := make(chan struct{}, 4)
	session := m.connect(t, changed)
	defer session.Close()

	if got := toolNames(t, session); !slices.Equal(got, []string{"fs_list", "proc_run"}) {
		t.Fatalf("tools = %v, want the child's two", got)
	}
	if n := m.live(t); n != 1 {
		t.Errorf("health reports %d MCP sessions, want 1", n)
	}

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "fs_list",
		Arguments: map[string]any{"path": "/org"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("the call failed: %+v", res.Content)
	}
	structured, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if want := `{"folders":["/org"]}`; string(structured) != want {
		t.Errorf("structured content = %s, want %s", structured, want)
	}
	if len(res.Content) != 1 {
		t.Fatalf("content = %+v, want the child's one block", res.Content)
	}
	if text, ok := res.Content[0].(*mcp.TextContent); !ok || text.Text != "listed /org" {
		t.Errorf("content = %+v, want the child's text unchanged", res.Content[0])
	}

	// A Process the owner starts while the session is open appears without
	// the session reconnecting, see PLAN.md section 2.3.
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "proc_run"}); err != nil {
		t.Fatalf("proc_run: %v", err)
	}
	waitFor(t, "the new tool to reach the session", func() bool {
		return slices.Contains(toolNames(t, session), "ffmpeg_transcode")
	})
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Error("the session sent no tools/list_changed of its own")
	}
	res, err = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ffmpeg_transcode"})
	if err != nil {
		t.Fatalf("ffmpeg_transcode: %v", err)
	}
	if res.IsError {
		t.Errorf("the proxied call failed: %+v", res.Content)
	}
}

func TestMCPSessionRunsAsTheOwnerWithNothingOfTheDaemons(t *testing.T) {
	t.Setenv("KITBASH_SOCKET", "/run/kitbash/leaked.sock")
	m := serveMCPDaemon(t, 0)
	session := m.connect(t, nil)
	defer session.Close()

	calls := m.sessions.recorded()
	if len(calls) != 1 {
		t.Fatalf("children started = %d, want one", len(calls))
	}
	call := calls[0]
	if call.Member.Name != m.user {
		t.Errorf("the child ran as %q, want the owner %q", call.Member.Name, m.user)
	}
	if call.Caller != m.process {
		t.Errorf("%s = %q, want the Process id %q", sysusers.EnvCaller, call.Caller, m.process)
	}
	if call.Binary != "/usr/bin/kitbash-mcp" {
		t.Errorf("the child is %q, want the configured kitbash-mcp", call.Binary)
	}

	raw, err := os.ReadFile(call.Dump)
	if err != nil {
		t.Fatalf("the child recorded no environment: %v", err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			got[key] = value
		}
	}
	want := map[string]string{
		"HOME":             call.Member.Home,
		"USER":             m.user,
		"LOGNAME":          m.user,
		"PATH":             sysusers.RunnerPath,
		"XDG_RUNTIME_DIR":  filepath.Join(sysusers.DefaultRunUser, strconv.Itoa(call.Member.UID)),
		sysusers.EnvCaller: m.process,
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("the child read %s=%q, want %q", key, got[key], value)
		}
	}
	for key := range got {
		if _, expected := want[key]; !expected {
			t.Errorf("the child inherited %s from the daemon, which it must not", key)
		}
	}
}

func TestMCPDeleteEndsTheSessionAndTheChild(t *testing.T) {
	m := serveMCPDaemon(t, 0)
	session := m.connect(t, nil)
	if n := m.live(t); n != 1 {
		t.Fatalf("health reports %d MCP sessions, want 1", n)
	}

	// Closing the client session is a DELETE on the endpoint.
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	calls := m.sessions.recorded()
	if len(calls) != 1 {
		t.Fatalf("children started = %d, want one", len(calls))
	}
	waitFor(t, "the child to exit", func() bool { return exited(calls[0]) })
	waitFor(t, "the session to be released", func() bool { return m.live(t) == 0 })
}

func TestMCPNinthSessionIsRefused(t *testing.T) {
	m := serveMCPDaemon(t, 0)
	for i := range MaxMCPSessions {
		session := m.connect(t, nil)
		defer session.Close()
		if n := m.live(t); n != i+1 {
			t.Fatalf("health reports %d MCP sessions, want %d", n, i+1)
		}
	}

	res, body := m.initialize(t, m.token)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the ninth session answered %d, want 429, body %s", res.StatusCode, body)
	}
	p := m.problemOf(res, body)
	if p.Slug() != problem.SlugConflict {
		t.Errorf("problem is %s, want conflict", p.Slug())
	}
	if calls := m.sessions.recorded(); len(calls) != MaxMCPSessions {
		t.Errorf("children started = %d, want %d; the refusal must start none",
			len(calls), MaxMCPSessions)
	}
}

func TestMCPIdleSessionEnds(t *testing.T) {
	m := serveMCPDaemon(t, 100*time.Millisecond)
	session := m.connect(t, nil)
	defer session.Close()

	calls := m.sessions.recorded()
	if len(calls) != 1 {
		t.Fatalf("children started = %d, want one", len(calls))
	}
	waitFor(t, "the idle session to end", func() bool { return m.live(t) == 0 })
	waitFor(t, "the child of the idle session to exit", func() bool { return exited(calls[0]) })

	// The session is gone, so the client that held it is told rather than
	// answered with somebody else's session.
	if _, err := session.ListTools(context.Background(), nil); err == nil {
		t.Error("the ended session still answered a request")
	}
}

func TestMCPUnregisteringTheProcessEndsItsSessions(t *testing.T) {
	m := serveMCPDaemon(t, 0)
	session := m.connect(t, nil)
	defer session.Close()

	res, body := m.do(http.MethodDelete, processesPath+"/"+m.process, "", nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("unregister: status %d, body %s", res.StatusCode, body)
	}
	calls := m.sessions.recorded()
	if len(calls) != 1 {
		t.Fatalf("children started = %d, want one", len(calls))
	}
	waitFor(t, "the child of the unregistered Process to exit", func() bool { return exited(calls[0]) })
	if n := m.live(t); n != 0 {
		t.Errorf("health reports %d MCP sessions, want none", n)
	}
	// The token is revoked with the registration, so nothing opens another.
	res, body = m.initialize(t, m.token)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a revoked token answered %d, want 401, body %s", res.StatusCode, body)
	}
}

func TestMCPSessionOfAnotherProcessIsNotReachable(t *testing.T) {
	m := serveMCPDaemon(t, 0)
	session := m.connect(t, nil)
	defer session.Close()

	other := registration("")
	other.Expose = ExposeMCP
	otherToken, res, body := m.register(other)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("register: status %d, body %s", res.StatusCode, body)
	}
	held := ""
	for _, sess := range m.server.mcpSessions.all() {
		held = sess.id
	}
	if held == "" {
		t.Fatal("the daemon holds no session to name")
	}

	req, err := http.NewRequest(http.MethodDelete, m.base+MCPPath, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+otherToken)
	req.Header.Set(MCPSessionHeader, held)
	answer, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("DELETE /mcp: %v", err)
	}
	defer answer.Body.Close()
	if answer.StatusCode != http.StatusForbidden {
		t.Fatalf("another Process ended the session with status %d, want 403", answer.StatusCode)
	}
	if n := m.live(t); n != 1 {
		t.Errorf("health reports %d MCP sessions, want the one that is still open", n)
	}
}

// TestMCPStoppingTheReceiverEndsItsSessions is the shutdown path: a session
// holds an event stream open, which is a request in flight, so a receiver that
// waited for it would take the whole shutdown timeout to stop. The session is
// left open on purpose here: the harness fails the test if ServeTCP does not
// return promptly once its context is cancelled.
func TestMCPStoppingTheReceiverEndsItsSessions(t *testing.T) {
	m := serveMCPDaemon(t, 0)
	session := m.connect(t, nil)
	if got := toolNames(t, session); len(got) != 2 {
		t.Fatalf("tools = %v, want the child's two", got)
	}
	if n := m.live(t); n != 1 {
		t.Fatalf("health reports %d MCP sessions, want 1", n)
	}
}

func TestMCPIsNotServedOverTheSocket(t *testing.T) {
	m := serveMCPDaemon(t, 0)
	res, body := m.do(http.MethodPost, MCPPath, "application/json", []byte("{}"))
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body %s", res.StatusCode, body)
	}
	if p := m.problemOf(res, body); p.Slug() != problem.SlugNotFound {
		t.Errorf("problem is %s, want not-found", p.Slug())
	}
}

func TestMCPRefusesABodyOverTheCap(t *testing.T) {
	m := serveMCPDaemon(t, 0)
	body := bytes.Repeat([]byte("a"), (4<<20)+1)
	req, err := http.NewRequest(http.MethodPost, m.base+MCPPath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+m.token)
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer res.Body.Close()
	answer, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body %s", res.StatusCode, answer)
	}
	if p := m.problemOf(res, answer); p.Slug() != problem.SlugTooLarge {
		t.Errorf("problem is %s, want too-large", p.Slug())
	}
	if calls := m.sessions.recorded(); len(calls) != 0 {
		t.Errorf("a body over the cap started %d children, want none", len(calls))
	}
}

func TestMCPRefusesARequestThatOpensNoSession(t *testing.T) {
	m := serveMCPDaemon(t, 0)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	req, err := http.NewRequest(http.MethodPost, m.base+MCPPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+m.token)
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer res.Body.Close()
	answer, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body %s", res.StatusCode, answer)
	}
	if calls := m.sessions.recorded(); len(calls) != 0 {
		t.Errorf("a request that opens no session started %d children, want none", len(calls))
	}
}

// callerExport is one span as the kitbash-mcp of a Process session records it:
// the owner's user, and the calling Process as kitbash.caller.
func callerExport(name, caller string, start time.Time) []byte {
	attrs := []*commonpb.KeyValue{{
		Key:   otlp.AttrTool,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: name}},
	}}
	if caller != "" {
		attrs = append(attrs, &commonpb.KeyValue{
			Key:   otlp.AttrCaller,
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: caller}},
		})
	}
	body, err := proto.Marshal(&coltrace.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: []*tracepb.Span{{
					TraceId:           bytes.Repeat([]byte{0xab}, 16),
					SpanId:            bytes.Repeat([]byte{0xcd}, 8),
					Name:              name,
					StartTimeUnixNano: uint64(start.UnixNano()),
					EndTimeUnixNano:   uint64(start.Add(time.Millisecond).UnixNano()),
					Attributes:        attrs,
				}},
			}},
		}},
	})
	if err != nil {
		panic(err)
	}
	return body
}

// TestQueryFiltersByCaller is what makes "what a Process did on its owner's
// behalf is one query" true, see PLAN.md section 2.3.
func TestQueryFiltersByCaller(t *testing.T) {
	m := serveMCPDaemon(t, 0)
	now := time.Now()
	for _, export := range [][]byte{
		callerExport("fs_list", "", now.Add(-time.Minute)),
		callerExport("fs_write", m.process, now.Add(-2*time.Minute)),
	} {
		if res, body := m.do(http.MethodPost, pathTraces, otlp.ContentTypeProtobuf, export); res.StatusCode != http.StatusOK {
			t.Fatalf("export = %d, body %s", res.StatusCode, body)
		}
	}

	res, body := m.postJSON(http.MethodPost, queryPath,
		queryRequest{Signal: store.SignalTraces, Caller: m.process})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("query status = %d, body %s", res.StatusCode, body)
	}
	var page struct {
		Records []spanRecord `json:"records"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(page.Records) != 1 || page.Records[0].Name != "fs_write" {
		t.Fatalf("the caller filter returned %+v, want the one span the Process recorded", page.Records)
	}
	if page.Records[0].Attributes.Caller != m.process {
		t.Errorf("caller = %q, want %q", page.Records[0].Attributes.Caller, m.process)
	}
	if !bytes.Contains(body, []byte(`"caller":"`+m.process+`"`)) {
		t.Errorf("the record does not publish attributes.caller: %s", body)
	}
}
