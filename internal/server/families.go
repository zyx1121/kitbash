package server

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/bridge"
	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/pkg"
	"github.com/zyx1121/kitbash/internal/proc"
	"github.com/zyx1121/kitbash/internal/telemetry"
)

// RegisterPackages adds the pkg family to an existing MCP server.
func RegisterPackages(s *mcp.Server, packages *pkg.Service) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "pkg_build",
		Description: "Build the Package at path from its current commit with the caller's rootless container " +
			"runtime. The digest is the OCI image ID. The image is labelled kitbash.path, kitbash.name, " +
			"kitbash.commit and kitbash.user, which is the whole build record. Only the tail of the build log " +
			"is returned; the full log never leaves the host. A build that runs and fails returns that same " +
			"tail as a bad-request. kitbash builds from a commit, so an uncommitted change under the path is " +
			"a conflict. Version 1 supports one container unit per Package. A unit with build is built from " +
			"its context; a unit with image is pulled by digest and relabelled through a one line " +
			"Containerfile so the result carries the same labels and its own image ID.",
		InputSchema:  pkgBuildInputSchema,
		OutputSchema: pkgBuildOutputSchema,
	}, buildHandler(packages))

	mcp.AddTool(s, &mcp.Tool{
		Name: "pkg_import",
		Description: "Wrap something external as a Package folder at path using a running import kit. Writes " +
			"the folder as one commit, does not build. The target folder must not exist yet. Returns " +
			"not-found when no running kit accepts the source and conflict when more than one does. A " +
			"member importing under /org does not fail: the call is queued for an admin the way fs_write " +
			"is, and the queued problem carries the approval id.",
		InputSchema:  pkgImportInputSchema,
		OutputSchema: pkgImportOutputSchema,
	}, importHandler(packages))

	mcp.AddTool(s, &mcp.Tool{
		Name: "pkg_list",
		Description: "Packages visible to the caller with their latest built digest, if any. Walks the " +
			"visible folders under /org and the caller's home and keeps those whose manifest carries a " +
			"deploy block.",
		InputSchema:  pkgListInputSchema,
		OutputSchema: pkgListOutputSchema,
	}, packagesHandler(packages))

	mcp.AddTool(s, &mcp.Tool{
		Name:         "pkg_inspect",
		Description:  "Manifest, build history and provenance of one Package. Builds are newest first.",
		InputSchema:  pkgInspectInputSchema,
		OutputSchema: pkgInspectOutputSchema,
	}, inspectHandler(packages))
}

// RegisterProcesses adds the proc family to an existing MCP server. The bridge
// may be nil, in which case a Process with expose: mcp runs but publishes no
// tools.
func RegisterProcesses(s *mcp.Server, processes *proc.Service, b *bridge.Bridge) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "proc_run",
		Description: "Start a Process from a Package digest, or converge an existing one with the same name " +
			"to that digest. Idempotent: same name and digest returns the running Process, a different " +
			"digest replaces it. The container is named kitbash-<package>-<name> and labelled kitbash.id, " +
			"kitbash.user, kitbash.package, kitbash.name and kitbash.digest. With expose: mcp the manifest's " +
			"tools join the caller's surface. A Process is identified by its Package path, so a name held " +
			"by a Process of another Package is a conflict, and so is a tool name another running Process " +
			"already answers. Version 1 runs the first container unit. Before the container starts the " +
			"Process is registered with kitbashd, which mints its Telemetry token; the container receives " +
			"KITBASH_TELEMETRY_ENDPOINT, KITBASH_TELEMETRY_TOKEN, KITBASH_PROCESS, KITBASH_PACKAGE and " +
			"KITBASH_USER. A Process whose manifest declares subscriptions: [telemetry] with expose: http " +
			"and a port is registered as a fan out subscriber.",
		InputSchema:  procRunInputSchema,
		OutputSchema: procRunOutputSchema,
	}, runHandler(processes, b))

	mcp.AddTool(s, &mcp.Tool{
		Name:         "proc_list",
		Description:  "Processes owned by the caller, running or stopped.",
		InputSchema:  procListInputSchema,
		OutputSchema: procListOutputSchema,
	}, processesHandler(processes))

	mcp.AddTool(s, &mcp.Tool{
		Name: "proc_stop",
		Description: "Stop a Process. It stays known and can be run again with proc_run. Its tools leave " +
			"the surface.",
		InputSchema:  procStopInputSchema,
		OutputSchema: procStopOutputSchema,
	}, stopHandler(processes, b))

	mcp.AddTool(s, &mcp.Tool{
		Name: "proc_logs",
		Description: "Recent stdout and stderr of a Process. Structured logs are in Telemetry; this is the " +
			"raw stream.",
		InputSchema:  procLogsInputSchema,
		OutputSchema: procLogsOutputSchema,
	}, logsHandler(processes))
}

// RegisterTelemetry adds the tel family to an existing MCP server. Both tools
// are forwarded to kitbashd over its unix socket, which is where the store
// lives; kitbash-mcp keeps no telemetry of its own.
func RegisterTelemetry(s *mcp.Server, client *telemetry.Client) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "tel_query",
		Description: "Query spans, logs or metrics by attributes and time range, newest first. A member sees " +
			"only their own records: user defaults to the caller and naming another member is not-permitted. " +
			"An admin may name any member or omit user for the whole machine. since defaults to 24 hours ago, " +
			"until to now. Metrics are accepted by the store but no producer emits them before M4, so that " +
			"signal returns an empty page.",
		InputSchema:  telQueryInputSchema,
		OutputSchema: telQueryOutputSchema,
	}, queryHandler(client))

	mcp.AddTool(s, &mcp.Tool{
		Name: "tel_retention",
		Description: "Read the retention window per signal, as a duration such as 720h or 30d. With set, an " +
			"admin changes one or more windows; kitbashd deletes older records on its next sweep. Defaults " +
			"are traces 30d, logs 14d, metrics 30d.",
		InputSchema:  telRetentionInputSchema,
		OutputSchema: telRetentionOutputSchema,
	}, retentionHandler(client))
}

// RegisterUsers adds the users family to an existing MCP server. Creating a
// member is root's work, so every tool here is forwarded to kitbashd over its
// socket and the daemon decides from the peer credentials whether the caller
// may do it, see PLAN.md section 4.5.
func RegisterUsers(s *mcp.Server, client *telemetry.Client) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "users_me",
		Description: "Who am I and what groups am I in. Answered by kitbashd from the socket's peer " +
			"credentials.",
		InputSchema:  usersMeInputSchema,
		OutputSchema: usersMeOutputSchema,
	}, meHandler(client))

	mcp.AddTool(s, &mcp.Tool{
		Name: "users_create",
		Description: "Create a member with an SSH public key, a private home and a subordinate id range, " +
			"through kitbashd. Admin only. The member can connect immediately; their home is a root for " +
			"Files and starts empty.",
		InputSchema:  usersCreateInputSchema,
		OutputSchema: usersCreateOutputSchema,
	}, createUserHandler(client))

	mcp.AddTool(s, &mcp.Tool{
		Name:         "users_list",
		Description:  "All members with uid, admin flag, key count and running Process count. Admin only.",
		InputSchema:  usersListInputSchema,
		OutputSchema: usersListOutputSchema,
	}, listUsersHandler(client))

	mcp.AddTool(s, &mcp.Tool{
		Name: "users_add_key",
		Description: "Add an SSH public key to a member. Admin only. A key already present is not added " +
			"twice.",
		InputSchema:  usersAddKeyInputSchema,
		OutputSchema: usersAddKeyOutputSchema,
	}, addKeyHandler(client))

	mcp.AddTool(s, &mcp.Tool{
		Name: "users_remove",
		Description: "Remove a member. Admin only. Their Processes stop and are unregistered, their " +
			"sessions end, their home is archived under /org/.archive/<name> where the surface cannot see " +
			"it, and the account is deleted. An admin cannot remove themselves or the last admin.",
		InputSchema:  usersRemoveInputSchema,
		OutputSchema: usersRemoveOutputSchema,
	}, removeUserHandler(client))
}

// RegisterApprovals adds the approvals family to an existing MCP server.
// Listing and rejecting are forwarded to kitbashd; approving claims the
// approval there and then runs the tool in this session, which is what makes
// the write land as the admin's Linux user, see approve.go.
func RegisterApprovals(s *mcp.Server, client *telemetry.Client, files *fs.Service, packages *pkg.Service) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "approvals_list",
		Description: "Approvals by state. Members see their own, admins see all. An executed approval " +
			"carries the tool's result, a normal output or a problem.",
		InputSchema:  approvalsListInputSchema,
		OutputSchema: approvalsListOutputSchema,
	}, listApprovalsHandler(client))

	mcp.AddTool(s, &mcp.Tool{
		Name: "approvals_approve",
		Description: "Approve and execute a queued operation. Admin only. The tool runs in the admin's " +
			"session as the admin's Linux user; a commit is authored by the requester with an Approved-by " +
			"trailer naming the admin. The result, a normal output or a problem, is returned here and " +
			"stored on the approval for the requester.",
		InputSchema:  approvalsApproveInputSchema,
		OutputSchema: approvalsApproveOutputSchema,
	}, approveHandler(client, files, packages))

	mcp.AddTool(s, &mcp.Tool{
		Name:         "approvals_reject",
		Description:  "Reject a queued operation with a reason the requester will see. Admin only.",
		InputSchema:  approvalsRejectInputSchema,
		OutputSchema: approvalsRejectOutputSchema,
	}, rejectHandler(client))
}

type pathInput struct {
	Path string `json:"path"`
}

type importInput struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}

type emptyInput struct{}

type runInput struct {
	Package string `json:"package"`
	Digest  string `json:"digest,omitempty"`
	Name    string `json:"name,omitempty"`
}

type idInput struct {
	ID string `json:"id"`
}

type logsInput struct {
	ID    string `json:"id"`
	Lines int    `json:"lines,omitempty"`
}

type retentionInput struct {
	Set json.RawMessage `json:"set,omitempty"`
}

type keyInput struct {
	Name   string `json:"name"`
	SSHKey string `json:"sshKey"`
}

type nameInput struct {
	Name string `json:"name"`
}

type stateInput struct {
	State string `json:"state,omitempty"`
}

type approveInput struct {
	ID   string `json:"id"`
	Note string `json:"note,omitempty"`
}

type rejectInput struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

func buildHandler(packages *pkg.Service) mcp.ToolHandlerFor[pathInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in pathInput) (*mcp.CallToolResult, any, error) {
		out, prob := packages.Build(ctx, in.Path)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return structuredResult(out)
	}
}

func importHandler(packages *pkg.Service) mcp.ToolHandlerFor[importInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in importInput) (*mcp.CallToolResult, any, error) {
		out, prob := packages.Import(ctx, pkg.ImportRequest{Path: in.Path, Source: in.Source})
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return structuredResult(out)
	}
}

// The users family is forwarded to kitbashd as it arrived and answered with
// the daemon's body as it arrived: the tool's schemas and the daemon's are the
// same ones, so reshaping either side could only lose something. A problem
// kitbashd returns reaches the agent unchanged, slug and fix and all.
func meHandler(client *telemetry.Client) mcp.ToolHandlerFor[emptyInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
		out, prob := client.Me(ctx)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return rawResult(out)
	}
}

func createUserHandler(client *telemetry.Client) mcp.ToolHandlerFor[json.RawMessage, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in json.RawMessage) (*mcp.CallToolResult, any, error) {
		out, prob := client.CreateUser(ctx, in)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return rawResult(out)
	}
}

func listUsersHandler(client *telemetry.Client) mcp.ToolHandlerFor[emptyInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
		out, prob := client.ListUsers(ctx)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return rawResult(out)
	}
}

func addKeyHandler(client *telemetry.Client) mcp.ToolHandlerFor[keyInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in keyInput) (*mcp.CallToolResult, any, error) {
		out, prob := client.AddKey(ctx, in.Name, in.SSHKey)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return rawResult(out)
	}
}

func removeUserHandler(client *telemetry.Client) mcp.ToolHandlerFor[nameInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in nameInput) (*mcp.CallToolResult, any, error) {
		out, prob := client.RemoveUser(ctx, in.Name)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return rawResult(out)
	}
}

func listApprovalsHandler(client *telemetry.Client) mcp.ToolHandlerFor[stateInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in stateInput) (*mcp.CallToolResult, any, error) {
		out, prob := client.ListApprovals(ctx, in.State)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return rawResult(out)
	}
}

func rejectHandler(client *telemetry.Client) mcp.ToolHandlerFor[rejectInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in rejectInput) (*mcp.CallToolResult, any, error) {
		out, prob := client.RejectApproval(ctx, in.ID, in.Reason)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return rawResult(out)
	}
}

func packagesHandler(packages *pkg.Service) mcp.ToolHandlerFor[emptyInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
		out, prob := packages.List(ctx)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return structuredResult(out)
	}
}

func inspectHandler(packages *pkg.Service) mcp.ToolHandlerFor[pathInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in pathInput) (*mcp.CallToolResult, any, error) {
		out, prob := packages.Inspect(ctx, in.Path)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return structuredResult(out)
	}
}

func runHandler(processes *proc.Service, b *bridge.Bridge) mcp.ToolHandlerFor[runInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in runInput) (*mcp.CallToolResult, any, error) {
		out, prob := processes.Run(ctx, in.Package, in.Digest, in.Name)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		if b != nil {
			// A run that replaced an older Process takes its tools off the
			// surface before it puts its own on.
			b.Remove(out.Replaced)
			prob := b.Add(ctx, out)
			// The Process reports the tools that are on the surface, which is
			// what the bridge published and not what the manifest declares.
			out.Tools = b.Tools(out.ID)
			if prob != nil {
				// The Process is running; its tools are not all on the
				// surface, and the caller is the one who can resolve that.
				return errorResult(prob), nil, nil
			}
		} else {
			out.Tools = nil
		}
		return structuredResult(out)
	}
}

func processesHandler(processes *proc.Service) mcp.ToolHandlerFor[emptyInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
		out, prob := processes.List(ctx)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return structuredResult(out)
	}
}

func stopHandler(processes *proc.Service, b *bridge.Bridge) mcp.ToolHandlerFor[idInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in idInput) (*mcp.CallToolResult, any, error) {
		out, prob := processes.Stop(ctx, in.ID)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		if b != nil {
			b.Remove(out.Process)
		}
		return structuredResult(out)
	}
}

// queryHandler forwards the input to kitbashd as it arrived. The tool's input
// schema and the daemon's request schema are the same one, so reshaping it
// here could only lose something.
func queryHandler(client *telemetry.Client) mcp.ToolHandlerFor[json.RawMessage, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in json.RawMessage) (*mcp.CallToolResult, any, error) {
		out, prob := client.Query(ctx, in)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return rawResult(out)
	}
}

func retentionHandler(client *telemetry.Client) mcp.ToolHandlerFor[retentionInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in retentionInput) (*mcp.CallToolResult, any, error) {
		out, prob := client.Retention(ctx, in.Set)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return rawResult(out)
	}
}

func logsHandler(processes *proc.Service) mcp.ToolHandlerFor[logsInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in logsInput) (*mcp.CallToolResult, any, error) {
		out, prob := processes.Logs(ctx, in.ID, in.Lines)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return structuredResult(out)
	}
}
