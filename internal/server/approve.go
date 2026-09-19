package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/manifest"
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
func approveHandler(client *telemetry.Client, files *fs.Service, packages *pkg.Service, permits *manifest.Permits) mcp.ToolHandlerFor[approveInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in approveInput) (*mcp.CallToolResult, any, error) {
		approval, prob := client.ClaimApproval(ctx, in.ID, in.Note)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		// The span of this call says which approval it ran and for whom, so a
		// query by user finds the write the requester never made themselves.
		telemetry.SetApproval(ctx, approval.ID)
		telemetry.SetRequester(ctx, approval.Requester)

		result := execute(ctx, files, packages, permits, approval)
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
func execute(ctx context.Context, files *fs.Service, packages *pkg.Service, permits *manifest.Permits, approval *telemetry.Approval) json.RawMessage {
	out, prob := run(ctx, files, packages, permits, approval)
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
func run(ctx context.Context, files *fs.Service, packages *pkg.Service, permits *manifest.Permits, approval *telemetry.Approval) (any, *problem.Problem) {
	admin := files.User()
	switch approval.Tool {
	case telemetry.ToolFSWrite:
		var in writeInput
		if prob := decodeInput(approval, &in); prob != nil {
			return nil, prob
		}
		if prob := mixedForms(in); prob != nil {
			return nil, prob
		}
		// A queued list is one approval, so every path in it is confined and
		// permitted before any of it runs, the same as the one path form.
		for _, path := range approvedPaths(in) {
			if prob := confined(files, approval, path); prob != nil {
				return nil, prob
			}
			if prob := permittedApproval(permits, path); prob != nil {
				return nil, prob
			}
		}
		if len(in.Files) > 0 {
			telemetry.SetPath(ctx, in.Files[0].Path)
			return files.WriteAll(ctx, fs.WriteAllRequest{
				Files:       in.entries(),
				Message:     in.Message,
				ExpectedSha: in.ExpectedSha,
				Author:      approval.Requester,
				ApprovedBy:  admin,
			})
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
		if prob := permittedApproval(permits, in.Path); prob != nil {
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

// approvedPaths are the Files paths one approved write names: the one path, or
// every path of a list. An empty list answers the one path, which is empty for
// an input that carries neither and is refused by the tool.
func approvedPaths(in writeInput) []string {
	if len(in.Files) == 0 {
		return []string{in.Path}
	}
	paths := make([]string, 0, len(in.Files))
	for _, file := range in.Files {
		paths = append(paths, file.Path)
	}
	return paths
}

// permittedApproval refuses an approved call whose queued path is outside what
// this session may name. Approving executes the queued tool inside this
// handler rather than as a call of its own, so the permits guard never sees
// that write: without this, a Process permitted approvals_approve could have
// anything under the shared root written for it by claiming an approval, which
// is every prefix rule undone by one tool.
//
// A member's or an admin's own session has no permits and is unchanged. The
// problem is returned rather than raised, so it is stored on the approval the
// way every other refusal of an approved call is and the admin reads why
// nothing happened.
func permittedApproval(permits *manifest.Permits, path string) *problem.Problem {
	if permits == nil || permits.Allows(path) {
		return nil
	}
	return problem.NotPermitted(path,
		fmt.Sprintf("this Process may not approve a call that names %s", path), PermitsPathFix)
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
