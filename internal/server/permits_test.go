package server_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/bridge"
	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/pkg"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/proc"
	"github.com/zyx1121/kitbash/internal/server"
	"github.com/zyx1121/kitbash/internal/telemetry"
	"github.com/zyx1121/kitbash/internal/telemetry/teltest"
)

// narrowed is the whole surface as a Process reaches it: one Package running
// through the bridge, and a permits block over both the built in tools and
// the Package's own.
type narrowed struct {
	*whole
	session *mcp.ClientSession
}

// serveNarrowed is connectWhole with a permits block, and with the ffmpeg
// Package already on the surface: what the bridge published before the block
// was read has to be filtered the same way the built ins are.
//
// The block is built from the fixture because a prefix is a real path: the
// roots of a test are its temporary directory, and a test that declared /org
// would be narrowing a tree that is not there.
func serveNarrowed(t *testing.T, permitsFor func(w *whole) *manifest.Permits) *narrowed {
	t.Helper()
	ctx := context.Background()
	w := newWhole(t)
	permits := permitsFor(w)
	if err := os.WriteFile(filepath.Join(w.folder, manifest.FileName), []byte(packageManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	// Every Process is run by kitbashd, so the surface is served against a
	// fake one whose starts land in this runtime, see PLAN.md section 2.3.
	daemon, err := teltest.Start()
	if err != nil {
		t.Fatalf("teltest.Start: %v", err)
	}
	t.Cleanup(daemon.Close)
	daemon.MirrorRuns(w.runner)
	processes := proc.New(w.files, w.runner, telemetry.NewClient(daemon.Socket))
	tools := bridge.New(w.files, processes, w.runner)
	packageServer := mcp.NewServer(&mcp.Implementation{Name: "ffmpeg", Version: "1"}, nil)
	packageServer.AddTool(&mcp.Tool{
		Name:        "transcode",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, transcode)
	tools.SetTransport(func(ctx context.Context, _ *proc.Process) (mcp.Transport, error) {
		clientSide, serverSide := mcp.NewInMemoryTransports()
		if _, err := packageServer.Connect(ctx, serverSide, nil); err != nil {
			return nil, err
		}
		return clientSide, nil
	})

	srv := server.New("test", server.Deps{
		Files:     w.files,
		Packages:  pkg.New(w.files, w.runner, tools),
		Processes: processes,
		Bridge:    tools,
		Permits:   permits,
	})
	if _, _, err := w.runner.Build(ctx, w.folder, w.folder+"/Containerfile",
		"localhost/kitbash/ffmpeg:test", map[string]string{
			podman.LabelPath: w.folder, podman.LabelName: "ffmpeg", podman.LabelUser: "tester",
		}); err != nil {
		t.Fatalf("building: %v", err)
	}
	process, prob := processes.Run(ctx, w.folder, "", "")
	if prob != nil {
		t.Fatalf("proc.Run: %s", prob.Detail)
	}
	if prob := tools.Add(ctx, process); prob != nil {
		t.Fatalf("bridge.Add: %s", prob.Detail)
	}

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "process", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		session.Close()
		serverSession.Wait()
		tools.Close()
	})
	return &narrowed{whole: w, session: session}
}

// listed is the tool names one session publishes, sorted.
func listed(t *testing.T, s *mcp.ClientSession) []string {
	t.Helper()
	list, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

// refused reads the problem of a call the permits block stopped and holds it
// to the shape a refusal has to have: not-permitted, 403, and a fix that names
// the manifest rather than this session.
func refused(t *testing.T, res *mcp.CallToolResult, wantFix string) problem.Problem {
	t.Helper()
	p := problemOf(t, res)
	if p.Slug() != problem.SlugNotPermitted {
		t.Errorf("problem is %s, want not-permitted", p.Slug())
	}
	if p.Status != 403 {
		t.Errorf("status is %d, want 403", p.Status)
	}
	if p.Fix != wantFix {
		t.Errorf("fix is %q, want %q", p.Fix, wantFix)
	}
	return p
}

// TestPermitsPublishOnlyPermittedTools is the listing half of the narrowing:
// built in tools and the tools of the owner's Processes are filtered by the
// same globs, see PLAN.md section 2.3.
func TestPermitsPublishOnlyPermittedTools(t *testing.T) {
	n := serveNarrowed(t, func(w *whole) *manifest.Permits {
		return &manifest.Permits{
			Tools: []string{"fs_read", "fs_l*", "ffmpeg_*"},
			Paths: []string{w.root},
		}
	})

	want := []string{"ffmpeg_transcode", "fs_list", "fs_read"}
	if got := listed(t, n.session); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tools/list is %v, want exactly %v", got, want)
	}
}

// TestPermitsPackagesAdmitsTheOtherKitsAndNoBuiltIn is the reserved word on a
// live surface: the bridge says which names are a running Process's, so a kit
// that composes other kits declares packages and gets their tools and not one
// built in.
func TestPermitsPackagesAdmitsTheOtherKitsAndNoBuiltIn(t *testing.T) {
	n := serveNarrowed(t, func(w *whole) *manifest.Permits {
		return &manifest.Permits{
			Tools: []string{"fs_read", "fs_list", manifest.PermitPackages},
			Paths: []string{w.root},
		}
	})

	want := []string{"ffmpeg_transcode", "fs_list", "fs_read"}
	if got := listed(t, n.session); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tools/list is %v, want exactly %v", got, want)
	}
	ok(t, call(t, n.session, "ffmpeg_transcode", map[string]any{}), "ffmpeg_transcode")

	// Every built in the word looks like it might cover is refused, including
	// the ones a *_* glob would have let in.
	for _, name := range []string{"fs_write", "users_remove", "pkg_build", "proc_run"} {
		refused(t, call(t, n.session, name, map[string]any{}), server.PermitsFix)
	}
}

// TestPermitsRefuseANonPermittedCall is the answer to a tool the listing left
// out. The SDK would answer that it knows no such tool, which is true of the
// Process and not of the surface, so the guard answers first and says what to
// change.
func TestPermitsRefuseANonPermittedCall(t *testing.T) {
	n := serveNarrowed(t, func(w *whole) *manifest.Permits {
		return &manifest.Permits{Tools: []string{"fs_read"}, Paths: []string{w.root}}
	})

	res := call(t, n.session, "fs_write", map[string]any{
		"path": filepath.Join(n.folder, "notes.md"), "content": "hello", "message": "write",
	})
	p := refused(t, res, server.PermitsFix)
	if !strings.Contains(p.Detail, "fs_write") {
		t.Errorf("detail is %q, which does not name the tool", p.Detail)
	}
	if p.Instance != "fs_write" {
		t.Errorf("instance is %q, want the tool that was called", p.Instance)
	}

	// A Package tool the block does not name is refused the same way: the
	// bridge published it, and the Process may still not call it.
	p = refused(t, call(t, n.session, "ffmpeg_transcode", map[string]any{}), server.PermitsFix)
	if !strings.Contains(p.Detail, "ffmpeg_transcode") {
		t.Errorf("detail is %q, which does not name the Package tool", p.Detail)
	}
}

// TestPermitsRefuseAPathOutsideThePrefixes is the path half: a permitted tool
// on a path the Package never declared is refused before the handler runs, so
// nothing is read, written or built on the way to the answer.
func TestPermitsRefuseAPathOutsideThePrefixes(t *testing.T) {
	n := serveNarrowed(t, func(w *whole) *manifest.Permits {
		return &manifest.Permits{
			Tools: []string{"fs_read", "fs_list", "pkg_inspect"},
			Paths: []string{w.folder},
		}
	})

	outside := filepath.Join(n.root, "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, manifest.FileName), []byte(`name: elsewhere
description: A folder the Process may see the name of and nothing else.
`), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(secret, []byte("not for the Process\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := refused(t, call(t, n.session, "fs_read", map[string]any{"path": secret}), server.PermitsPathFix)
	if p.Instance != secret {
		t.Errorf("instance is %q, want the path that was refused", p.Instance)
	}
	refused(t, call(t, n.session, "pkg_inspect", map[string]any{"path": outside}), server.PermitsPathFix)

	// The same tool inside the declared prefix answers normally, so the block
	// narrows the surface rather than closing it.
	ok(t, call(t, n.session, "fs_read", map[string]any{
		"path": filepath.Join(n.folder, manifest.FileName),
	}), "fs_read")
}

// TestPermitsWithoutAPathRefuseEveryToolThatTakesOne: a Package that says
// which tools it wants and not where may call none of them on a path, and the
// tools that name nothing are unaffected.
func TestPermitsWithoutAPathRefuseEveryToolThatTakesOne(t *testing.T) {
	n := serveNarrowed(t, func(*whole) *manifest.Permits {
		return &manifest.Permits{Tools: []string{"*"}}
	})

	for _, name := range []string{"fs_list", "fs_read", "pkg_build", "pkg_inspect", "proc_run"} {
		refused(t, call(t, n.session, name, map[string]any{}), server.PermitsPathFix)
	}
	// proc_list takes no path, so the tools list is the whole rule for it.
	ok(t, call(t, n.session, "proc_list", map[string]any{}), "proc_list")
}

// TestPermitsEmptyBlockIsAnEmptySurface is the default of PLAN.md section 2.3:
// a Process whose Package declares nothing reaches nothing.
func TestPermitsEmptyBlockIsAnEmptySurface(t *testing.T) {
	n := serveNarrowed(t, func(*whole) *manifest.Permits { return &manifest.Permits{} })

	if got := listed(t, n.session); len(got) != 0 {
		t.Errorf("tools/list is %v, want nothing", got)
	}
	for _, name := range []string{"fs_list", "proc_list", "ffmpeg_transcode", "users_me"} {
		refused(t, call(t, n.session, name, map[string]any{}), server.PermitsFix)
	}
}

// TestPermitsRefuseApprovingAQueuedPathOutsideThePrefixes closes the one door
// the guard cannot see through: approvals_approve runs the queued fs_write
// inside its own handler, so without a check there a Process permitted that
// tool could have anything under the shared root written for it.
func TestPermitsRefuseApprovingAQueuedPathOutsideThePrefixes(t *testing.T) {
	tr := newTracedWithDaemonPermits(t, func(w *whole) *manifest.Permits {
		return &manifest.Permits{
			Tools: []string{"approvals_*", "fs_*"},
			// The Process may name its own folder and nothing else under the
			// shared root.
			Paths: []string{filepath.Join(w.root, "flows")},
		}
	})
	tr.daemon.SetAdmin(true)
	tr.files.SetShared(tr.root)

	target := filepath.Join(tr.root, "handbook", "kitbash.yaml")
	input, _ := json.Marshal(map[string]any{
		"path": target, "content": handbookManifest, "message": "Add the handbook",
	})
	queued := tr.daemon.AddApproval(teltest.Approval{
		Requester: "member",
		Tool:      telemetry.ToolFSWrite,
		Input:     input,
	})

	res := call(t, tr.session, "approvals_approve", map[string]any{"id": queued.ID})
	ok(t, res, "approvals_approve")
	out := structured[approved](t, res)

	// The refusal is the approval's stored result, so the admin reads why
	// nothing happened rather than finding an approval that did nothing.
	var refusal problem.Problem
	if err := json.Unmarshal(out.Result, &refusal); err != nil {
		t.Fatalf("the result is not problem details: %v", err)
	}
	if refusal.Slug() != problem.SlugNotPermitted || refusal.Status != 403 {
		t.Fatalf("the result is %+v, want not-permitted at 403", refusal)
	}
	if refusal.Fix != server.PermitsPathFix {
		t.Errorf("fix is %q, want %q", refusal.Fix, server.PermitsPathFix)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("%s exists; the queued write ran outside every permitted prefix", target)
	}
	held, _ := tr.daemon.Approval(queued.ID)
	if !strings.Contains(string(held.Result), problem.SlugNotPermitted) {
		t.Errorf("the stored result is %s, want the refusal", held.Result)
	}
}

// TestApprovingIsUnchangedForAnAdminSession is the other side of the same
// door: a member at their own SSH session has no permits block, and approving
// executes the queued write exactly as it did.
func TestApprovingIsUnchangedForAnAdminSession(t *testing.T) {
	tr := approving(t)

	target := filepath.Join(tr.root, "handbook", "kitbash.yaml")
	input, _ := json.Marshal(map[string]any{
		"path": target, "content": handbookManifest, "message": "Add the handbook",
	})
	queued := tr.daemon.AddApproval(teltest.Approval{
		Requester: "member",
		Tool:      telemetry.ToolFSWrite,
		Input:     input,
	})

	res := call(t, tr.session, "approvals_approve", map[string]any{"id": queued.ID})
	ok(t, res, "approvals_approve")
	out := structured[approved](t, res)
	var written fs.WriteResult
	if err := json.Unmarshal(out.Result, &written); err != nil {
		t.Fatalf("the result is not an fs_write output: %v, body %s", err, out.Result)
	}
	if written.Path != target || len(written.Commit.Sha) != 40 {
		t.Fatalf("the result is %+v, want the file and its commit", written)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the approved write did not land: %v", err)
	}
}

// TestAMemberSessionIsNarrowedByNothing is the other half of the rule: an SSH
// session has no permits block and is not filtered, and a member exporting
// KITBASH_PERMITS in their own shell does not acquire one.
func TestAMemberSessionIsNarrowedByNothing(t *testing.T) {
	n := serveNarrowed(t, func(*whole) *manifest.Permits { return nil })

	got := listed(t, n.session)
	if len(got) < 12 {
		t.Errorf("tools/list is %v, want the whole surface", got)
	}
	ok(t, call(t, n.session, "proc_list", map[string]any{}), "proc_list")

	t.Setenv(manifest.EnvPermits, `{"tools":["fs_read"]}`)
	t.Setenv(telemetry.EnvCaller, "")
	permits, err := server.PermitsFromEnv()
	if err != nil {
		t.Fatalf("PermitsFromEnv: %v", err)
	}
	if permits != nil {
		t.Errorf("a session with no caller credential read %+v, want no block", permits)
	}

	// The session of a Process is the one that reads it.
	t.Setenv(telemetry.EnvCaller, "a-credential-kitbashd-minted")
	permits, err = server.PermitsFromEnv()
	if err != nil {
		t.Fatalf("PermitsFromEnv: %v", err)
	}
	if permits == nil || !permits.Match("fs_read", nil) || permits.Match("fs_write", nil) {
		t.Errorf("a Process session read %+v, want the declared block", permits)
	}

	// A Process session with no variable at all is a Process permitted
	// nothing. A daemon of an earlier release sets none, and so does this one
	// between deploy/install.sh replacing the binaries and restarting
	// kitbashd; the surface must not fall open to the owner's whole one there.
	os.Unsetenv(manifest.EnvPermits)
	permits, err = server.PermitsFromEnv()
	if err != nil {
		t.Fatalf("PermitsFromEnv without the variable: %v", err)
	}
	if permits == nil {
		t.Fatal("a Process session with no permits variable was narrowed by nothing")
	}
	if permits.Match("fs_read", nil) || permits.AnyPath() {
		t.Errorf("a Process session with no permits variable read %+v, want the empty block", permits)
	}

	// A block this build cannot read ends the session rather than widening it.
	t.Setenv(manifest.EnvPermits, `{"tools":["fs read"]}`)
	if _, err := server.PermitsFromEnv(); err == nil {
		t.Error("PermitsFromEnv accepted a block it cannot honour")
	}
}
