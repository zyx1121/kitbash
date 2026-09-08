package fs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
)

// queue stands in for kitbashd: it says whether the caller is an admin and
// records what was queued. The temporary root of the fixture stands in for
// /org, which is the only thing about /org this package knows.
type queue struct {
	admin bool
	// silent is a daemon that did not answer who the caller is, which is what
	// a host whose kitbashd is down looks like from here.
	silent bool
	queued []telemetry.Approval
	refuse *problem.Problem
	ids    int
}

func (q *queue) Admin(context.Context) (bool, bool) {
	if q.silent {
		return false, false
	}
	return q.admin, true
}

func (q *queue) CreateApproval(_ context.Context, tool string, input json.RawMessage) (*telemetry.Approval, *problem.Problem) {
	if q.refuse != nil {
		return nil, q.refuse
	}
	q.ids++
	approval := telemetry.Approval{
		ID:        fmt.Sprintf("0199a000-0000-7000-8000-%012d", q.ids),
		Requester: "member",
		Tool:      tool,
		Input:     input,
		State:     telemetry.StatePending,
	}
	q.queued = append(q.queued, approval)
	return &approval, nil
}

// shared is the fixture for the queue: the tree, with its root standing in for
// /org and a queue behind it.
func shared(t *testing.T, admin bool) (*fs.Service, string, *queue) {
	t.Helper()
	service, root := tree(t)
	q := &queue{admin: admin}
	service.SetShared(root)
	service.SetApprovals(q)
	return service, root, q
}

func TestMemberWriteUnderTheSharedRootIsQueued(t *testing.T) {
	service, root, q := shared(t, false)
	target := filepath.Join(root, "handbook", "onboarding.md")
	body := "# Onboarding\n"

	_, prob := service.Write(context.Background(), fs.WriteRequest{
		Path:        target,
		Content:     &body,
		Message:     "Add the onboarding page",
		ExpectedSha: "b0a0f7e3e2b1c4d5a6b7c8d9e0f1a2b3c4d5e6f7",
	})
	if prob == nil {
		t.Fatal("a member's write under the shared root was carried out")
	}
	if prob.Slug() != problem.SlugQueued {
		t.Fatalf("problem is %s, want queued", prob.Slug())
	}
	if prob.Status != 202 {
		t.Errorf("status is %d, want 202", prob.Status)
	}
	if len(q.queued) != 1 {
		t.Fatalf("%d calls were queued, want 1", len(q.queued))
	}
	approval := q.queued[0]
	if prob.Instance != approval.ID {
		t.Errorf("instance is %q, want the approval id %q", prob.Instance, approval.ID)
	}
	if !strings.Contains(prob.Fix, "approvals_approve "+approval.ID) {
		t.Errorf("fix is %q, want it to name the approval", prob.Fix)
	}
	if approval.Tool != telemetry.ToolFSWrite {
		t.Errorf("the queued tool is %q, want fs_write", approval.Tool)
	}
	// The input is the tool's input as it was made, so the admin approves the
	// call the member asked for and not a summary of it.
	var input map[string]any
	if err := json.Unmarshal(approval.Input, &input); err != nil {
		t.Fatalf("the queued input is not JSON: %v", err)
	}
	want := map[string]any{
		"path":        target,
		"content":     body,
		"message":     "Add the onboarding page",
		"expectedSha": "b0a0f7e3e2b1c4d5a6b7c8d9e0f1a2b3c4d5e6f7",
	}
	for key, value := range want {
		if input[key] != value {
			t.Errorf("the queued input has %s = %v, want %v", key, input[key], value)
		}
	}
	if len(input) != len(want) {
		t.Errorf("the queued input is %v, want exactly %v", input, want)
	}
	// A queued call is a call that did not happen.
	if _, err := os.Stat(target); err == nil {
		t.Error("the file was written even though the call was queued")
	}
	if _, err := os.Stat(filepath.Join(root, "handbook", ".git")); err == nil {
		t.Error("the repository was created even though the call was queued")
	}
}

// A call that would have been refused is refused now, not queued: an admin
// reading the queue sees calls that could run.
func TestRefusedWritesUnderTheSharedRootAreNotQueued(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
		slug string
	}{
		{
			name: "dot component",
			path: filepath.Join("handbook", ".ssh", "authorized_keys"),
			body: "ssh-ed25519 AAAA\n",
			slug: problem.SlugInvalidPath,
		},
		{
			name: "parent segment",
			path: filepath.Join("handbook", "..", "..", "escape.md"),
			body: "x\n",
			slug: problem.SlugInvalidPath,
		},
		{
			name: "invalid manifest",
			path: filepath.Join("handbook", "kitbash.yaml"),
			body: "name: Not Kebab Case\n",
			slug: problem.SlugInvalidManifest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, root, q := shared(t, false)
			body := tc.body
			_, prob := service.Write(context.Background(), fs.WriteRequest{
				Path:    filepath.Join(root, tc.path),
				Content: &body,
				Message: "Write something kitbash refuses",
			})
			if prob == nil {
				t.Fatal("the write was accepted")
			}
			if prob.Slug() != tc.slug {
				t.Errorf("problem is %s, want %s", prob.Slug(), tc.slug)
			}
			if len(q.queued) != 0 {
				t.Errorf("%d calls were queued, want none", len(q.queued))
			}
		})
	}
}

// An admin writes the shared root directly. The permission is the kernel's,
// granted by the group on the folder; kitbash only declines to queue.
func TestAdminWriteUnderTheSharedRootIsNotQueued(t *testing.T) {
	service, root, q := shared(t, true)
	target := filepath.Join(root, "handbook", "policy.md")
	body := "# Policy\n"

	out, prob := service.Write(context.Background(), fs.WriteRequest{
		Path:    target,
		Content: &body,
		Message: "Add the policy page",
	})
	if prob != nil {
		t.Fatalf("Write: %s", prob.Detail)
	}
	if len(q.queued) != 0 {
		t.Errorf("%d calls were queued, want none", len(q.queued))
	}
	if out.Commit.Author != "tester" {
		t.Errorf("the commit author is %q, want the admin who wrote it", out.Commit.Author)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the file was not written: %v", err)
	}
}

// A daemon that does not answer queues nothing. The write is attempted and the
// kernel decides, which is what a host without kitbashd does: an admin's write
// lands through the group bits and a member's is refused. The fixture root is
// writable, so this is the admin half of that.
func TestAWriteIsNotQueuedWhenTheDaemonDoesNotAnswer(t *testing.T) {
	service, root, q := shared(t, false)
	q.silent = true
	target := filepath.Join(root, "handbook", "onboarding.md")
	body := "# Onboarding\n"

	out, prob := service.Write(context.Background(), fs.WriteRequest{
		Path:    target,
		Content: &body,
		Message: "Add the onboarding page",
	})
	if prob != nil {
		t.Fatalf("Write: %s", prob.Detail)
	}
	if len(q.queued) != 0 {
		t.Errorf("%d calls were queued, want none: there was nothing to queue with", len(q.queued))
	}
	if out.Commit.Sha == "" {
		t.Error("the write produced no commit")
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the file was not written: %v", err)
	}
}

// A queue that refuses is not a queue that lets the write through.
func TestAQueueThatRefusesPassesTheProblemThrough(t *testing.T) {
	service, root, q := shared(t, false)
	q.refuse = problem.ConflictFix("member", "you hold 64 pending approvals", "Wait for an admin.")
	target := filepath.Join(root, "handbook", "onboarding.md")
	body := "# Onboarding\n"

	_, prob := service.Write(context.Background(), fs.WriteRequest{
		Path:    target,
		Content: &body,
		Message: "Add the onboarding page",
	})
	if prob == nil {
		t.Fatal("the write happened although the queue refused it")
	}
	if prob.Slug() != problem.SlugConflict {
		t.Errorf("problem is %s, want the daemon's conflict", prob.Slug())
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("the file was written even though the call could not be queued")
	}
}

// An approved call is committed by the admin whose session ran it and authored
// by the member who asked for it, with a trailer naming the admin.
func TestApprovedWriteIsAuthoredByTheRequester(t *testing.T) {
	service, root, _ := shared(t, true)
	target := filepath.Join(root, "handbook", "onboarding.md")
	body := "# Onboarding\n"

	out, prob := service.Write(context.Background(), fs.WriteRequest{
		Path:       target,
		Content:    &body,
		Message:    "Add the onboarding page",
		Author:     "member",
		ApprovedBy: "tester",
	})
	if prob != nil {
		t.Fatalf("Write: %s", prob.Detail)
	}
	if out.Commit.Author != "member" {
		t.Errorf("the commit author is %q, want the requester", out.Commit.Author)
	}
	if !strings.Contains(out.Commit.Message, "Approved-by: tester") {
		t.Errorf("the commit message is %q, want the Approved-by trailer", out.Commit.Message)
	}
	committer := gitShow(t, filepath.Join(root, "handbook"), "%cn")
	if committer != "tester" {
		t.Errorf("the committer is %q, want the admin whose session ran it", committer)
	}
}

// A name git would read as anything but a member is refused before it reaches
// the command line.
func TestWriteRefusesAnAuthorThatIsNotAMemberName(t *testing.T) {
	service, root, _ := shared(t, true)
	body := "x\n"

	_, prob := service.Write(context.Background(), fs.WriteRequest{
		Path:       filepath.Join(root, "handbook", "onboarding.md"),
		Content:    &body,
		Message:    "Add the onboarding page",
		Author:     "member <someone@example.org>",
		ApprovedBy: "tester",
	})
	if prob == nil {
		t.Fatal("an author that is not a member name was accepted")
	}
	if prob.Slug() != problem.SlugBadRequest {
		t.Errorf("problem is %s, want bad-request", prob.Slug())
	}
}

// /org is 2775 and owned by kitbash-admin, so what an admin writes there has
// to stay writable by the group: with the default umask the next admin could
// not replace a file the first one wrote.
func TestWritesUnderTheSharedRootStayGroupWritable(t *testing.T) {
	service, root, _ := shared(t, true)
	folder := filepath.Join(root, "policies")
	manifest := "name: policies\ndescription: What this organization requires of everyone who works here.\n"

	if _, prob := service.Write(context.Background(), fs.WriteRequest{
		Path:    filepath.Join(folder, "kitbash.yaml"),
		Content: &manifest,
		Message: "Add the policies folder",
	}); prob != nil {
		t.Fatalf("Write: %s", prob.Detail)
	}

	if got := mode(t, filepath.Join(folder, "kitbash.yaml")); got.Perm() != 0o664 {
		t.Errorf("the file is %v, want 0664 so another admin can replace it", got.Perm())
	}
	// The folder the write created keeps the group and the setgid bit, so what
	// lands inside it stays admin writable too.
	if got := mode(t, folder); got.Perm() != 0o775 || got&os.ModeSetgid == 0 {
		t.Errorf("%s is %v, want 2775", folder, got)
	}
	// git writes its objects read only unless the repository says it is
	// shared, so the second admin to commit would be refused by the kernel.
	if got := gitConfig(t, folder, "core.sharedRepository"); got != "group" {
		t.Errorf("core.sharedRepository is %q, want group", got)
	}
}

// A multi file write is the same rule: an imported Package under the shared
// root is as replaceable as a written one.
func TestWriteFilesUnderTheSharedRootStayGroupWritable(t *testing.T) {
	service, root, _ := shared(t, true)
	manifest := "name: time\ndescription: A wrapped MCP server that answers what the time is right now.\n"
	body := "FROM alpine\n"
	folder := filepath.Join(root, "time")

	if _, prob := service.WriteFiles(context.Background(), fs.WriteFilesRequest{
		Path: folder,
		Files: []fs.File{
			{Path: "kitbash.yaml", Content: &manifest},
			{Path: "src/Containerfile", Content: &body},
		},
		Message: "Import npm:time-mcp",
	}); prob != nil {
		t.Fatalf("WriteFiles: %s", prob.Detail)
	}

	for _, name := range []string{"kitbash.yaml", filepath.Join("src", "Containerfile")} {
		if got := mode(t, filepath.Join(folder, name)); got.Perm() != 0o664 {
			t.Errorf("%s is %v, want 0664", name, got.Perm())
		}
	}
	if got := mode(t, filepath.Join(folder, "src")); got.Perm() != 0o775 || got&os.ModeSetgid == 0 {
		t.Errorf("the src folder is %v, want 2775", got)
	}
}

// A member's home is theirs alone, so nothing about it is widened.
func TestWritesOutsideTheSharedRootKeepTheDefaultModes(t *testing.T) {
	service, root := tree(t)
	folder := filepath.Join(root, "notes")
	manifest := "name: notes\ndescription: Notes a member keeps to themselves in their own home.\n"

	if _, prob := service.Write(context.Background(), fs.WriteRequest{
		Path:    filepath.Join(folder, "kitbash.yaml"),
		Content: &manifest,
		Message: "Add a private folder",
	}); prob != nil {
		t.Fatalf("Write: %s", prob.Detail)
	}
	if got := mode(t, filepath.Join(folder, "kitbash.yaml")); got.Perm() != 0o644 {
		t.Errorf("the file is %v, want the default 0644", got.Perm())
	}
	if got := mode(t, folder); got.Perm() != 0o755 || got&os.ModeSetgid != 0 {
		t.Errorf("the folder is %v, want the default 0755", got)
	}
	if got := gitConfig(t, folder, "core.sharedRepository"); got != "" {
		t.Errorf("core.sharedRepository is %q, want it unset in a member's home", got)
	}
}

// mode is the mode of one path.
func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode()
}

// gitConfig reads one setting of a repository, empty when it is not set.
func gitConfig(t *testing.T, repo, key string) string {
	t.Helper()
	cmd := exec.Command("git", "-c", "safe.directory="+repo, "config", "--get", key)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		// git exits 1 for a setting that is not there, which is an answer.
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitShow reads one field of the newest commit of a repository, which is how
// the committer is checked: the surface returns the author and says nothing
// about who ran the commit.
func gitShow(t *testing.T, repo, format string) string {
	t.Helper()
	cmd := exec.Command("git", "-c", "safe.directory="+repo, "log", "-1", "--format="+format)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log in %s: %v", repo, err)
	}
	return strings.TrimSpace(string(out))
}
