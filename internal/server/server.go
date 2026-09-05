// Package server wires the fs family onto an MCP server. kitbash-mcp serves it
// over stdio today; kitbashd will serve the same tools in M2.
package server

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/problem"
)

// Name is the MCP server name every client sees.
const Name = "kitbash"

// New builds the MCP server for one caller.
func New(version string, files *fs.Service) *mcp.Server {
	if version == "" {
		version = "dev"
	}
	s := mcp.NewServer(&mcp.Implementation{
		Name:        Name,
		Version:     version,
		Title:       "kitbash",
		Description: "Files, Packages, Processes and Telemetry for agents. M1 serves the fs family.",
	}, nil)
	s.AddReceivingMiddleware(problemGuard)
	Register(s, files)
	return s
}

// Register adds the fs family to an existing MCP server.
func Register(s *mcp.Server, files *fs.Service) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "fs_list",
		Description: "List what is visible at a path. With no path, returns the visible top level folders " +
			"across /org and the caller's home. Inside a folder, returns visible subfolders (name and " +
			"description only, no contents) and the files directly in it. Folders without a manifest are omitted.",
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
		Description: "Create or replace one file and commit it to the enclosing top level repository, " +
			"attributed to the caller. Writing kitbash.yaml into a new folder is how a folder becomes " +
			"visible; the manifest is validated against spec/manifest.schema.json before commit. Binary " +
			"content is base64. Pass expectedSha to refuse the write if the file changed since it was read.",
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

type writeInput struct {
	Path          string  `json:"path"`
	Content       *string `json:"content,omitempty"`
	ContentBase64 *string `json:"contentBase64,omitempty"`
	Message       string  `json:"message"`
	ExpectedSha   string  `json:"expectedSha,omitempty"`
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

// errorResult renders a problem as the one text content of a failed call,
// never as a bare error string.
func errorResult(p *problem.Problem) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: p.JSON()}},
	}
}
