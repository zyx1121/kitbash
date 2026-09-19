// Package server wires the tool families onto an MCP server. kitbash-mcp
// serves them over stdio today; kitbashd will serve the same tools from M3.
package server

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/bridge"
	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/pkg"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/proc"
	"github.com/zyx1121/kitbash/internal/telemetry"
)

// Name is the MCP server name every client sees.
const Name = "kitbash"

// Deps are the services the surface is built from. Files is required; the
// others are the M2 and M3 families, and a server built without them serves
// fs only.
type Deps struct {
	Files     *fs.Service
	Packages  *pkg.Service
	Processes *proc.Service
	Bridge    *bridge.Bridge
	Telemetry *telemetry.Provider
	// Permits narrows this session to what a Package declared its Process may
	// call, see PLAN.md section 2.3. It is nil for a member's own session,
	// which is narrowed by nothing; PermitsFromEnv is what reads it for the
	// session of a Process.
	Permits *manifest.Permits
}

// New builds the MCP server for one caller.
func New(version string, deps Deps) *mcp.Server {
	if version == "" {
		version = "dev"
	}
	s := mcp.NewServer(&mcp.Implementation{
		Name:        Name,
		Version:     version,
		Title:       "kitbash",
		Description: "Files, Packages, Processes and Telemetry for agents. M3 serves the fs, pkg, proc and tel families.",
	}, &mcp.ServerOptions{Instructions: Instructions})
	s.AddReceivingMiddleware(problemGuard)
	if deps.Permits != nil {
		// Between the two, so a refusal is still one span: tracing wraps it
		// and problemGuard is behind it, which is where a call this Process
		// may make goes on to fail on its own terms.
		s.AddReceivingMiddleware(permitsGuard(*deps.Permits, packageTools(deps.Bridge)))
	}
	// Middleware added later wraps middleware added earlier, so tracing goes
	// on last: the span is open before the arguments are validated and closes
	// after problemGuard has turned every failure into problem details.
	s.AddReceivingMiddleware(tracing(deps.Telemetry, deps.Files, deps.Bridge))
	if deps.Bridge != nil {
		// The bridge publishes one tool per Package tool on this server, so it
		// has to hold the server before any Process is added.
		deps.Bridge.Attach(s)
	}
	Register(s, deps.Files)
	if deps.Packages != nil {
		RegisterPackages(s, deps.Packages)
	}
	if deps.Processes != nil {
		RegisterProcesses(s, deps.Processes, deps.Bridge)
	}
	if deps.Telemetry != nil {
		client := deps.Telemetry.Client()
		RegisterTelemetry(s, client)
		// The users and approvals families live in kitbashd, so they are on
		// the surface only when this session has a socket to reach it on.
		RegisterUsers(s, client)
		// The secrets family lives in kitbashd too, for a stronger reason than
		// the users family does: the values are root owned files, and this
		// process is the member's.
		RegisterSecrets(s, client)
		// The approvals family takes the block as well: approving executes the
		// queued tool inside its own handler, which the guard never sees, see
		// permittedApproval.
		RegisterApprovals(s, client, deps.Files, deps.Packages, deps.Permits)
	}
	return s
}

// Register adds the fs family to an existing MCP server.
func Register(s *mcp.Server, files *fs.Service) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "fs_list",
		Description: "List what is visible at a path. With no path, returns the visible top level folders " +
			"across /org and the caller's home: one line of path, name and description each, no files. " +
			"Inside a folder, returns its subfolders and the files directly in it, each file as a name and " +
			"a size. A folder is visible when it or a folder above it carries a kitbash.yaml with a name " +
			"and a description, so the folders inside a Package are listed and need no manifest of their own.",
		InputSchema:  listInputSchema,
		OutputSchema: listOutputSchema,
	}, listHandler(files))

	mcp.AddTool(s, &mcp.Tool{
		Name: "fs_read",
		Description: "Read one file. Text media types return text content. image/png and image/jpeg return " +
			"MCP image content. application/pdf returns extracted text. Files larger than 1 MiB return " +
			"too-large; use offset and limit for text. The first content block is the file, the trailing " +
			"text block is the metadata as JSON: path, mediaType, size, sha and truncated.",
		InputSchema: readInputSchema,
	}, readHandler(files))

	mcp.AddTool(s, &mcp.Tool{
		Name: "fs_write",
		Description: "Create or replace files and commit them to the enclosing top level repository, " +
			"attributed to the caller. Either one file in path with content or contentBase64, or up to 64 " +
			"of them in files, all under one top level folder; either way it is one commit, so write every " +
			"file of a Package in one call. Writing kitbash.yaml into a new folder is how a folder becomes " +
			"visible, and the folders inside it need none of their own; the manifest is validated against " +
			"spec/manifest.schema.json before commit. Binary content is base64. Pass expectedSha to refuse " +
			"the write if the file, or for a list the repository, changed since it was read. A list that " +
			"names one path the caller may not write is refused whole and writes nothing. " +
			"A member writing under /org does not fail: the call is queued for an admin and the result is a " +
			"queued problem (202) whose instance is the approval id; approvals_list shows the outcome once " +
			"an admin decides.",
		InputSchema:  writeInputSchema,
		OutputSchema: writeOutputSchema,
	}, writeHandler(files))

	mcp.AddTool(s, &mcp.Tool{
		Name:         "fs_history",
		Description:  "List commits that touched a path, newest first.",
		InputSchema:  historyInputSchema,
		OutputSchema: historyOutputSchema,
	}, historyHandler(files))
}

type listInput struct {
	Path string `json:"path,omitempty"`
}

type readInput struct {
	Path   string `json:"path"`
	Offset *int64 `json:"offset,omitempty"`
	Limit  *int64 `json:"limit,omitempty"`
}

// writeInput is fs_write's input in both of its forms: one file named by path,
// or a list of them in files. The schema refuses a call that carries both, and
// the handler refuses it again, because the input schema is the client's to
// honour and this is the surface's own answer.
type writeInput struct {
	Path          string      `json:"path,omitempty"`
	Content       *string     `json:"content,omitempty"`
	ContentBase64 *string     `json:"contentBase64,omitempty"`
	Files         []writeFile `json:"files,omitempty"`
	Message       string      `json:"message"`
	ExpectedSha   string      `json:"expectedSha,omitempty"`
}

// writeFile is one entry of fs_write's files.
type writeFile struct {
	Path          string  `json:"path"`
	Content       *string `json:"content,omitempty"`
	ContentBase64 *string `json:"contentBase64,omitempty"`
}

// entries turns the list form into what the fs family takes.
func (in writeInput) entries() []fs.WriteFileEntry {
	files := make([]fs.WriteFileEntry, 0, len(in.Files))
	for _, f := range in.Files {
		files = append(files, fs.WriteFileEntry{
			Path:          f.Path,
			Content:       f.Content,
			ContentBase64: f.ContentBase64,
		})
	}
	return files
}

// mixedForms is the refusal of a call that is both forms at once. Which files
// such a call means is a guess, and a write is not a guess.
func mixedForms(in writeInput) *problem.Problem {
	if len(in.Files) == 0 {
		return nil
	}
	if in.Path == "" && in.Content == nil && in.ContentBase64 == nil {
		return nil
	}
	return problem.BadRequest(in.Path,
		"this call carries both files and the one file form, and one write is one of the two",
		"Send path with content, or send files and nothing else.")
}

type historyInput struct {
	Path  string `json:"path"`
	Limit int    `json:"limit,omitempty"`
}

func listHandler(files *fs.Service) mcp.ToolHandlerFor[listInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, any, error) {
		out, prob := files.List(ctx, in.Path)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return structuredResult(out)
	}
}

func readHandler(files *fs.Service) mcp.ToolHandlerFor[readInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, any, error) {
		opts := fs.ReadOptions{}
		if in.Offset != nil {
			opts.Offset = *in.Offset
			opts.Window = true
		}
		if in.Limit != nil {
			opts.Limit = *in.Limit
			opts.Window = true
		}
		out, prob := files.Read(ctx, in.Path, opts)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		meta, err := json.Marshal(out.Meta)
		if err != nil {
			return errorResult(problem.Internal(in.Path, err.Error(), "")), nil, nil
		}
		var content mcp.Content
		if out.Image != nil {
			content = &mcp.ImageContent{Data: out.Image, MIMEType: out.Meta.MediaType}
		} else {
			content = &mcp.TextContent{Text: out.Text}
		}
		// fs_read carries no structured content on purpose: clients prefer it
		// over the content blocks, which would hide the file body behind its
		// metadata. The metadata is the trailing text block instead.
		return &mcp.CallToolResult{
			Content: []mcp.Content{content, &mcp.TextContent{Text: string(meta)}},
		}, nil, nil
	}
}

func writeHandler(files *fs.Service) mcp.ToolHandlerFor[writeInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in writeInput) (*mcp.CallToolResult, any, error) {
		if prob := mixedForms(in); prob != nil {
			return errorResult(prob), nil, nil
		}
		if len(in.Files) > 0 {
			out, prob := files.WriteAll(ctx, fs.WriteAllRequest{
				Files:       in.entries(),
				Message:     in.Message,
				ExpectedSha: in.ExpectedSha,
			})
			if prob != nil {
				return errorResult(prob), nil, nil
			}
			return structuredResult(out)
		}
		out, prob := files.Write(ctx, fs.WriteRequest{
			Path:          in.Path,
			Content:       in.Content,
			ContentBase64: in.ContentBase64,
			Message:       in.Message,
			ExpectedSha:   in.ExpectedSha,
		})
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return structuredResult(out)
	}
}

func historyHandler(files *fs.Service) mcp.ToolHandlerFor[historyInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in historyInput) (*mcp.CallToolResult, any, error) {
		out, prob := files.History(ctx, in.Path, in.Limit)
		if prob != nil {
			return errorResult(prob), nil, nil
		}
		return structuredResult(out)
	}
}

// structuredResult returns one value both as structured content and as the
// JSON text block clients without structured support read.
func structuredResult(value any) (*mcp.CallToolResult, any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return errorResult(problem.Internal("", err.Error(), "")), nil, nil
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(encoded)}},
		StructuredContent: json.RawMessage(encoded),
	}, nil, nil
}

// rawResult is structuredResult for a body that is already JSON and belongs to
// someone else, such as an answer kitbashd composed. It is passed through
// byte for byte rather than decoded and re-encoded here.
func rawResult(body json.RawMessage) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(body)}},
		StructuredContent: body,
	}, nil, nil
}

// errorResult renders a problem as the one text content of a failed call,
// never as a bare error string.
func errorResult(p *problem.Problem) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: p.JSON()}},
	}
}
