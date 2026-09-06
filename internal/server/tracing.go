package server

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/bridge"
	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
)

// telFamily is the prefix of the tel tools, whose arguments describe records
// other calls left rather than anything this call touches.
const telFamily = "tel_"

// tracing opens the span of every tools/call, which is the invariant in
// PLAN.md section 2.6 that there is no untraced path. It is added after
// problemGuard so it wraps it: the span is open before the SDK validates the
// arguments, before the handler runs and before a panic is turned into a
// problem, and it closes after all three.
//
// The context the handler receives carries the span, so a service adds
// kitbash.package, kitbash.process and kitbash.path to the same span with the
// telemetry helpers rather than with the OpenTelemetry SDK.
func tracing(p *telemetry.Provider, files *fs.Service, b *bridge.Bridge) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != callToolMethod {
				return next(ctx, method, req)
			}
			name := toolName(req)
			ctx, span := p.StartTool(ctx, name, files.User())
			if span == nil {
				// No provider: a server built without Telemetry serves the
				// same surface, it just records nothing.
				return next(ctx, method, req)
			}
			defer span.End()
			// A proxied Package tool is answered by a Process, and the bridge
			// is the only thing that knows which one.
			if path, process, proxied := b.Owner(name); proxied {
				span.SetPackage(path)
				span.SetProcess(process)
			} else if !strings.HasPrefix(name, telFamily) {
				// A built in tool names the Files path it is about in its
				// arguments. A Package tool's path means whatever the Package
				// decided it means, and a tel tool's is a filter over other
				// calls rather than a path this call touched, so neither is
				// read here.
				span.SetPath(callPath(req))
			}

			result, err := next(ctx, method, req)
			if err != nil {
				// A method that failed outside a tool result, such as a call
				// to a tool that is not on the surface.
				span.Fail("", err.Error())
				return result, err
			}
			call, isCall := result.(*mcp.CallToolResult)
			if !isCall || !call.IsError {
				span.OK()
				return result, err
			}
			// problemGuard has already made every failure structured, so the
			// slug and the title are there to be read.
			if prob := resultProblem(call); prob != nil {
				span.Fail(prob.Slug(), prob.Title)
			} else {
				span.Fail("", "the call failed")
			}
			return result, err
		}
	}
}

// resultProblem reads the problem details a failed call carries.
func resultProblem(call *mcp.CallToolResult) *problem.Problem {
	if len(call.Content) != 1 {
		return nil
	}
	text, isText := call.Content[0].(*mcp.TextContent)
	if !isText || !isProblem(text.Text) {
		return nil
	}
	var p problem.Problem
	if err := json.Unmarshal([]byte(text.Text), &p); err != nil {
		return nil
	}
	return &p
}

// callPath is the path argument of a call, empty when the tool takes none.
// The arguments are read as they arrived: the SDK has not yet validated them
// when this runs, so anything that does not decode is simply not a path, and a
// path longer than an attribute may be is cut here rather than sent and
// refused. A caller does not get to choose the size of a telemetry record.
func callPath(req mcp.Request) string {
	call, ok := req.(*mcp.CallToolRequest)
	if !ok || call.Params == nil || len(call.Params.Arguments) == 0 {
		return ""
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(call.Params.Arguments, &args); err != nil {
		return ""
	}
	if len(args.Path) > telemetry.AttributeValueLimit {
		return args.Path[:telemetry.AttributeValueLimit]
	}
	return args.Path
}
