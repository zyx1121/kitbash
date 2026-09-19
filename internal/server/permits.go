package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/bridge"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
)

// listToolsMethod is the method whose result the permits guard filters.
const listToolsMethod = "tools/list"

// PermitsFix is what an agent is told to do about a refusal: the answer is in
// the Package's manifest, not in this session, and the member who runs the
// Process is the one who can change it.
const (
	PermitsFix     = "Declare the tool in provides.permits.tools of this Package's kitbash.yaml."
	PermitsPathFix = "Declare the path prefix in provides.permits.paths of this Package's kitbash.yaml."
)

// packageTools is the predicate the reserved word packages is read with: the
// bridge published the tools of the owner's running Processes, so it is the
// one that can say which names are theirs. A session with no bridge has no
// Package tools, and packages admits nothing there.
func packageTools(b *bridge.Bridge) manifest.PackageTool {
	if b == nil {
		return nil
	}
	return func(name string) bool {
		_, _, ok := b.Owner(name)
		return ok
	}
}

// pathArgument names, for each built in tool that acts on a Files path, the
// argument that carries it. A tool that takes no path is governed by the
// permitted tool names alone: proc_list and tel_query name nothing a prefix
// could cover.
//
// Package tools are not here on purpose. Their arguments are the Package's own
// schema, so what a Process may ask of another Process is decided by whether
// the tool is permitted at all.
var pathArgument = map[string]string{
	"fs_list":     "path",
	"fs_read":     "path",
	"fs_write":    "path",
	"fs_history":  "path",
	"pkg_build":   "path",
	"pkg_import":  "path",
	"pkg_inspect": "path",
	"proc_run":    "package",
}

// PermitsFromEnv reads the permits block of the Process this session serves.
// It answers nil for a member's own SSH session, which is narrowed by nothing:
// the block is honoured only beside KITBASH_CALLER, and only kitbashd sets
// that, so exporting KITBASH_PERMITS by hand in a member's shell changes
// neither what they may call nor what they may reach.
//
// A block that cannot be read is an error rather than an empty surface. The
// session does not start, kitbashd reports it, and the Process is told its
// call failed; serving the owner's whole surface because a glob had a typo is
// the one answer this must never give.
func PermitsFromEnv() (*manifest.Permits, error) {
	if telemetry.Caller() == "" {
		return nil, nil
	}
	value, set := os.LookupEnv(manifest.EnvPermits)
	if !set {
		// A session of a Process with no variable is a Process permitted
		// nothing, not a Process permitted everything. A daemon of an earlier
		// release sets no variable, and so does this one for the minutes
		// between deploy/install.sh replacing the binaries and restarting
		// kitbashd; an empty surface there is a kit that stops working until
		// the daemon is back, and the other answer is a kit with its owner's
		// whole surface, which is the thing permits exist to prevent.
		return &manifest.Permits{}, nil
	}
	permits, err := manifest.ParsePermits([]byte(value))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", manifest.EnvPermits, err)
	}
	return &permits, nil
}

// permitsGuard is what narrowing a Process's surface is made of, see PLAN.md
// section 2.3. It filters tools/list to the permitted names and refuses a
// tools/call that names something else, or that names a path outside every
// permitted prefix, before the handler runs.
//
// Filtering the listing and refusing the call are one rule read twice, and
// both are needed: the tools are registered on this server whether or not the
// Process may call them, because the bridge publishes a Process's tools as it
// starts it and the surface is one set. What tools/list leaves out is what
// tools/call refuses.
func permitsGuard(permits manifest.Permits, isPackage manifest.PackageTool) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case listToolsMethod:
				result, err := next(ctx, method, req)
				if err != nil {
					return result, err
				}
				list, ok := result.(*mcp.ListToolsResult)
				if !ok {
					return result, nil
				}
				return permittedTools(permits, list, isPackage), nil
			case callToolMethod:
				name := toolName(req)
				if !permits.Match(name, isPackage) {
					return errorResult(problem.NotPermitted(name,
						fmt.Sprintf("this Process may not call %s", name), PermitsFix)), nil
				}
				if prob := permittedPath(permits, name, req); prob != nil {
					return errorResult(prob), nil
				}
				return next(ctx, method, req)
			}
			return next(ctx, method, req)
		}
	}
}

// permittedTools is the listing with everything this Process may not call
// taken out. The page is filtered rather than refilled: a cursor the SDK
// answered still names the same place in the whole set, so a client paging
// through reads every permitted tool and no others.
func permittedTools(permits manifest.Permits, list *mcp.ListToolsResult, isPackage manifest.PackageTool) *mcp.ListToolsResult {
	kept := make([]*mcp.Tool, 0, len(list.Tools))
	for _, tool := range list.Tools {
		if tool != nil && permits.Match(tool.Name, isPackage) {
			kept = append(kept, tool)
		}
	}
	filtered := *list
	filtered.Tools = kept
	return &filtered
}

// permittedPath refuses a call whose path argument is outside every permitted
// prefix. A Process that permits tools and no prefix may call nothing that
// takes a path at all: it declared what it wanted to do and not where, and
// the surface does not guess a root for it.
//
// A call that names no path is left to the tool: fs_list without one lists the
// caller's roots, which is names and descriptions, and every path the Process
// then asks about comes back through here.
func permittedPath(permits manifest.Permits, name string, req mcp.Request) *problem.Problem {
	argument, takesPath := pathArgument[name]
	if !takesPath {
		return nil
	}
	if !permits.AnyPath() {
		return problem.NotPermitted(name,
			fmt.Sprintf("%s acts on a path and this Process is permitted none", name), PermitsPathFix)
	}
	// Every path the call names, not only the one the tool's first argument
	// carries: fs_write takes a list, and a Process permitted one prefix could
	// otherwise write every folder of its owner's home by putting the paths in
	// files, where nothing would look at them.
	for _, path := range append([]string{argumentString(req, argument)}, listedPaths(req)...) {
		if path == "" {
			continue
		}
		if !permits.Allows(path) {
			return problem.NotPermitted(path,
				fmt.Sprintf("this Process may not name %s", path), PermitsPathFix)
		}
	}
	return nil
}

// listedPaths are the paths in the files argument of an fs_write, empty for
// every other call. They are read as they arrived, like every other argument
// here: an entry that does not decode is not a path this guard can clear, and
// the tool refuses the call on its own terms afterwards.
func listedPaths(req mcp.Request) []string {
	call, ok := req.(*mcp.CallToolRequest)
	if !ok || call.Params == nil || len(call.Params.Arguments) == 0 {
		return nil
	}
	var arguments struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(call.Params.Arguments, &arguments); err != nil {
		return nil
	}
	paths := make([]string, 0, len(arguments.Files))
	for _, file := range arguments.Files {
		paths = append(paths, file.Path)
	}
	return paths
}

// argumentString reads one string argument of a call, empty when the call
// carries none or carries something else. Arguments that do not match the
// tool's schema are the SDK's to refuse, which it does after this: a value of
// the wrong type is not permitted here either way.
func argumentString(req mcp.Request, name string) string {
	call, ok := req.(*mcp.CallToolRequest)
	if !ok || call.Params == nil || len(call.Params.Arguments) == 0 {
		return ""
	}
	var arguments map[string]json.RawMessage
	if err := json.Unmarshal(call.Params.Arguments, &arguments); err != nil {
		return ""
	}
	raw, held := arguments[name]
	if !held {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}
