package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// builtInTools is the whole built in surface: 4 fs, 4 pkg, 4 proc, 2 tel,
// 5 users and 3 approvals, the number the README's verify step counts.
const builtInTools = 22

// The Package this job writes, builds and runs. The folder is the admin's own,
// the manifest is fixtures/echo, and the container name is the one proc_run
// derives from the Package and the Process name.
const (
	packageName = "echo"
	container   = "kitbash-echo-echo"
	echoTool    = "echo_echo"
)

// clientDir holds the second MCP client and the node_modules the job installed
// beside it; clientMount is where podman cp puts them in the container.
const (
	clientDir   = "client"
	clientMount = "/e2e-client"
)

// fixtures are the files of the echo Package, written in this order: the
// manifest first, because that is what makes the folder visible.
var fixtures = []string{"kitbash.yaml", "Dockerfile", "server.js"}

// state is what one step of the job leaves for the next.
type state struct {
	admin      *session
	member     *session
	pkgPath    string
	orgFile    string
	digest     string
	processID  string
	approvalID string
	echoText   string
}

// TestSurface is the job: one admin and one member driving the whole surface
// on a real host, in the order an organization uses it.
func TestSurface(t *testing.T) {
	requireHost(t)
	s := &state{
		pkgPath:  filepath.Join("/home", adminName(), packageName),
		orgFile:  "/org/handbook/e2e.md",
		echoText: fmt.Sprintf("end to end at %s", time.Now().UTC().Format(time.RFC3339)),
	}
	steps := []struct {
		name string
		run  func(*testing.T, *state)
	}{
		{"the admin is served the built in surface", theBuiltInSurface},
		{"fs_write writes the echo Package", writeThePackage},
		{"a schema violation is a bad request", schemaViolation},
		{"pkg_build builds it with rootless podman", buildThePackage},
		{"proc_run starts it and echo_echo answers", runThePackage},
		{"the second client calls it over /mcp", secondClient},
		{"users_create adds a member", createAMember},
		{"a member cannot list another home", anotherHome},
		{"a member's write to /org is queued", queuedWrite},
		{"the admin approves it and the member is the author", approveTheWrite},
		{"tel_query returns the span and the build log", queryTelemetry},
		{"the new member is served their own identity", theNewMember},
	}
	// The steps are one story and share the host, so they run in order on one
	// test rather than as subtests: the first failure ends the job, and the
	// sessions a step opened stay open for the next.
	for _, step := range steps {
		t.Logf("step: %s", step.name)
		step.run(t, s)
	}
}

func theBuiltInSurface(t *testing.T, s *state) {
	s.admin = dial(t, adminName())
	names := s.admin.tools()
	if len(names) != builtInTools {
		t.Fatalf("the admin was served %d tools, want %d: %s",
			len(names), builtInTools, strings.Join(names, " "))
	}
	var me struct {
		User  string `json:"user"`
		Admin bool   `json:"admin"`
	}
	s.admin.ok("users_me", map[string]any{}, &me)
	if me.User != adminName() || !me.Admin {
		t.Fatalf("users_me answered %+v, want %s as an admin", me, adminName())
	}
}

func writeThePackage(t *testing.T, s *state) {
	for _, name := range fixtures {
		body, err := os.ReadFile(filepath.Join("fixtures", packageName, name))
		if err != nil {
			t.Fatalf("reading the fixture %s: %v", name, err)
		}
		var out struct {
			Path   string `json:"path"`
			Commit struct {
				Sha    string `json:"sha"`
				Author string `json:"author"`
			} `json:"commit"`
		}
		s.admin.ok("fs_write", map[string]any{
			"path":    filepath.Join(s.pkgPath, name),
			"content": string(body),
			"message": "Add " + name + " of the end to end Package",
		}, &out)
		if out.Commit.Author != adminName() || len(out.Commit.Sha) != 40 {
			t.Fatalf("fs_write %s committed %+v, want a commit by %s", name, out.Commit, adminName())
		}
	}
}

func schemaViolation(t *testing.T, s *state) {
	// message is minLength 3 in the published input schema, so this never
	// reaches the handler: the surface answers a problem rather than a bare
	// protocol error.
	res := s.admin.call("fs_write", map[string]any{
		"path":    filepath.Join(s.pkgPath, "violation.txt"),
		"content": "nothing is written",
		"message": "no",
	})
	res.mustProblem(t, "fs_write", "bad-request")
}

func buildThePackage(t *testing.T, s *state) {
	var out struct {
		Path   string `json:"path"`
		Digest string `json:"digest"`
		Commit string `json:"commit"`
		Log    string `json:"log"`
	}
	res := s.admin.callWithin(buildTimeout, "pkg_build", map[string]any{"path": s.pkgPath})
	res.mustSucceed(t, "pkg_build")
	if err := json.Unmarshal(res.Structured, &out); err != nil {
		t.Fatalf("decoding pkg_build: %v", err)
	}
	if !strings.HasPrefix(out.Digest, "sha256:") || len(out.Digest) != 71 {
		t.Fatalf("pkg_build answered the digest %q, want sha256 and 64 hexadecimal digits", out.Digest)
	}
	if out.Log == "" {
		t.Fatalf("pkg_build returned no build log")
	}
	s.digest = out.Digest
	t.Logf("built %s at %s", s.pkgPath, out.Digest)
}

func runThePackage(t *testing.T, s *state) {
	var out struct {
		ID     string   `json:"id"`
		Name   string   `json:"name"`
		State  string   `json:"state"`
		Expose string   `json:"expose"`
		Digest string   `json:"digest"`
		Tools  []string `json:"tools"`
	}
	s.admin.ok("proc_run", map[string]any{"package": s.pkgPath}, &out)
	if out.State != "running" || out.Expose != "mcp" {
		t.Fatalf("proc_run answered %+v, want a running Process exposing mcp", out)
	}
	if out.Digest != s.digest {
		t.Fatalf("proc_run started %s, want the digest pkg_build answered, %s", out.Digest, s.digest)
	}
	if len(out.Tools) != 1 || out.Tools[0] != echoTool {
		t.Fatalf("proc_run published %v, want %s", out.Tools, echoTool)
	}
	s.processID = out.ID

	// The Package's tool joins the session that started it, and answering it
	// is a podman exec into the container that is now running.
	names := s.admin.tools()
	if len(names) != builtInTools+1 {
		t.Fatalf("the surface is %d tools after proc_run, want %d: %s",
			len(names), builtInTools+1, strings.Join(names, " "))
	}
	var echoed struct {
		Text string `json:"text"`
	}
	s.admin.ok(echoTool, map[string]any{"text": s.echoText}, &echoed)
	if echoed.Text != s.echoText {
		t.Fatalf("%s answered %q, want %q", echoTool, echoed.Text, s.echoText)
	}
}

// secondClient runs the TypeScript SDK against /mcp from inside the Process's
// own container. The Process token lives only there, so the client that bears
// it has to run there too.
func secondClient(t *testing.T, s *state) {
	local, err := filepath.Abs(clientDir)
	if err != nil {
		t.Fatalf("resolving %s: %v", clientDir, err)
	}
	if _, err := os.Stat(filepath.Join(local, "node_modules")); err != nil {
		t.Fatalf("the second client has no node_modules: %v", err)
	}
	if out, err := runAs(t, adminName(),
		"podman", "cp", local, container+":"+clientMount); err != nil {
		t.Fatalf("copying the client into %s: %v\n%s", container, err, out)
	}
	text := s.echoText + " over /mcp"
	out, err := runAs(t, adminName(),
		"podman", "exec", "--env", "E2E_ECHO_TEXT="+text, container,
		"node", clientMount+"/mcp.mjs")
	if err != nil {
		t.Fatalf("the second client failed: %v\n%s", err, out)
	}
	var answer struct {
		OK     bool     `json:"ok"`
		Tools  []string `json:"tools"`
		Echoed struct {
			Text string `json:"text"`
		} `json:"echoed"`
	}
	last := lastJSONLine(out)
	if err := json.Unmarshal([]byte(last), &answer); err != nil {
		t.Fatalf("the second client answered %q: %v", truncate(out), err)
	}
	if !answer.OK || answer.Echoed.Text != text {
		t.Fatalf("the second client answered %+v, want %q echoed", answer, text)
	}
	t.Logf("the second client was served %s over /mcp", strings.Join(answer.Tools, " "))
}

func createAMember(t *testing.T, s *state) {
	var out struct {
		User  string `json:"user"`
		UID   int    `json:"uid"`
		Admin bool   `json:"admin"`
	}
	s.admin.ok("users_create", map[string]any{
		"name":   secondName(),
		"sshKey": sshKey(t),
	}, &out)
	if out.User != secondName() || out.UID == 0 || out.Admin {
		t.Fatalf("users_create answered %+v, want %s as a regular member", out, secondName())
	}
}

func anotherHome(t *testing.T, s *state) {
	s.member = dial(t, memberName())
	res := s.member.call("fs_list", map[string]any{"path": filepath.Join("/home", adminName())})
	res.mustProblem(t, "fs_list", "invalid-path")
}

func queuedWrite(t *testing.T, s *state) {
	res := s.member.call("fs_write", map[string]any{
		"path":    s.orgFile,
		"content": "# End to end\n\nWritten by a member, landed by an admin.\n",
		"message": "Add the end to end note to the handbook",
	})
	queued := res.mustProblem(t, "fs_write", "queued")
	if queued.Status != 202 || queued.Instance == "" {
		t.Fatalf("the queued problem is %+v, want status 202 and the approval id as its instance", queued)
	}
	s.approvalID = queued.Instance
}

func approveTheWrite(t *testing.T, s *state) {
	// Both sessions are closed first: Telemetry is flushed at exit, and the
	// query below reads what these two recorded.
	s.member.close()
	s.admin.close()
	s.admin = dial(t, adminName())

	var pending struct {
		Approvals []struct {
			ID        string `json:"id"`
			Requester string `json:"requester"`
			Tool      string `json:"tool"`
			State     string `json:"state"`
		} `json:"approvals"`
	}
	s.admin.ok("approvals_list", map[string]any{"state": "pending"}, &pending)
	found := false
	for _, approval := range pending.Approvals {
		if approval.ID == s.approvalID {
			found = true
			if approval.Requester != memberName() || approval.Tool != "fs_write" {
				t.Fatalf("the approval is %+v, want an fs_write by %s", approval, memberName())
			}
		}
	}
	if !found {
		t.Fatalf("approvals_list does not hold %s: %+v", s.approvalID, pending.Approvals)
	}

	var approved struct {
		ID     string `json:"id"`
		State  string `json:"state"`
		Result struct {
			Path   string `json:"path"`
			Commit struct {
				Sha    string `json:"sha"`
				Author string `json:"author"`
			} `json:"commit"`
		} `json:"result"`
	}
	s.admin.ok("approvals_approve", map[string]any{"id": s.approvalID}, &approved)
	if approved.State != "approved" {
		t.Fatalf("approvals_approve answered %+v, want the approved state", approved)
	}
	if approved.Result.Path != s.orgFile || approved.Result.Commit.Author != memberName() {
		t.Fatalf("the approval landed %+v, want %s authored by %s",
			approved.Result, s.orgFile, memberName())
	}

	// The file is in /org for everyone, and its history names the member.
	var history struct {
		Commits []struct {
			Author  string `json:"author"`
			Message string `json:"message"`
		} `json:"commits"`
	}
	s.admin.ok("fs_history", map[string]any{"path": s.orgFile}, &history)
	if len(history.Commits) == 0 || history.Commits[0].Author != memberName() {
		t.Fatalf("fs_history answered %+v, want a commit by %s", history.Commits, memberName())
	}
}

func queryTelemetry(t *testing.T, s *state) {
	var traces struct {
		Records []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Attributes struct {
				User string `json:"user"`
				Tool string `json:"tool"`
			} `json:"attributes"`
		} `json:"records"`
	}
	s.admin.ok("tel_query", map[string]any{
		"signal": "traces",
		"user":   adminName(),
		"tool":   echoTool,
	}, &traces)
	if len(traces.Records) == 0 {
		t.Fatalf("tel_query answered no %s span", echoTool)
	}
	for _, record := range traces.Records {
		if record.Attributes.User != adminName() {
			t.Fatalf("a span of %s is attributed to %s", echoTool, record.Attributes.User)
		}
	}

	var logs struct {
		Records []struct {
			Body       string `json:"body"`
			Severity   string `json:"severity"`
			Attributes struct {
				Tool    string `json:"tool"`
				Package string `json:"package"`
			} `json:"attributes"`
		} `json:"records"`
	}
	s.admin.ok("tel_query", map[string]any{
		"signal": "logs",
		"user":   adminName(),
		"tool":   "pkg_build",
	}, &logs)
	if len(logs.Records) == 0 {
		t.Fatalf("tel_query answered no pkg_build log record")
	}
	if logs.Records[0].Body == "" {
		t.Fatalf("the pkg_build log record carries no body: %+v", logs.Records[0])
	}
	t.Logf("tel_query answered %d spans and %d log records", len(traces.Records), len(logs.Records))
}

func theNewMember(t *testing.T, s *state) {
	member := dial(t, secondName())
	var me struct {
		User  string `json:"user"`
		Admin bool   `json:"admin"`
	}
	member.ok("users_me", map[string]any{}, &me)
	if me.User != secondName() || me.Admin {
		t.Fatalf("users_me answered %+v, want %s as a regular member", me, secondName())
	}
	member.close()
}

// lastJSONLine is the answer a script printed after whatever it logged before.
func lastJSONLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "{") && json.Valid([]byte(line)) {
			return line
		}
	}
	return ""
}
