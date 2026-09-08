package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/pkg"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
)

// approveResult is the output of approvals_approve. The result is the tool's
// own output, or the problem the execution returned: an approval that ran and
// failed is still an approval that was decided.
type approveResult struct {
	ID     string          `json:"id"`
	State  string          `json:"state"`
	Result json.RawMessage `json:"result"`
}

// approveHandler answers approvals_approve. It claims the approval through
// kitbashd, which is where admin and state are enforced, then runs the tool
// here, in the admin's session, as the admin's Linux user. That is the whole
// mechanism: the write into /org succeeds because of the group permission on
// the folder, not because kitbash granted anything, see PLAN.md section 2.1.
//
// The requester's input is not privileged by having been approved. It goes
// through the same fs.Write and pkg.Import as any other call, with the same
// path, manifest and size rules; only the authorship of the commit differs.
func approveHandler(client *telemetry.Client, files *fs.Service, packages *pkg.Service) mcp.ToolHandlerFor[approveInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in approveInput) (*mcp.CallToolResult, any, error) {
		approval, prob := client.ClaimApproval(ctx, in.ID, in.Note)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		// The span of this call says which approval it ran and for whom, so a
		// query by user finds the write the requester never made themselves.
		telemetry.SetApproval(ctx, approval.ID)
		telemetry.SetRequester(ctx, approval.Requester)

		result := execute(ctx, files, packages, approval)
		if prob := client.StoreApprovalResult(ctx, approval.ID, result); prob != nil {
			// The tool has run. Saying so is the daemon's job and it refused,
			// so the caller is told rather than left with a result the
			// requester will never see.
			return errorResult(prob), nil, nil
		}
		return structuredResult(approveResult{
			ID:     approval.ID,
			State:  telemetry.StateApproved,
			Result: result,
		})
	}
}

// execute runs one approved call and returns what is stored on the approval:
// the tool's output, or the problem it failed with.
func execute(ctx context.Context, files *fs.Service, packages *pkg.Service, approval *telemetry.Approval) json.RawMessage {
	out, prob := run(ctx, files, packages, approval)
	if prob != nil {
		return json.RawMessage(prob.JSON())
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage(problem.Internal(approval.ID, err.Error(), "").JSON())
	}
	return encoded
}

// run dispatches one approved call. The admin is the session's own Linux user,
// which is who the commit is committed by.
//
// The path is confined to the shared root before anything runs. Only writes to
// the shared root are queued, so an approval naming anything else is not a
// call this session executes: the tool would otherwise happily write the
// admin's own home on a requester's behalf, because the admin's roots include
// it and the kernel would allow it.
func run(ctx context.Context, files *fs.Service, packages *pkg.Service, approval *telemetry.Approval) (any, *problem.Problem) {
	admin := files.User()
	switch approval.Tool {
	case telemetry.ToolFSWrite:
		var in writeInput
		if prob := decodeInput(approval, &in); prob != nil {
			return nil, prob
		}
		if prob := confined(files, approval, in.Path); prob != nil {
			return nil, prob
		}
		telemetry.SetPath(ctx, in.Path)
		return files.Write(ctx, fs.WriteRequest{
			Path:          in.Path,
			Content:       in.Content,
			ContentBase64: in.ContentBase64,
			Message:       in.Message,
			ExpectedSha:   in.ExpectedSha,
			Author:        approval.Requester,
			ApprovedBy:    admin,
		})
	case telemetry.ToolPkgImport:
		if packages == nil {
			return nil, problem.Internal(approval.ID,
				"this session serves no pkg family, so an import cannot be executed", "")
		}
		var in importInput
		if prob := decodeInput(approval, &in); prob != nil {
			return nil, prob
		}
		if prob := confined(files, approval, in.Path); prob != nil {
			return nil, prob
		}
		telemetry.SetPackage(ctx, in.Path)
		return packages.Import(ctx, pkg.ImportRequest{
			Path:       in.Path,
			Source:     in.Source,
			Author:     approval.Requester,
			ApprovedBy: admin,
		})
	default:
		return nil, problem.BadRequest(approval.ID,
			fmt.Sprintf("%q is not a tool this session can execute", approval.Tool),
			"Only fs_write and pkg_import are queued, so only those can be approved.")
	}
}

// confined refuses an approved call whose path is not under the shared root.
// The problem is stored on the approval like any other outcome, so the admin
// reads why nothing happened and rejects it.
func confined(files *fs.Service, approval *telemetry.Approval, path string) *problem.Problem {
	if files.UnderShared(path) {
		return nil
	}
	return problem.BadRequest(approval.ID, "the queued path is outside the shared root",
		fmt.Sprintf("Reject this approval with approvals_reject; %s only queues writes under %s.",
			approval.Tool, sharedOrNone(files)))
}

// sharedOrNone names the shared root, or says there is none.
func sharedOrNone(files *fs.Service) string {
	if shared := files.Shared(); shared != "" {
		return shared
	}
	return "the shared root, which this session does not have"
}

// decodeInput reads the input the approval stored. It is the requester's, so a
// body that does not fit the tool is the requester's mistake and not this
// session's failure.
func decodeInput(approval *telemetry.Approval, into any) *problem.Problem {
	if err := json.Unmarshal(approval.Input, into); err != nil {
		return problem.BadRequest(approval.ID,
			fmt.Sprintf("the queued input is not a %s input: %v", approval.Tool, err),
			"Reject this approval; the call has to be made again.")
	}
	return nil
}
