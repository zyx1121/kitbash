package telemetry_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
	"github.com/zyx1121/kitbash/internal/telemetry/teltest"
)

// writeInput is one queued fs_write, as the tool's input schema declares it.
const writeInput = `{"path":"/org/handbook/onboarding.md","content":"# Onboarding\n","message":"Add the onboarding page"}`

func TestApprovalGoesFromPendingToApprovedWithAResult(t *testing.T) {
	daemon := newDaemon(t)
	client := telemetry.NewClient(daemon.Socket)
	ctx := context.Background()

	approval, prob := client.CreateApproval(ctx, telemetry.ToolFSWrite, json.RawMessage(writeInput))
	if prob != nil {
		t.Fatalf("CreateApproval: %s", prob.Detail)
	}
	if approval.ID == "" || approval.State != telemetry.StatePending {
		t.Fatalf("the approval is %+v, want a pending one with an id", approval)
	}
	// The input is stored as it was sent: an admin approves the call the
	// member made.
	held, _ := daemon.Approval(approval.ID)
	if string(held.Input) != writeInput {
		t.Errorf("the daemon holds %s, want the input verbatim", held.Input)
	}

	// A member may not claim it.
	if _, prob := client.ClaimApproval(ctx, approval.ID, ""); prob == nil {
		t.Error("a member claimed an approval")
	} else if prob.Slug() != problem.SlugNotPermitted {
		t.Errorf("problem is %s, want not-permitted", prob.Slug())
	}

	daemon.SetAdmin(true)
	claimed, prob := client.ClaimApproval(ctx, approval.ID, "Looks right")
	if prob != nil {
		t.Fatalf("ClaimApproval: %s", prob.Detail)
	}
	if claimed.State != telemetry.StateApproved || claimed.Tool != telemetry.ToolFSWrite {
		t.Fatalf("the claim answered %+v, want the approved call and its tool", claimed)
	}
	if string(claimed.Input) != writeInput {
		t.Errorf("the claim carried %s, want the input verbatim", claimed.Input)
	}

	// Claiming it twice is a conflict, so two admins cannot both run it.
	if _, prob := client.ClaimApproval(ctx, approval.ID, ""); prob == nil {
		t.Error("the same approval was claimed twice")
	} else if prob.Status != http.StatusConflict {
		t.Errorf("status is %d, want 409", prob.Status)
	}

	result := json.RawMessage(`{"path":"/org/handbook/onboarding.md","commit":{"sha":"abc"}}`)
	if prob := client.StoreApprovalResult(ctx, approval.ID, result); prob != nil {
		t.Fatalf("StoreApprovalResult: %s", prob.Detail)
	}
	stored, _ := daemon.Approval(approval.ID)
	if string(stored.Result) != string(result) {
		t.Errorf("the stored result is %s, want the tool's output", stored.Result)
	}
	// The result is written once.
	if prob := client.StoreApprovalResult(ctx, approval.ID, result); prob == nil {
		t.Error("the result was stored twice")
	}

	listed, prob := client.ListApprovals(ctx, telemetry.StateApproved)
	if prob != nil {
		t.Fatalf("ListApprovals: %s", prob.Detail)
	}
	var page struct {
		Approvals []telemetry.Approval `json:"approvals"`
	}
	if err := json.Unmarshal(listed, &page); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if len(page.Approvals) != 1 || page.Approvals[0].ID != approval.ID {
		t.Fatalf("the approved page is %+v, want the one that was executed", page.Approvals)
	}
	if string(page.Approvals[0].Result) != string(result) {
		t.Errorf("the listed result is %s, want the stored one", page.Approvals[0].Result)
	}
}

func TestRejectingAnApprovalStoresTheReason(t *testing.T) {
	daemon := newDaemon(t)
	daemon.SetAdmin(true)
	client := telemetry.NewClient(daemon.Socket)
	ctx := context.Background()
	queued := daemon.AddApproval(teltest.Approval{
		Tool: telemetry.ToolFSWrite, Input: json.RawMessage(writeInput)})

	body, prob := client.RejectApproval(ctx, queued.ID, "This belongs in your home folder")
	if prob != nil {
		t.Fatalf("RejectApproval: %s", prob.Detail)
	}
	var decided map[string]any
	if err := json.Unmarshal(body, &decided); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if decided["state"] != telemetry.StateRejected {
		t.Errorf("approvals_reject answered %v, want the rejected state", decided)
	}
	held, _ := daemon.Approval(queued.ID)
	if held.Reason != "This belongs in your home folder" {
		t.Errorf("the stored reason is %q, want the one the admin gave", held.Reason)
	}
	// A rejected approval cannot then be approved.
	if _, prob := client.ClaimApproval(ctx, queued.ID, ""); prob == nil {
		t.Error("a rejected approval was claimed")
	} else if prob.Status != http.StatusConflict {
		t.Errorf("status is %d, want 409", prob.Status)
	}
}

// A member holding the cap of pending approvals cannot queue another, which is
// what bounds what one member costs the daemon.
func TestTheQueueRefusesBeyondThePendingCap(t *testing.T) {
	daemon := newDaemon(t)
	client := telemetry.NewClient(daemon.Socket)
	ctx := context.Background()
	for i := range teltest.PendingLimit {
		daemon.AddApproval(teltest.Approval{
			Tool:  telemetry.ToolFSWrite,
			Input: json.RawMessage(fmt.Sprintf(`{"path":"/org/handbook/%d.md"}`, i)),
		})
	}

	_, prob := client.CreateApproval(ctx, telemetry.ToolFSWrite, json.RawMessage(writeInput))
	if prob == nil {
		t.Fatal("a member queued more than the cap")
	}
	if prob.Slug() != problem.SlugConflict {
		t.Errorf("problem is %s, want conflict", prob.Slug())
	}
}

// A claim of an id nobody queued is not found, and the client says so rather
// than inventing an approval.
func TestClaimingAnUnknownApproval(t *testing.T) {
	daemon := newDaemon(t)
	daemon.SetAdmin(true)
	client := telemetry.NewClient(daemon.Socket)

	_, prob := client.ClaimApproval(context.Background(), "0199a000-0000-7000-8000-999999999999", "")
	if prob == nil {
		t.Fatal("an unknown approval was claimed")
	}
	if prob.Slug() != problem.SlugNotFound {
		t.Errorf("problem is %s, want not-found", prob.Slug())
	}
}
