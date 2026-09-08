package pkg_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/bridge"
	"github.com/zyx1121/kitbash/internal/pkg"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
)

// queue stands in for kitbashd, the same way internal/fs stands one up: it
// says whether the caller is an admin and keeps what was queued.
type queue struct {
	admin  bool
	silent bool
	queued []telemetry.Approval
}

func (q *queue) Admin(context.Context) (bool, bool) {
	if q.silent {
		return false, false
	}
	return q.admin, true
}

func (q *queue) CreateApproval(_ context.Context, tool string, input json.RawMessage) (*telemetry.Approval, *problem.Problem) {
	approval := telemetry.Approval{
		ID:        "0199a000-0000-7000-8000-000000000001",
		Requester: "member",
		Tool:      tool,
		Input:     input,
		State:     telemetry.StatePending,
	}
	q.queued = append(q.queued, approval)
	return &approval, nil
}

// sharedImporter is the import fixture with its root standing in for /org and
// a queue behind it.
func sharedImporter(t *testing.T, kits *stubKits, q *queue) (*pkg.Service, string) {
	t.Helper()
	f := newFixture(t)
	f.files.SetShared(f.root)
	f.files.SetApprovals(q)
	return pkg.New(f.files, f.runner, kits), f.root
}

// A member importing into the shared root queues the call rather than running
// an import kit: the admin who approves it runs the kit in their own session.
func TestImportUnderTheSharedRootIsQueued(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"files": []map[string]any{
		{"path": "kitbash.yaml", "content": importedManifest},
	}})
	kits := &stubKits{kits: []bridge.Kit{kit("/org/import-mcp", "^npm:")}, result: string(body)}
	q := &queue{}
	packages, root := sharedImporter(t, kits, q)
	target := filepath.Join(root, "time")

	_, prob := packages.Import(context.Background(), pkg.ImportRequest{
		Path: target, Source: "npm:time-mcp@1.0.0"})
	if prob == nil {
		t.Fatal("a member's import into the shared root was carried out")
	}
	if prob.Slug() != problem.SlugQueued || prob.Status != 202 {
		t.Fatalf("problem is %s with status %d, want queued 202", prob.Slug(), prob.Status)
	}
	if len(q.queued) != 1 {
		t.Fatalf("%d calls were queued, want 1", len(q.queued))
	}
	approval := q.queued[0]
	if prob.Instance != approval.ID {
		t.Errorf("instance is %q, want the approval id %q", prob.Instance, approval.ID)
	}
	if approval.Tool != telemetry.ToolPkgImport {
		t.Errorf("the queued tool is %q, want pkg_import", approval.Tool)
	}
	var input map[string]any
	if err := json.Unmarshal(approval.Input, &input); err != nil {
		t.Fatalf("the queued input is not JSON: %v", err)
	}
	if input["path"] != target || input["source"] != "npm:time-mcp@1.0.0" {
		t.Errorf("the queued input is %v, want the tool's input", input)
	}
	// Nothing ran: no kit was called and no folder was written.
	if kits.called != nil {
		t.Errorf("the import kit was called with %v, want it left alone", kits.called)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("the folder was written even though the call was queued")
	}
}

// A folder that already exists is a conflict before anything is queued: an
// admin never reads a call that could not have run.
func TestImportOfAnExistingFolderIsNotQueued(t *testing.T) {
	kits := &stubKits{kits: []bridge.Kit{kit("/org/import-mcp", "^npm:")}}
	q := &queue{}
	packages, root := sharedImporter(t, kits, q)
	target := filepath.Join(root, "time")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	_, prob := packages.Import(context.Background(), pkg.ImportRequest{
		Path: target, Source: "npm:time-mcp@1.0.0"})
	if prob == nil {
		t.Fatal("an existing folder was imported into")
	}
	if prob.Slug() != problem.SlugConflict {
		t.Errorf("problem is %s, want conflict", prob.Slug())
	}
	if len(q.queued) != 0 {
		t.Errorf("%d calls were queued, want none", len(q.queued))
	}
}

// An admin imports into the shared root directly, and the commit is the
// admin's own.
func TestAdminImportUnderTheSharedRootRuns(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"files": []map[string]any{
		{"path": "kitbash.yaml", "content": importedManifest},
	}})
	kits := &stubKits{kits: []bridge.Kit{kit("/org/import-mcp", "^npm:")}, result: string(body)}
	q := &queue{admin: true}
	packages, root := sharedImporter(t, kits, q)

	out, prob := packages.Import(context.Background(), pkg.ImportRequest{
		Path: filepath.Join(root, "time"), Source: "npm:time-mcp@1.0.0"})
	if prob != nil {
		t.Fatalf("Import: %s", prob.Detail)
	}
	if len(q.queued) != 0 {
		t.Errorf("%d calls were queued, want none", len(q.queued))
	}
	if out.Commit.Author != "tester" {
		t.Errorf("the commit author is %q, want the admin who imported it", out.Commit.Author)
	}
}

// An approved import is one commit authored by the member who asked for it.
func TestApprovedImportIsAuthoredByTheRequester(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"files": []map[string]any{
		{"path": "kitbash.yaml", "content": importedManifest},
	}})
	kits := &stubKits{kits: []bridge.Kit{kit("/org/import-mcp", "^npm:")}, result: string(body)}
	q := &queue{admin: true}
	packages, root := sharedImporter(t, kits, q)

	out, prob := packages.Import(context.Background(), pkg.ImportRequest{
		Path:       filepath.Join(root, "time"),
		Source:     "npm:time-mcp@1.0.0",
		Author:     "member",
		ApprovedBy: "tester",
	})
	if prob != nil {
		t.Fatalf("Import: %s", prob.Detail)
	}
	if out.Commit.Author != "member" {
		t.Errorf("the commit author is %q, want the requester", out.Commit.Author)
	}
	if !strings.Contains(out.Commit.Message, "Approved-by: tester") {
		t.Errorf("the commit message is %q, want the Approved-by trailer", out.Commit.Message)
	}
}
