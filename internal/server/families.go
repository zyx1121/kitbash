package server

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/bridge"
	"github.com/zyx1121/kitbash/internal/pkg"
	"github.com/zyx1121/kitbash/internal/proc"
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
			"a conflict. Version 1 supports one container unit with a build context per Package.",
		InputSchema:  pkgBuildInputSchema,
		OutputSchema: pkgBuildOutputSchema,
	}, buildHandler(packages))

	mcp.AddTool(s, &mcp.Tool{
		Name: "pkg_import",
		Description: "Wrap something external as a Package folder at path using a running import kit. Writes " +
			"the folder as one commit, does not build. The target folder must not exist yet. Returns " +
			"not-found when no running kit accepts the source and conflict when more than one does.",
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
			"already answers. Version 1 runs the first container unit.",
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
		out, prob := packages.Import(ctx, in.Path, in.Source)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return structuredResult(out)
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

func logsHandler(processes *proc.Service) mcp.ToolHandlerFor[logsInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in logsInput) (*mcp.CallToolResult, any, error) {
		out, prob := processes.Logs(ctx, in.ID, in.Lines)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return structuredResult(out)
	}
}
