package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// builtIn is the whole built in surface, named rather than counted: 4 fs,
// 4 pkg, 4 proc, 2 tel, 5 users and 3 approvals, the 22 the README's verify
// step counts and the families of spec/mcp-surface.yaml one for one. A tool
// that is renamed is a client that breaks, so the names are the assertion.
var builtIn = []string{
	"approvals_approve", "approvals_list", "approvals_reject",
	"fs_history", "fs_list", "fs_read", "fs_write",
	"pkg_build", "pkg_import", "pkg_inspect", "pkg_list",
	"proc_list", "proc_logs", "proc_run", "proc_stop",
	"tel_query", "tel_retention",
	"users_add_key", "users_create", "users_list", "users_me", "users_remove",
}

// The Package this job writes, builds and runs. The folder is the admin's own,
// the manifest is fixtures/echo, and the container name is the one proc_run
// derives from the Package and the Process name.
const (
	packageName = "echo"
	container   = "kitbash-echo-echo"
	echoTool    = "echo_echo"
)

// clientMount is where podman cp puts the second MCP client in the container.
// Where it comes from is the job's to say: a member has to be able to read it,
// and the checkout is under a home that is not theirs.
const (
	clientEnv   = "KITBASH_E2E_CLIENT"
	clientMount = "/e2e-client"
)

// The run kit this job dispatches to, and the Package that names it. The kit
// starts nothing: what is proved is that proc_run reached it with the hook's
// arguments and registered the Process it answered with, see PLAN.md section 3.
const (
	runnerName    = "runner"
	elsewhereName = "elsewhere"
	runTool       = "runner_run"
	stopTool      = "runner_stop"
	callsTool     = "runner_calls"
)

// fixtures are the files of the echo and runner Packages, written in this
// order: the manifest first, because that is what makes the folder visible.
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
	// The run kit, the Package that names it, and the Process the kit
	// answered proc_run with.
	runnerPath     string
	elsewherePath  string
	elsewhereID    string
	elsewhereImage string
}

// TestSurface is the job: one admin and one member driving the whole surface
// on a real host, in the order an organization uses it.
func TestSurface(t *testing.T) {
	requireHost(t)
	s := &state{
		pkgPath:       filepath.Join("/home", adminName(), packageName),
		runnerPath:    filepath.Join("/home", adminName(), runnerName),
		elsewherePath: filepath.Join("/home", adminName(), elsewhereName),
		orgFile:       "/org/handbook/e2e.md",
		echoText:      fmt.Sprintf("end to end at %s", time.Now().UTC().Format(time.RFC3339)),
	}
	steps := []struct {
		name string
		run  func(*testing.T, *state)
	}{
		{"the host runs the release the job built", theHostsRelease},
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
		{"a run kit is built and joins the surface", buildTheRunKit},
		{"proc_run dispatches to the run kit", dispatchToTheRunKit},
		{"proc_stop forwards to the run kit", stopThroughTheRunKit},
	}
	// The steps are one story and share the host, so they run in order on one
	// test rather than as subtests: the first failure ends the job, and the
	// sessions a step opened stay open for the next.
	for _, step := range steps {
		t.Logf("step: %s", step.name)
		step.run(t, s)
	}
}

// theHostsRelease is the first step because everything after it is about a
// build: the daemon on the socket and the kitbash-mcp a session runs are the
// binaries this job built, not whatever was on the host before.
func theHostsRelease(t *testing.T, _ *state) {
	theRelease(t)
}

func theBuiltInSurface(t *testing.T, s *state) {
	s.admin = dial(t, adminName())
	names := s.admin.tools()
	sort.Strings(names)
	if strings.Join(names, " ") != strings.Join(builtIn, " ") {
		t.Fatalf("the admin was served %d tools:\n%s\nwant %d:\n%s",
			len(names), strings.Join(names, " "), len(builtIn), strings.Join(builtIn, " "))
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
	writeFixture(t, s.admin, packageName, s.pkgPath)
}

// writeFixture writes one fixture Package into Files as the session's member,
// one file per commit, the manifest first because that is what makes the
// folder visible.
func writeFixture(t *testing.T, session *session, fixture, target string) {
	t.Helper()
	for _, name := range fixtures {
		body, err := os.ReadFile(filepath.Join("fixtures", fixture, name))
		if err != nil {
			t.Fatalf("reading the fixture %s: %v", name, err)
		}
		writeFile(t, session, filepath.Join(target, name), string(body))
	}
}

// writeFile writes one file and holds the commit to the member who wrote it.
func writeFile(t *testing.T, session *session, path, content string) {
	t.Helper()
	var out struct {
		Path   string `json:"path"`
		Commit struct {
			Sha    string `json:"sha"`
			Author string `json:"author"`
		} `json:"commit"`
	}
	session.ok("fs_write", map[string]any{
		"path":    path,
		"content": content,
		"message": "Add " + filepath.Base(path) + " of the end to end Package",
	}, &out)
	if out.Commit.Author != session.user || len(out.Commit.Sha) != 40 {
		t.Fatalf("fs_write %s committed %+v, want a commit by %s", path, out.Commit, session.user)
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
	theCeiling(t, out.ID)

	// The Package's tool joins the session that started it, and answering it
	// is a podman exec into the container that is now running.
	names := s.admin.tools()
	sort.Strings(names)
	want := append(append([]string{}, builtIn...), echoTool)
	sort.Strings(want)
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("the surface after proc_run is\n%s\nwant the built ins and %s",
			strings.Join(names, " "), echoTool)
	}
	var echoed struct {
		Text string `json:"text"`
	}
	s.admin.ok(echoTool, map[string]any{"text": s.echoText}, &echoed)
	if echoed.Text != s.echoText {
		t.Fatalf("%s answered %q, want %q", echoTool, echoed.Text, s.echoText)
	}
}

// theCeiling reads the limit kitbashd wrote into the Process's own cgroup. The
// manifest asks for 256Mi and a member cannot write that file, which is what
// makes the limit an enforcement rather than a record.
//
// A host that cannot delegate runs its Processes unplaced, which is a host
// worth running the rest of this on and not one this job accepts by default:
// the runner has cgroup v2 and kitbashd runs as root there, so a missing
// ceiling is a regression. KITBASH_E2E_NO_CGROUPS is for the host that really
// has none, and it says so here rather than passing quietly.
func theCeiling(t *testing.T, id string) {
	const want = 256 * 1024 * 1024
	path := filepath.Join("/sys/fs/cgroup/kitbash", adminName(), id, "memory.max")
	out, err := exec.Command("sudo", "-n", "cat", path).Output()
	if err != nil {
		if os.Getenv(noCgroupsEnv) != "" {
			t.Logf("%s is set and this host placed no ceiling at %s, so its limits are recorded and not enforced: %v",
				noCgroupsEnv, path, err)
			return
		}
		t.Fatalf("no ceiling at %s: kitbashd placed this Process nowhere, and the daemon log says why. "+
			"Set %s if this host really has no cgroup v2 to delegate: %v", path, noCgroupsEnv, err)
	}
	got := strings.TrimSpace(string(out))
	if got != strconv.Itoa(want) {
		t.Fatalf("%s is %s, want %d for the manifest's 256Mi", path, got, want)
	}
	t.Logf("the ceiling of the Process is %s bytes", got)
}

// secondClient runs the TypeScript SDK against /mcp from inside the Process's
// own container. The Process token lives only there, so the client that bears
// it has to run there too.
func secondClient(t *testing.T, s *state) {
	local, err := filepath.Abs(envOr(clientEnv, "client"))
	if err != nil {
		t.Fatalf("resolving the second client: %v", err)
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

	// Nor does the member see what the admin is running. proc_list is the
	// caller's own Processes, and this member has none.
	var mine struct {
		Processes []struct {
			ID      string `json:"id"`
			Package string `json:"package"`
		} `json:"processes"`
	}
	s.member.ok("proc_list", map[string]any{}, &mine)
	for _, process := range mine.Processes {
		if process.ID == s.processID || strings.HasPrefix(process.Package, "/home/"+adminName()) {
			t.Fatalf("proc_list answered %s the admin's Process %+v", memberName(), process)
		}
	}
	if len(mine.Processes) != 0 {
		t.Fatalf("proc_list answered %s %d Processes, want none", memberName(), len(mine.Processes))
	}

	// The surface a member is served is the built ins alone: the admin's
	// Package publishes its tool to the admin.
	names := s.member.tools()
	sort.Strings(names)
	if strings.Join(names, " ") != strings.Join(builtIn, " ") {
		t.Fatalf("%s was served\n%s\nwant the built ins alone", memberName(), strings.Join(names, " "))
	}
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
		if record.Attributes.Tool != echoTool {
			t.Fatalf("a span answered for tool %s carries kitbash.tool %q",
				echoTool, record.Attributes.Tool)
		}
		if record.Name != echoTool {
			t.Fatalf("a span of %s is named %q", echoTool, record.Name)
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
	build := logs.Records[0]
	if build.Body == "" {
		t.Fatalf("the pkg_build log record carries no body: %+v", build)
	}
	if build.Attributes.Tool != "pkg_build" || build.Attributes.Package != s.pkgPath {
		t.Fatalf("the build log record carries tool %q and package %q, want pkg_build and %s",
			build.Attributes.Tool, build.Attributes.Package, s.pkgPath)
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

// buildTheRunKit builds and runs the fixture run kit. It is a Package like any
// other: what makes it a kit is the provides.kit block in its manifest, and
// what puts its tools within reach of dispatch is expose: mcp.
func buildTheRunKit(t *testing.T, s *state) {
	writeFixture(t, s.admin, runnerName, s.runnerPath)

	var built struct {
		Digest string `json:"digest"`
	}
	res := s.admin.callWithin(buildTimeout, "pkg_build", map[string]any{"path": s.runnerPath})
	res.mustSucceed(t, "pkg_build")
	if err := json.Unmarshal(res.Structured, &built); err != nil {
		t.Fatalf("decoding pkg_build: %v", err)
	}

	var out struct {
		State  string   `json:"state"`
		Runner string   `json:"runner"`
		Tools  []string `json:"tools"`
	}
	s.admin.ok("proc_run", map[string]any{"package": s.runnerPath}, &out)
	if out.State != "running" {
		t.Fatalf("proc_run answered %+v, want the run kit running", out)
	}
	if out.Runner != "" {
		t.Fatalf("the kit itself names the runner %q, want the built in runner to have started it", out.Runner)
	}
	names := append([]string{}, out.Tools...)
	sort.Strings(names)
	want := []string{callsTool, runTool, stopTool}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("the run kit published %v, want %v", names, want)
	}
}

// dispatchToTheRunKit is the whole point of the two manifest fields: a Package
// whose unit names a runner is started by that kit, and what the kit answers
// with is the Process kitbash registers and lists.
func dispatchToTheRunKit(t *testing.T, s *state) {
	// The manifest is written here rather than kept as a fixture because the
	// runner is an absolute path, and which home it is under is this host's
	// answer rather than the repository's.
	writeFile(t, s.admin, filepath.Join(s.elsewherePath, "kitbash.yaml"), fmt.Sprintf(`name: %s
description: A Package this host never starts. The fixture run kit answers for it, see PLAN.md section 3.
deploy:
  units:
    - type: container
      build: .
      runner: %s
      expose: none
`, elsewhereName, s.runnerPath))
	writeFile(t, s.admin, filepath.Join(s.elsewherePath, "Dockerfile"),
		"FROM "+baseImage(t)+"\nENTRYPOINT [\"/bin/true\"]\n")

	var built struct {
		Digest string `json:"digest"`
	}
	res := s.admin.callWithin(buildTimeout, "pkg_build", map[string]any{"path": s.elsewherePath})
	res.mustSucceed(t, "pkg_build")
	if err := json.Unmarshal(res.Structured, &built); err != nil {
		t.Fatalf("decoding pkg_build: %v", err)
	}
	s.elsewhereImage = built.Digest

	var out struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		State  string `json:"state"`
		Digest string `json:"digest"`
		Runner string `json:"runner"`
	}
	s.admin.ok("proc_run", map[string]any{"package": s.elsewherePath}, &out)
	if out.Runner != s.runnerPath || out.State != "running" {
		t.Fatalf("proc_run answered %+v, want a running Process owned by the kit at %s", out, s.runnerPath)
	}
	if out.Digest != s.elsewhereImage {
		t.Fatalf("proc_run registered the digest %s, want the one pkg_build answered, %s", out.Digest, s.elsewhereImage)
	}
	s.elsewhereID = out.ID

	// The kit was called with the four arguments of the hook, and the unit is
	// the one the manifest wrote.
	call := lastKitCall(t, s, "run")
	if call.Package != s.elsewherePath || call.Digest != s.elsewhereImage || call.Name != elsewhereName {
		t.Fatalf("the kit was called with %+v, want the Package, its digest and the Process name", call)
	}
	if call.Unit["type"] != "container" || call.Unit["runner"] != s.runnerPath {
		t.Fatalf("the kit was handed the unit %v, want the container unit as the manifest wrote it", call.Unit)
	}
	if call.ID != out.ID {
		t.Fatalf("the kit answered the id %s and proc_run registered %s", call.ID, out.ID)
	}

	// The Process is on proc_list with the kit that owns it, although nothing
	// of it runs on this host: the registration is what knows it exists.
	process, found := listed(t, s, out.ID)
	if !found {
		t.Fatalf("proc_list does not hold the Process %s the kit runs", out.ID)
	}
	if process.Runner != s.runnerPath || process.Package != s.elsewherePath {
		t.Fatalf("proc_list answered %+v, want the Package and the kit that runs it", process)
	}
	if out, err := runAs(t, adminName(), "podman", "ps", "--all", "--format", "{{.Names}}"); err == nil {
		if strings.Contains(out, "kitbash-"+elsewhereName) {
			t.Fatalf("a container of %s exists here: %s", elsewhereName, out)
		}
	}
}

// stopThroughTheRunKit is the other half of the ownership: kitbash cannot stop
// what it did not start, so proc_stop forwards to the kit's stop tool and the
// registration goes with the Process.
func stopThroughTheRunKit(t *testing.T, s *state) {
	var stopped struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	s.admin.ok("proc_stop", map[string]any{"id": s.elsewhereID}, &stopped)
	if stopped.State != "stopped" {
		t.Fatalf("proc_stop answered %+v, want the stopped state", stopped)
	}
	call := lastKitCall(t, s, "stop")
	if call.ID != s.elsewhereID {
		t.Fatalf("the kit was asked to stop %s, want %s", call.ID, s.elsewhereID)
	}
	if _, found := listed(t, s, s.elsewhereID); found {
		t.Fatalf("proc_list still holds the stopped Process %s", s.elsewhereID)
	}
}

// kitCall is one call the fixture kit recorded, as its calls tool hands it
// back.
type kitCall struct {
	Tool    string         `json:"tool"`
	ID      string         `json:"id"`
	Package string         `json:"package"`
	Digest  string         `json:"digest"`
	Name    string         `json:"name"`
	Unit    map[string]any `json:"unit"`
}

// lastKitCall is the newest call of one tool the fixture kit recorded, read
// through the kit's own surface tool.
func lastKitCall(t *testing.T, s *state, tool string) kitCall {
	t.Helper()
	var out struct {
		Calls []kitCall `json:"calls"`
	}
	s.admin.ok(callsTool, map[string]any{}, &out)
	for i := len(out.Calls) - 1; i >= 0; i-- {
		if out.Calls[i].Tool == tool {
			return out.Calls[i]
		}
	}
	t.Fatalf("the kit recorded no %s call: %+v", tool, out.Calls)
	return kitCall{}
}

// listedProcess is one entry of proc_list this job reads.
type listedProcess struct {
	ID      string `json:"id"`
	Package string `json:"package"`
	Digest  string `json:"digest"`
	State   string `json:"state"`
	Runner  string `json:"runner"`
}

// listed is one Process of the admin's proc_list, by id.
func listed(t *testing.T, s *state, id string) (listedProcess, bool) {
	t.Helper()
	var out struct {
		Processes []listedProcess `json:"processes"`
	}
	s.admin.ok("proc_list", map[string]any{}, &out)
	for _, process := range out.Processes {
		if process.ID == id {
			return process, true
		}
	}
	return listedProcess{}, false
}

// baseImage is the image the fixtures are built on, read from the echo
// Dockerfile so the job pulls one image and builds every Package on it.
func baseImage(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("fixtures", packageName, "Dockerfile"))
	if err != nil {
		t.Fatalf("reading the fixture Dockerfile: %v", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if after, found := strings.CutPrefix(line, "FROM "); found {
			return strings.TrimSpace(after)
		}
	}
	t.Fatal("the fixture Dockerfile carries no FROM line")
	return ""
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
