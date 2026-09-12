package server_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
	"github.com/zyx1121/kitbash/internal/telemetry/teltest"
)

// handbookManifest makes a folder visible, which is what a queued write in
// these tests asks for.
const handbookManifest = `name: handbook
description: How this organization works. Read before writing anything into this root.
`

// approved is the output of approvals_approve, decoded the way an agent reads
// it. The result is left raw: it is either the tool's output or a problem.
type approved struct {
	ID     string          `json:"id"`
	State  string          `json:"state"`
	Result json.RawMessage `json:"result"`
}

func TestSurfaceCarriesTheUsersAndApprovalsFamilies(t *testing.T) {
	tr := newTracedWithDaemon(t)

	tools, err := tr.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	published := map[string]bool{}
	for _, tool := range tools.Tools {
		published[tool.Name] = true
		if strings.HasPrefix(tool.Name, "users_") || strings.HasPrefix(tool.Name, "approvals_") {
			if tool.Description == "" || tool.InputSchema == nil || tool.OutputSchema == nil {
				t.Errorf("%s is published without a description or a schema", tool.Name)
			}
		}
	}
	for _, name := range []string{
		"users_me", "users_create", "users_list", "users_add_key", "users_remove",
		"approvals_list", "approvals_approve", "approvals_reject",
	} {
		if !published[name] {
			t.Errorf("%s is not on the surface", name)
		}
	}
}

// The users family is kitbashd's. The surface forwards the call and answers
// with the daemon's body.
func TestUsersToolsForwardToTheDaemon(t *testing.T) {
	tr := newTracedWithDaemon(t)
	tr.daemon.SetIdentity(teltest.Identity{User: "tester", UID: 1001, Admin: true,
		Groups: []string{"kitbash-users", "kitbash-admin"}})

	res := call(t, tr.session, "users_me", map[string]any{})
	ok(t, res, "users_me")
	me := structured[telemetry.Identity](t, res)
	if me.User != "tester" || !me.Admin {
		t.Errorf("users_me answered %+v, want the caller kitbashd knows", me)
	}

	res = call(t, tr.session, "users_create", map[string]any{
		"name":   "member",
		"sshKey": "ssh-ed25519 AAAA member@laptop",
	})
	ok(t, res, "users_create")
	created := structured[map[string]any](t, res)
	if created["user"] != "member" {
		t.Errorf("users_create answered %v, want the member it created", created)
	}
	members := tr.daemon.Members()
	if len(members) != 1 || members[0].User != "member" {
		t.Errorf("the daemon holds %+v, want the member the call created", members)
	}

	res = call(t, tr.session, "users_add_key", map[string]any{
		"name":   "member",
		"sshKey": "ssh-ed25519 BBBB member@desktop",
	})
	ok(t, res, "users_add_key")

	res = call(t, tr.session, "users_list", map[string]any{})
	ok(t, res, "users_list")
	listed := structured[struct {
		Users []teltest.Member `json:"users"`
	}](t, res)
	if len(listed.Users) != 1 || listed.Users[0].Keys != 2 {
		t.Errorf("users_list answered %+v, want the member with both keys", listed.Users)
	}

	res = call(t, tr.session, "users_remove", map[string]any{"name": "member"})
	ok(t, res, "users_remove")
	if len(tr.daemon.Members()) != 0 {
		t.Errorf("the daemon still holds %+v", tr.daemon.Members())
	}
}

// A refusal by kitbashd is the agent's answer unchanged: the surface adds no
// permission logic of its own.
func TestUsersToolPassesTheDaemonsProblemThrough(t *testing.T) {
	tr := newTracedWithDaemon(t)

	p := problemOf(t, call(t, tr.session, "users_list", map[string]any{}))
	if p.Slug() != problem.SlugNotPermitted {
		t.Errorf("slug is %s, want not-permitted", p.Slug())
	}
	if p.Status != 403 {
		t.Errorf("status is %d, want 403", p.Status)
	}
}

// A member's write under the shared root is queued instead of refused, and the
// queued problem carries the approval id the requester follows.
func TestMemberWriteUnderTheSharedRootIsQueuedOverTheSurface(t *testing.T) {
	tr := newTracedWithDaemon(t)
	tr.files.SetShared(tr.root)
	tr.files.SetApprovals(tr.provider.Client())
	target := filepath.Join(tr.root, "handbook", "kitbash.yaml")

	p := problemOf(t, call(t, tr.session, "fs_write", map[string]any{
		"path":    target,
		"content": handbookManifest,
		"message": "Add the handbook",
	}))
	if p.Slug() != problem.SlugQueued || p.Status != 202 {
		t.Fatalf("problem is %s with status %d, want queued 202", p.Slug(), p.Status)
	}
	queued := tr.daemon.Approvals()
	if len(queued) != 1 {
		t.Fatalf("the daemon holds %d approvals, want 1", len(queued))
	}
	if p.Instance != queued[0].ID {
		t.Errorf("instance is %q, want the approval id %q", p.Instance, queued[0].ID)
	}
	if queued[0].Tool != telemetry.ToolFSWrite || queued[0].State != telemetry.StatePending {
		t.Errorf("the queued call is %+v, want a pending fs_write", queued[0])
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("the file was written even though the call was queued")
	}
}

// Approving runs the queued call in the admin's session: the commit is
// authored by the requester and carries the trailer, and the result is stored
// where the requester reads it.
// approving is a session kitbashd calls an admin, with the fixture root
// standing in for /org: an approval is executed only under the shared root.
func approving(t *testing.T) *traced {
	t.Helper()
	tr := newTracedWithDaemon(t)
	tr.daemon.SetAdmin(true)
	tr.files.SetShared(tr.root)
	return tr
}

func TestApproveExecutesTheQueuedWrite(t *testing.T) {
	tr := approving(t)
	target := filepath.Join(tr.root, "handbook", "kitbash.yaml")
	input, _ := json.Marshal(map[string]any{
		"path":    target,
		"content": handbookManifest,
		"message": "Add the handbook",
	})
	queued := tr.daemon.AddApproval(teltest.Approval{
		Requester: "member",
		Tool:      telemetry.ToolFSWrite,
		Input:     input,
	})

	res := call(t, tr.session, "approvals_approve", map[string]any{
		"id": queued.ID, "note": "Looks right"})
	ok(t, res, "approvals_approve")
	out := structured[approved](t, res)
	if out.ID != queued.ID || out.State != telemetry.StateApproved {
		t.Fatalf("approvals_approve answered %+v, want the approved id", out)
	}
	var written fs.WriteResult
	if err := json.Unmarshal(out.Result, &written); err != nil {
		t.Fatalf("the result is not an fs_write output: %v", err)
	}
	if written.Path != target || len(written.Commit.Sha) != 40 {
		t.Fatalf("the result is %+v, want the file and its commit", written)
	}
	if written.Commit.Author != "member" {
		t.Errorf("the commit author is %q, want the requester", written.Commit.Author)
	}
	if !strings.Contains(written.Commit.Message, "Approved-by: tester") {
		t.Errorf("the commit message is %q, want the Approved-by trailer", written.Commit.Message)
	}
	// The commit is really in the repository, with the admin as committer.
	repo := filepath.Join(tr.root, "handbook")
	if got := gitLog(t, repo, "%an"); got != "member" {
		t.Errorf("git says the author is %q, want member", got)
	}
	if got := gitLog(t, repo, "%cn"); got != "tester" {
		t.Errorf("git says the committer is %q, want the admin who ran it", got)
	}
	// The requester reads the outcome off the approval.
	held, _ := tr.daemon.Approval(queued.ID)
	if held.State != telemetry.StateApproved || held.DecidedBy != "tester" {
		t.Errorf("the approval is %+v, want it approved by the admin", held)
	}
	var stored fs.WriteResult
	if err := json.Unmarshal(held.Result, &stored); err != nil {
		t.Fatalf("the stored result is not an fs_write output: %v", err)
	}
	if stored.Commit.Sha != written.Commit.Sha {
		t.Errorf("the stored result is %+v, want the one that was returned", stored)
	}

	// The admin's session records the span, so it says which approval it ran
	// and for whom.
	tr.flush(t)
	span, found := tr.daemon.Span("approvals_approve")
	if !found {
		t.Fatalf("no approvals_approve span reached the daemon: %+v", tr.daemon.Spans())
	}
	if got := span.Attributes[telemetry.AttrApproval]; got != queued.ID {
		t.Errorf("%s is %q, want the approval id", telemetry.AttrApproval, got)
	}
	if got := span.Attributes[telemetry.AttrRequester]; got != "member" {
		t.Errorf("%s is %q, want member", telemetry.AttrRequester, got)
	}
}

// An approved call is not privileged by having been approved: it goes through
// the same rules, and a failure is stored on the approval and returned.
func TestApproveStoresTheProblemWhenTheToolFails(t *testing.T) {
	tr := approving(t)
	input, _ := json.Marshal(map[string]any{
		"path":    filepath.Join(tr.root, "handbook", "kitbash.yaml"),
		"content": "name: Not Kebab Case\n",
		"message": "Add a broken manifest",
	})
	queued := tr.daemon.AddApproval(teltest.Approval{
		Requester: "member",
		Tool:      telemetry.ToolFSWrite,
		Input:     input,
	})

	res := call(t, tr.session, "approvals_approve", map[string]any{"id": queued.ID})
	ok(t, res, "approvals_approve")
	out := structured[approved](t, res)
	var failed problem.Problem
	if err := json.Unmarshal(out.Result, &failed); err != nil {
		t.Fatalf("the result is not problem details: %v", err)
	}
	if failed.Slug() != problem.SlugInvalidManifest {
		t.Errorf("the stored problem is %s, want invalid-manifest", failed.Slug())
	}
	held, _ := tr.daemon.Approval(queued.ID)
	if len(held.Result) == 0 {
		t.Fatal("the approval carries no result, so the requester learns nothing")
	}
	if !strings.Contains(string(held.Result), problem.SlugInvalidManifest) {
		t.Errorf("the stored result is %s, want the problem the tool returned", held.Result)
	}
}

// Rejecting is kitbashd's decision; the surface forwards it and nothing runs.
func TestRejectForwardsToTheDaemon(t *testing.T) {
	tr := approving(t)
	target := filepath.Join(tr.root, "handbook", "kitbash.yaml")
	input, _ := json.Marshal(map[string]any{
		"path":    target,
		"content": handbookManifest,
		"message": "Add the handbook",
	})
	queued := tr.daemon.AddApproval(teltest.Approval{
		Requester: "member", Tool: telemetry.ToolFSWrite, Input: input})

	res := call(t, tr.session, "approvals_reject", map[string]any{
		"id": queued.ID, "reason": "This belongs in your home folder"})
	ok(t, res, "approvals_reject")
	decided := structured[map[string]any](t, res)
	if decided["state"] != telemetry.StateRejected {
		t.Errorf("approvals_reject answered %v, want the rejected state", decided)
	}
	held, _ := tr.daemon.Approval(queued.ID)
	if held.Reason != "This belongs in your home folder" {
		t.Errorf("the stored reason is %q, want the admin's", held.Reason)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("the rejected write was carried out")
	}
}

// Only writes to the shared root are ever queued, so an approval naming
// anything else is refused rather than run. Without this the admin's own home
// is reachable through an approval, because the admin's roots include it and
// the kernel would allow the write.
func TestApproveRefusesAPathOutsideTheSharedRoot(t *testing.T) {
	tr := newTracedWithDaemon(t)
	tr.daemon.SetAdmin(true)
	// The shared root is one folder of the fixture; the target is another,
	// standing in for the admin's home.
	tr.files.SetShared(filepath.Join(tr.root, "org"))
	target := filepath.Join(tr.root, "home", "kitbash.yaml")
	input, _ := json.Marshal(map[string]any{
		"path":    target,
		"content": handbookManifest,
		"message": "Add the handbook",
	})
	queued := tr.daemon.AddApproval(teltest.Approval{
		Requester: "member", Tool: telemetry.ToolFSWrite, Input: input})

	res := call(t, tr.session, "approvals_approve", map[string]any{"id": queued.ID})
	ok(t, res, "approvals_approve")
	out := structured[approved](t, res)
	var refused problem.Problem
	if err := json.Unmarshal(out.Result, &refused); err != nil {
		t.Fatalf("the result is not problem details: %v", err)
	}
	if refused.Slug() != problem.SlugBadRequest {
		t.Errorf("the stored problem is %s, want bad-request", refused.Slug())
	}
	if !strings.Contains(refused.Detail, "outside the shared root") {
		t.Errorf("the detail is %q, want it to say the path is outside the shared root", refused.Detail)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the approval wrote outside the shared root")
	}
	// The admin reads why nothing happened off the approval as well.
	held, _ := tr.daemon.Approval(queued.ID)
	if !strings.Contains(string(held.Result), "outside the shared root") {
		t.Errorf("the stored result is %s, want the refusal", held.Result)
	}
}

// An approval that was decided already is kitbashd's to refuse, and its
// conflict is the agent's answer: two admins cannot run the same call twice.
func TestApproveOfADecidedApprovalIsTheDaemonsConflict(t *testing.T) {
	tr := approving(t)
	target := filepath.Join(tr.root, "handbook", "kitbash.yaml")
	input, _ := json.Marshal(map[string]any{
		"path":    target,
		"content": handbookManifest,
		"message": "Add the handbook",
	})
	queued := tr.daemon.AddApproval(teltest.Approval{
		Requester: "member", Tool: telemetry.ToolFSWrite, Input: input,
		State: telemetry.StateApproved})

	p := problemOf(t, call(t, tr.session, "approvals_approve", map[string]any{"id": queued.ID}))
	if p.Slug() != problem.SlugConflict || p.Status != 409 {
		t.Errorf("problem is %s with status %d, want the daemon's conflict 409", p.Slug(), p.Status)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("an approval that was already decided ran again")
	}
}

// The tool has run by the time the outcome is stored, so a daemon that refuses
// the result is reported to the admin rather than swallowed: the requester
// will never see what happened and only the admin can say so.
func TestApproveReportsAFailureToStoreTheResult(t *testing.T) {
	tr := approving(t)
	target := filepath.Join(tr.root, "handbook", "kitbash.yaml")
	input, _ := json.Marshal(map[string]any{
		"path":    target,
		"content": handbookManifest,
		"message": "Add the handbook",
	})
	queued := tr.daemon.AddApproval(teltest.Approval{
		Requester: "member", Tool: telemetry.ToolFSWrite, Input: input})
	tr.daemon.AnswerResult(teltest.Problem(409, "conflict", "Conflict",
		"this approval already carries a result", "Read it with approvals_list."))

	p := problemOf(t, call(t, tr.session, "approvals_approve", map[string]any{"id": queued.ID}))
	if p.Slug() != problem.SlugConflict {
		t.Errorf("problem is %s, want the daemon's conflict", p.Slug())
	}
	// The write itself happened, which is exactly why the admin is told.
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the approved write did not land: %v", err)
	}
	held, _ := tr.daemon.Approval(queued.ID)
	if len(held.Result) != 0 {
		t.Errorf("the approval carries %s, want nothing: the store refused", held.Result)
	}
}

// The requester reads the outcome with approvals_list, which is the only way
// they learn what the admin's session did.
func TestApprovalsListCarriesTheResult(t *testing.T) {
	tr := approving(t)
	input, _ := json.Marshal(map[string]any{
		"path":    filepath.Join(tr.root, "handbook", "kitbash.yaml"),
		"content": handbookManifest,
		"message": "Add the handbook",
	})
	queued := tr.daemon.AddApproval(teltest.Approval{
		Requester: "member", Tool: telemetry.ToolFSWrite, Input: input})
	ok(t, call(t, tr.session, "approvals_approve", map[string]any{"id": queued.ID}), "approvals_approve")

	res := call(t, tr.session, "approvals_list", map[string]any{"state": "approved"})
	ok(t, res, "approvals_list")
	page := structured[struct {
		Approvals []telemetry.Approval `json:"approvals"`
	}](t, res)
	if len(page.Approvals) != 1 {
		t.Fatalf("the approved page holds %d approvals, want the one that ran", len(page.Approvals))
	}
	listed := page.Approvals[0]
	if listed.ID != queued.ID || listed.Requester != "member" {
		t.Errorf("the listed approval is %+v, want the requester's own", listed)
	}
	var written fs.WriteResult
	if err := json.Unmarshal(listed.Result, &written); err != nil {
		t.Fatalf("the listed result is not an fs_write output: %v", err)
	}
	if len(written.Commit.Sha) != 40 || written.Commit.Author != "member" {
		t.Errorf("the listed result is %+v, want the commit the requester authored", written)
	}
}

// A member's approve is refused by kitbashd, and nothing runs here.
func TestApproveByAMemberIsRefused(t *testing.T) {
	tr := newTracedWithDaemon(t)
	target := filepath.Join(tr.root, "handbook", "kitbash.yaml")
	input, _ := json.Marshal(map[string]any{
		"path":    target,
		"content": handbookManifest,
		"message": "Add the handbook",
	})
	queued := tr.daemon.AddApproval(teltest.Approval{
		Requester: "member", Tool: telemetry.ToolFSWrite, Input: input})

	p := problemOf(t, call(t, tr.session, "approvals_approve", map[string]any{"id": queued.ID}))
	if p.Slug() != problem.SlugNotPermitted {
		t.Errorf("slug is %s, want not-permitted", p.Slug())
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("a member's approve executed the write")
	}
}

// gitLog reads one field of the newest commit of a repository.
func gitLog(t *testing.T, repo, format string) string {
	t.Helper()
	cmd := exec.Command("git", "-c", "safe.directory="+repo, "log", "-1", "--format="+format)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log in %s: %v", repo, err)
	}
	return strings.TrimSpace(string(out))
}
