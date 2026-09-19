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

// builtIn is the whole built in surface, sorted and named rather than counted:
// 4 fs, 4 pkg, 4 proc, 3 secrets, 2 tel, 5 users and 3 approvals, the 25 the
// README's verify step counts and the families of spec/mcp-surface.yaml one for
// one. A tool that is renamed is a client that breaks, so the names are the
// assertion.
var builtIn = []string{
	"approvals_approve", "approvals_list", "approvals_reject",
	"fs_history", "fs_list", "fs_read", "fs_write",
	"pkg_build", "pkg_import", "pkg_inspect", "pkg_list",
	"proc_list", "proc_logs", "proc_run", "proc_stop",
	"secrets_list", "secrets_remove", "secrets_set",
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

// fixtures are the three files every fixture Package carries. writeFixture
// walks the folder instead, because a Package is whatever is in it; this list
// is what the two jobs that write a Package one file at a time iterate, the
// mount fixtures and the upgrade half, whose writes are made by the previous
// release.
var fixtures = []string{"kitbash.yaml", "Dockerfile", "server.js"}

// The import-cli kit and the CLI Package it drafts, which is the acceptance
// sentence of M7 in PLAN.md 5.3 driven end to end: the kit is a seeded folder
// of /org, the source names an Alpine package and nothing else, and every tool
// below is one the kit wrote.
//
// The source carries no version. Issue #107 names jq 1.7.1, and the Alpine
// branch node:22-alpine points at carries 1.8.x, so a pin would fail the build
// for a reason that has nothing to do with the import; the unpinned form
// installs whatever the branch has. A version that is wanted is written
// jq@1.8.2-r0 for one apk release, or jq@1.8.2 for the ~= form that resolves
// to the newest release of that version.
const (
	importKitPath = "/org/import-cli"
	cliName       = "jq"
	cliSource     = "cli:apk:jq"
	importTool    = "import-cli_import"
	refineTool    = "import-cli_refine"
	cliRunTool    = "jq_run"
	cliProbeTool  = "jq_probe"
	cliTool       = "jq_jq"
	cliCopyTool   = "jq_copy"
)

// The mounts the refined Package is given, PLAN.md 2.3 and issue #122: one
// folder of its owner's home read only, holding the document a tool reads by
// path, and one read write, where a tool leaves a file its owner then reads
// with fs_read. The owner is the admin who imported the Package, because a
// mount source is a folder of the Process owner's own home.
const (
	cliDocsName  = "docs"
	cliOutName   = "out"
	cliDocsMount = "/files/docs"
	cliOutMount  = "/files/out"
	cliDocFile   = "doc.json"
	cliCopyFile  = "copy.json"
)

// The document the refined tool filters and what jq must answer for it. The
// filter and the document go in as a schema validated tool call, not as a
// command line, which is the whole point of the import.
const (
	cliDocument = `{"items":[{"name":"kitbash","stars":3},{"name":"jq","stars":1}]}`
	cliFilter   = ".items | map(.name)"
	cliStdout   = "[\"kitbash\",\"jq\"]\n"
)

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
	// The CLI Package import-cli drafted, what its probe reported about the
	// binary, and the image and the Process of the draft, which the
	// refinement has to replace.
	cliPath      string
	probe        probeOutput
	cliDigest    string
	cliProcessID string
	// The two folders of the admin's home the refined Package mounts, the
	// first read only and the second read write.
	cliDocsPath string
	cliOutPath  string
	// The two Packages of the Files mounts, the member's folders they mount,
	// and the text the member wrote for the read only one to read back.
	readerPath string
	writerPath string
	notesPath  string
	outPath    string
	noteText   string
	// The value the member sets with secrets_set, made fresh at each run so a
	// stale one cannot pass the assertions that read it back.
	secret string
}

// probeOutput is what a drafted Package's probe tool reports about its binary,
// and what the kit's refine tool is called with.
type probeOutput struct {
	Help    string `json:"help"`
	Version string `json:"version"`
	Man     string `json:"man"`
}

// cliResult is what a generated Package answers a tool call with. The adapter
// returns it as one text block rather than as structured content, and the
// bridge passes a Package's own content through, so it is decoded from the
// text here.
type cliResult struct {
	ExitCode int       `json:"exitCode"`
	Stdout   string    `json:"stdout"`
	Stderr   string    `json:"stderr"`
	Files    []cliFile `json:"files"`
}

// cliFile is one entry of a result's files. An output named as a plain name
// carries its bytes; one named as a path under a read write mount carries the
// path it was left at and nothing else, because the adapter never reads a file
// back out of a mount.
type cliFile struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	ContentBase64 string `json:"contentBase64"`
}

// importedFile is one file the kit answers with, which is what pkg_import
// writes for the draft and what fs_write writes for the refinement.
type importedFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// TestSurface is the job: one admin and one member driving the whole surface
// on a real host, in the order an organization uses it.
func TestSurface(t *testing.T) {
	requireHost(t)
	s := &state{
		readerPath: filepath.Join("/home", memberName(), readerName),
		writerPath: filepath.Join("/home", memberName(), writerName),
		notesPath:  filepath.Join("/home", memberName(), notesName),
		outPath:    filepath.Join("/home", memberName(), outName),
		noteText:   fmt.Sprintf("# notes\n\nWritten by a member at %s.\n", time.Now().UTC().Format(time.RFC3339)),

		pkgPath:       filepath.Join("/home", adminName(), packageName),
		runnerPath:    filepath.Join("/home", adminName(), runnerName),
		elsewherePath: filepath.Join("/home", adminName(), elsewhereName),
		cliPath:       filepath.Join("/home", adminName(), cliName),
		cliDocsPath:   filepath.Join("/home", adminName(), cliDocsName),
		cliOutPath:    filepath.Join("/home", adminName(), cliOutName),
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
		{"the import-cli kit is built and run from /org", runTheImportKit},
		{"pkg_import drafts a Package around an Alpine CLI", importTheCLI},
		{"the draft's probe reports what the binary says about itself", probeTheDraft},
		{"the admin writes the folders the refined Package mounts", writeTheCLIFolders},
		{"refine turns that into a tool with a schema and two mounts", refineTheDraft},
		{"the refined tool filters a document and names a missing file", callTheRefinedTool},
		{"the refined tool reads a document through the read only mount", readThroughTheCLIMount},
		{"a copy tool leaves its output in the read write mount", writeThroughTheCLIMount},
		{"a member writes the files a Process will read", memberWritesTheFiles},
		{"a Package mounting that folder read only is built and run", runTheReader},
		{"its tool reads the file the member wrote", readThroughTheMount},
		{"a write into the read only mount is refused by the kernel", writeIntoTheReadOnlyMount},
		{"a second Package mounted read write writes a file", runTheWriter},
		{"fs_read answers with what the Process wrote", readWhatTheProcessWrote},
		{"an illegal mount is refused at proc_run", illegalMounts},
		{"the member sets a secret and the listing shows the name", theMemberSetsASecret},
		{"a Package declaring that name runs and its tool reads the variable", aProcessReadsTheSecret},
		{"the same Package under a member who has not set it is refused", aSecondMemberIsRefused},
		{"the span of secrets_set carries no value", theSpanCarriesNoValue},
		{"secrets_remove takes the name away and the next run is refused", theSecretIsRemoved},
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
// every file of it in one fs_write and therefore in one commit. The echo
// fixture carries a public/ folder with no manifest of its own, so this is the
// M10 write shape end to end: the folders inside a Package are described by
// the Package, and its files go in with it, see PLAN.md section 2.1.
func writeFixture(t *testing.T, session *session, fixture, target string) {
	t.Helper()
	root := filepath.Join("fixtures", fixture)
	var files []map[string]any
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, map[string]any{
			"path":    filepath.Join(target, rel),
			"content": string(body),
		})
		return nil
	}); err != nil {
		t.Fatalf("reading the %s fixture: %v", fixture, err)
	}
	if len(files) < 3 {
		t.Fatalf("the %s fixture holds %d files, want the Package", fixture, len(files))
	}

	var out struct {
		Paths  []string `json:"paths"`
		Commit struct {
			Sha    string `json:"sha"`
			Author string `json:"author"`
		} `json:"commit"`
	}
	session.ok("fs_write", map[string]any{
		"files":   files,
		"message": "Add the " + fixture + " Package",
	}, &out)
	if len(out.Paths) != len(files) {
		t.Fatalf("fs_write wrote %v, want every file of the fixture", out.Paths)
	}
	if out.Commit.Author != session.user || len(out.Commit.Sha) != 40 {
		t.Fatalf("fs_write committed %+v, want one commit by %s", out.Commit, session.user)
	}
	// Every file is in that one commit, including the one in the folder that
	// carries no manifest.
	for _, file := range files {
		path := file["path"].(string)
		var history struct {
			Commits []struct {
				Sha string `json:"sha"`
			} `json:"commits"`
		}
		session.ok("fs_history", map[string]any{"path": path}, &history)
		if len(history.Commits) != 1 || history.Commits[0].Sha != out.Commit.Sha {
			t.Fatalf("the history of %s is %+v, want the one commit the list made", path, history.Commits)
		}
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
	process, found := listed(t, s, s.elsewherePath, out.ID)
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
	if _, found := listed(t, s, s.elsewherePath, s.elsewhereID); found {
		t.Fatalf("proc_list still holds the stopped Process %s", s.elsewhereID)
	}
}

// runTheImportKit builds and runs the import-cli kit out of /org, where
// install.sh seeded it. It is a Package like any other: its manifest declares
// provides.kit: [import] and an import tool whose source pattern is what
// pkg_import routes on, and running it is what puts it within reach.
func runTheImportKit(t *testing.T, s *state) {
	var built struct {
		Digest string `json:"digest"`
	}
	res := s.admin.callWithin(buildTimeout, "pkg_build", map[string]any{"path": importKitPath})
	res.mustSucceed(t, "pkg_build")
	if err := json.Unmarshal(res.Structured, &built); err != nil {
		t.Fatalf("decoding pkg_build: %v", err)
	}

	var out struct {
		State string   `json:"state"`
		Tools []string `json:"tools"`
	}
	// The digest is passed rather than left to default, here and at every
	// proc_run below: a Process is converged onto one image, and naming it is
	// how the caller knows which build is answering.
	s.admin.ok("proc_run", map[string]any{"package": importKitPath, "digest": built.Digest}, &out)
	if out.State != "running" {
		t.Fatalf("proc_run answered %+v, want the import kit running", out)
	}
	names := append([]string{}, out.Tools...)
	sort.Strings(names)
	want := []string{importTool, refineTool}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("the import kit published %v, want %v", names, want)
	}
}

// importTheCLI is the first half of the two step import: pkg_import picks the
// kit whose source pattern accepts cli:apk:jq, writes the draft as one commit,
// and the draft builds and runs knowing nothing about the binary beyond its
// name.
func importTheCLI(t *testing.T, s *state) {
	var imported struct {
		Path   string `json:"path"`
		Commit struct {
			Sha    string `json:"sha"`
			Author string `json:"author"`
		} `json:"commit"`
	}
	s.admin.ok("pkg_import", map[string]any{"path": s.cliPath, "source": cliSource}, &imported)
	if imported.Commit.Author != adminName() || len(imported.Commit.Sha) != 40 {
		t.Fatalf("pkg_import committed %+v, want a commit by %s", imported.Commit, adminName())
	}

	// The source named no version, so the Dockerfile asks the Alpine branch
	// for whatever it carries. This is read back rather than assumed: what the
	// image installs is the one thing in the draft that the kit decided alone.
	dockerfile := s.admin.call("fs_read", map[string]any{"path": filepath.Join(s.cliPath, "Dockerfile")})
	dockerfile.mustSucceed(t, "fs_read")
	if !strings.Contains(dockerfile.text(), "apk add --no-cache "+cliName+"\n") {
		t.Fatalf("the drafted Dockerfile does not install %s unpinned:\n%s", cliName, truncate(dockerfile.text()))
	}

	var built struct {
		Digest string `json:"digest"`
	}
	res := s.admin.callWithin(buildTimeout, "pkg_build", map[string]any{"path": s.cliPath})
	res.mustSucceed(t, "pkg_build")
	if err := json.Unmarshal(res.Structured, &built); err != nil {
		t.Fatalf("decoding pkg_build: %v", err)
	}
	s.cliDigest = built.Digest

	var out struct {
		ID     string   `json:"id"`
		State  string   `json:"state"`
		Digest string   `json:"digest"`
		Tools  []string `json:"tools"`
	}
	s.admin.ok("proc_run", map[string]any{"package": s.cliPath, "digest": built.Digest}, &out)
	if out.State != "running" || out.Digest != built.Digest {
		t.Fatalf("proc_run answered %+v, want the drafted Package running at %s", out, built.Digest)
	}
	s.cliProcessID = out.ID
	names := append([]string{}, out.Tools...)
	sort.Strings(names)
	want := []string{cliProbeTool, cliRunTool}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("the drafted Package published %v, want %v", names, want)
	}
}

// probeTheDraft reads the binary through the Package that wraps it. What the
// binary says about itself is the input the refinement is made from, and it
// comes from the running Process rather than from a fixture in this
// repository.
func probeTheDraft(t *testing.T, s *state) {
	res := s.admin.call(cliProbeTool, map[string]any{})
	res.mustSucceed(t, cliProbeTool)
	if err := json.Unmarshal([]byte(res.text()), &s.probe); err != nil {
		t.Fatalf("decoding what %s reported: %v\n%s", cliProbeTool, err, truncate(res.text()))
	}
	if !strings.Contains(s.probe.Help, "Usage:") || !strings.Contains(s.probe.Help, cliName) {
		t.Fatalf("%s reported a help text that is not %s's:\n%s", cliProbeTool, cliName, truncate(s.probe.Help))
	}
	if !strings.Contains(s.probe.Version, cliName+"-") {
		t.Fatalf("%s reported the version %q, want the one the binary prints", cliProbeTool, truncate(s.probe.Version))
	}
	t.Logf("%s reported %s, %d bytes of help and %d of man",
		cliProbeTool, strings.TrimSpace(s.probe.Version), len(s.probe.Help), len(s.probe.Man))
}

// writeTheCLIFolders is what the mounts of the refinement point at: a folder
// holding the document a tool reads by path, and an empty one for a tool to
// write into. Both carry a kitbash.yaml, because a folder the surface cannot
// see is a folder a Process cannot be given, PLAN.md 2.3.
//
// They are the admin's folders and not the member's, which is the ownership
// rule and not a shortcut: a mount source has to be under the home of whoever
// owns the Process, and the Process here is the one the admin imported, built
// and ran. A member's folder mounted into an admin's Process is not-permitted
// at proc_run, which e2e/mounts_test.go proves from the member's side.
func writeTheCLIFolders(t *testing.T, s *state) {
	writeFile(t, s.admin, filepath.Join(s.cliDocsPath, "kitbash.yaml"),
		folderManifest(cliDocsName, "Documents the imported CLI reads through a read only mount."))
	writeFile(t, s.admin, filepath.Join(s.cliDocsPath, cliDocFile), cliDocument)
	writeFile(t, s.admin, filepath.Join(s.cliOutPath, "kitbash.yaml"),
		folderManifest(cliOutName, "Where the imported CLI leaves an output through a read write mount."))
}

// refineTheDraft is the second half: the kit is called with what probe
// reported, the files it answers with are written over the folder, and the
// Package is built and run again. The manifest that ends up running was
// drafted by the kit and refined from the Package's own probe, which is the
// acceptance sentence of issue #107.
//
// It is also where the two mounts of issue #122 are declared. They are an
// argument of refine rather than an edit afterwards, so what runs is the
// manifest the kit wrote, mounts and all.
func refineTheDraft(t *testing.T, s *state) {
	var refined struct {
		Files []importedFile `json:"files"`
	}
	s.admin.ok(refineTool, map[string]any{
		"source":  cliSource,
		"help":    s.probe.Help,
		"version": s.probe.Version,
		"man":     s.probe.Man,
		"mounts": []map[string]any{
			{"source": s.cliDocsPath, "target": cliDocsMount, "mode": "ro"},
			{"source": s.cliOutPath, "target": cliOutMount, "mode": "rw"},
		},
	}, &refined)
	if len(refined.Files) == 0 {
		t.Fatalf("%s answered no files", refineTool)
	}
	for _, file := range refined.Files {
		writeFile(t, s.admin, filepath.Join(s.cliPath, file.Path), file.Content)
	}
	addTheCopyTool(t, s, refined.Files)

	var built struct {
		Digest string `json:"digest"`
	}
	res := s.admin.callWithin(buildTimeout, "pkg_build", map[string]any{"path": s.cliPath})
	res.mustSucceed(t, "pkg_build")
	if err := json.Unmarshal(res.Structured, &built); err != nil {
		t.Fatalf("decoding pkg_build: %v", err)
	}
	// The refinement rewrote tools.json, which the image carries, so this is a
	// different image from the draft's. A Package that built to the same
	// digest would leave the draft's container answering the refined
	// manifest's tools.
	if built.Digest == s.cliDigest {
		t.Fatalf("the refined Package built to %s, the same image as the draft", built.Digest)
	}

	var out struct {
		ID     string   `json:"id"`
		State  string   `json:"state"`
		Digest string   `json:"digest"`
		Tools  []string `json:"tools"`
		Mounts []struct {
			Source string `json:"source"`
			Target string `json:"target"`
			Mode   string `json:"mode"`
		} `json:"mounts"`
	}
	s.admin.ok("proc_run", map[string]any{"package": s.cliPath, "digest": built.Digest}, &out)
	if out.State != "running" || out.Digest != built.Digest {
		t.Fatalf("proc_run answered %+v, want the refined Package running at %s", out, built.Digest)
	}
	// The mounts the kit wrote into the manifest are the ones kitbashd resolved,
	// which is what makes the paths below reach anything at all.
	if len(out.Mounts) != 2 ||
		out.Mounts[0].Source != s.cliDocsPath || out.Mounts[0].Target != cliDocsMount || out.Mounts[0].Mode != "ro" ||
		out.Mounts[1].Source != s.cliOutPath || out.Mounts[1].Target != cliOutMount || out.Mounts[1].Mode != "rw" {
		t.Fatalf("proc_run reported the mounts %+v, want %s read only and %s read write",
			out.Mounts, cliDocsMount, cliOutMount)
	}
	if out.ID == s.cliProcessID {
		t.Fatalf("proc_run answered the draft's Process %s, want the refined image to have replaced it", out.ID)
	}
	names := append([]string{}, out.Tools...)
	sort.Strings(names)
	want := []string{cliProbeTool, cliRunTool, cliTool, cliCopyTool}
	sort.Strings(want)
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("the refined Package published %v, want %v", names, want)
	}
}

// callTheRefinedTool is what all of it was for: one schema validated call that
// filters a document, and one that names a file nobody sent.
func callTheRefinedTool(t *testing.T, s *state) {
	res := s.admin.call(cliTool, map[string]any{
		"filter":         cliFilter,
		"stdin":          cliDocument,
		"compact_output": true,
	})
	res.mustSucceed(t, cliTool)
	var answer cliResult
	if err := json.Unmarshal([]byte(res.text()), &answer); err != nil {
		t.Fatalf("decoding what %s answered: %v\n%s", cliTool, err, truncate(res.text()))
	}
	if answer.ExitCode != 0 || answer.Stdout != cliStdout {
		t.Fatalf("%s answered exit code %d and %q, want 0 and %q (stderr: %s)",
			cliTool, answer.ExitCode, answer.Stdout, cliStdout, truncate(answer.Stderr))
	}
	t.Logf("%s filtered the document to %s", cliTool, strings.TrimSpace(answer.Stdout))

	// A Process sees none of the caller's Files, so a file argument names an
	// entry of the call's own files. One that was never sent is the Package's
	// own not-found, and the bridge passes a Package's problem through in the
	// surface's language, see PLAN.md 5.5 on the bind mount that is not
	// decided.
	missing := s.admin.call(cliTool, map[string]any{"filter": ".", "file": []string{"absent.json"}})
	problem := missing.mustProblem(t, cliTool, "not-found")
	if !strings.Contains(problem.Detail, "absent.json") {
		t.Fatalf("%s answered the problem %+v, want one naming absent.json", cliTool, problem)
	}
}

// copyToolYAML is the tool this job adds to the refined Package by hand. jq
// takes its input from a file and writes to standard output, so nothing the kit
// generates from jq's help text has an output argument, and the output half of
// a mount needs one. cp is in the base image, its two arguments are a file each
// and the second is what it writes, which is exactly the shape the adapter
// treats as an output.
//
// It is written here rather than in a fixture folder because it belongs to the
// Package the kit wrote: a fixture would be a second Package to build.
const copyToolYAML = `    - name: copy
      description: >-
        Copies a file, both paths given under this Package's mounts. Added by the
        end to end job, because jq has no output argument to prove the output
        half of a mount with.
      input:
        type: object
        additionalProperties: false
        required: [src, dst]
        properties:
          src:
            type: string
            format: kitbash-file
            description: >-
              The file to copy, as an absolute path under a folder this Package
              mounts, or the name of an entry of files.
          dst:
            type: string
            format: kitbash-file
            description: >-
              Where to leave the copy, as an absolute path under a folder this
              Package mounts read write.
      output:
        type: object
        required: [exitCode, stdout, stderr]
        properties:
          exitCode:
            type: integer
            description: The status cp exited with.
          stdout:
            type: string
            description: What cp wrote to standard output, which is nothing.
          stderr:
            type: string
            description: What cp wrote to standard error.
          files:
            type: array
            description: The output, named and located, carrying no contents.
            items:
              type: object
              required: [name]
              properties:
                name:
                  type: string
                path:
                  type: string
`

// addTheCopyTool writes the copy tool into the refined Package, in the manifest
// the surface validates against and in the tools.json the adapter builds argv
// from. Both are the kit's own files, read back from what refine answered and
// written again as one more commit each, before the build that makes the image.
func addTheCopyTool(t *testing.T, s *state, files []importedFile) {
	t.Helper()
	var manifest, tools string
	for _, file := range files {
		switch file.Path {
		case "kitbash.yaml":
			manifest = file.Content
		case "tools.json":
			tools = file.Content
		}
	}
	if manifest == "" || tools == "" {
		t.Fatalf("%s answered no manifest or no tools.json", refineTool)
	}

	// The tools list ends where the deploy block begins, which is the one place
	// in a generated manifest another tool can be added without reindenting it.
	// That line is written by manifest() in deploy/org/import-cli/generate.js,
	// which is what to read if this insertion ever stops finding its place.
	if !strings.Contains(manifest, "\ndeploy:\n") {
		t.Fatalf("the refined manifest has no deploy block:\n%s", truncate(manifest))
	}
	manifest = strings.Replace(manifest, "\ndeploy:\n", "\n"+copyToolYAML+"deploy:\n", 1)
	writeFile(t, s.admin, filepath.Join(s.cliPath, "kitbash.yaml"), manifest)

	var document map[string]any
	if err := json.Unmarshal([]byte(tools), &document); err != nil {
		t.Fatalf("decoding the refined tools.json: %v", err)
	}
	entries, ok := document["tools"].(map[string]any)
	if !ok {
		t.Fatalf("the refined tools.json carries no tools object:\n%s", truncate(tools))
	}
	// The same schema the manifest declares, because the adapter answers
	// tools/list from this file and never reads the manifest.
	inputSchema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"src", "dst"},
		"properties": map[string]any{
			"src": map[string]any{"type": "string", "format": "kitbash-file"},
			"dst": map[string]any{"type": "string", "format": "kitbash-file"},
		},
	}
	entries["copy"] = map[string]any{
		"argv":        []string{"cp"},
		"options":     map[string]any{},
		"positionals": []string{"src", "dst"},
		"stdin":       nil,
		"outputs":     []string{"dst"},
		"inputSchema": inputSchema,
	}
	patched, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatalf("encoding the patched tools.json: %v", err)
	}
	writeFile(t, s.admin, filepath.Join(s.cliPath, "tools.json"), string(patched)+"\n")
}

// readThroughTheCLIMount is the input half of issue #122: the document is not
// in the call at all. It was written with fs_write, the Package mounts the
// folder read only, and the tool is given the path the container sees it at.
func readThroughTheCLIMount(t *testing.T, s *state) {
	res := s.admin.call(cliTool, map[string]any{
		"filter":         cliFilter,
		"file":           []string{cliDocsMount + "/" + cliDocFile},
		"compact_output": true,
	})
	res.mustSucceed(t, cliTool)
	var answer cliResult
	if err := json.Unmarshal([]byte(res.text()), &answer); err != nil {
		t.Fatalf("decoding what %s answered: %v\n%s", cliTool, err, truncate(res.text()))
	}
	if answer.ExitCode != 0 || answer.Stdout != cliStdout {
		t.Fatalf("%s read %s and answered exit code %d and %q, want 0 and %q (stderr: %s)",
			cliTool, cliDocFile, answer.ExitCode, answer.Stdout, cliStdout, truncate(answer.Stderr))
	}

	// A path outside every mount is the Package's own refusal, which is what
	// keeps a mounted Package from being a way to read the whole host. It
	// reaches the caller as an invalid-path and not as a bad-request carrying
	// JSON, because that class is one a Package may claim, see the passable
	// list in internal/bridge/passthrough.go and package_errors in
	// spec/mcp-surface.yaml.
	outside := s.admin.call(cliTool, map[string]any{"filter": ".", "file": []string{"/etc/hosts"}})
	refusal := outside.mustProblem(t, cliTool, "invalid-path")
	if !strings.Contains(refusal.Detail, "outside every folder this unit mounts") {
		t.Fatalf("%s refused /etc/hosts with %q, want the adapter's own detail", cliTool, truncate(refusal.Detail))
	}
	if !strings.Contains(refusal.Fix, cliDocsMount) {
		t.Fatalf("%s answered the fix %q, want one naming the mounts it has", cliTool, truncate(refusal.Fix))
	}
}

// writeThroughTheCLIMount is the output half: the tool writes its file into the
// read write mount, the result says where it is and carries none of it, and the
// owner reads it with fs_read as an ordinary file of Files.
func writeThroughTheCLIMount(t *testing.T, s *state) {
	res := s.admin.call(cliCopyTool, map[string]any{
		"src": cliDocsMount + "/" + cliDocFile,
		"dst": cliOutMount + "/" + cliCopyFile,
	})
	res.mustSucceed(t, cliCopyTool)
	var answer cliResult
	if err := json.Unmarshal([]byte(res.text()), &answer); err != nil {
		t.Fatalf("decoding what %s answered: %v\n%s", cliCopyTool, err, truncate(res.text()))
	}
	if answer.ExitCode != 0 {
		t.Fatalf("%s answered exit code %d (stderr: %s)", cliCopyTool, answer.ExitCode, truncate(answer.Stderr))
	}
	if len(answer.Files) != 1 || answer.Files[0].Name != cliCopyFile ||
		answer.Files[0].Path != cliOutMount+"/"+cliCopyFile || answer.Files[0].ContentBase64 != "" {
		t.Fatalf("%s answered the files %+v, want %s at %s with no contents",
			cliCopyTool, answer.Files, cliCopyFile, cliOutMount+"/"+cliCopyFile)
	}

	// The file is in the member's own home, where the mount source pointed, and
	// it has no commit: a Process writing through a mount commits nothing, which
	// is the known hole of PLAN.md 2.1.
	read := s.admin.call("fs_read", map[string]any{"path": filepath.Join(s.cliOutPath, cliCopyFile)})
	read.mustSucceed(t, "fs_read")
	if len(read.Content) == 0 || read.Content[0].Text != cliDocument {
		t.Fatalf("fs_read answered %q, want the document the tool copied, %q",
			truncate(read.text()), cliDocument)
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

// listed is one Process of the admin's proc_list, by id. It asks about the
// Package, because that is the question proc_list answers in full: without one
// the listing is a line per Process and carries neither the digest nor the
// kit that owns it.
func listed(t *testing.T, s *state, pkgPath, id string) (listedProcess, bool) {
	t.Helper()
	var out struct {
		Processes []listedProcess `json:"processes"`
	}
	s.admin.ok("proc_list", map[string]any{"package": pkgPath}, &out)
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
