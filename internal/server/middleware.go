package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/problem"
)

// callToolMethod is the only method whose results the surface rewrites.
const callToolMethod = "tools/call"

// problemGuard is the last line of the promise that every failure is
// structured. The SDK validates arguments against the tool's input schema
// before a handler runs and reports a violation as a bare string, and a panic
// inside a handler would escape the same way, so both are turned into RFC 9457
// problem details here.
func problemGuard(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (result mcp.Result, err error) {
		if method != callToolMethod {
			return next(ctx, method, req)
		}
		name := toolName(req)
		defer func() {
			if r := recover(); r != nil {
				result = errorResult(problem.Internal(name, fmt.Sprintf("panic in %s: %v", name, r), ""))
				err = nil
			}
		}()
		result, err = next(ctx, method, req)
		if err != nil {
			return result, err
		}
		call, isCall := result.(*mcp.CallToolResult)
		if !isCall || !call.IsError {
			return result, err
		}
		return rewriteBareError(name, call), nil
	}
}

// neverRepeated are the tools whose arguments must not be quoted back. The
// SDK reports a schema violation by printing the value it refused, which for
// secrets_set is the secret itself, and PLAN.md section 2.3 says no problem
// detail ever quotes a value. The violation is still reported, in the shape
// the schema publishes, and the caller is the one who sent the value.
var neverRepeated = map[string]bool{"secrets_set": true}

// rewriteBareError replaces an error result that is not already problem
// details, which is how the SDK reports a schema violation.
func rewriteBareError(name string, call *mcp.CallToolResult) *mcp.CallToolResult {
	if len(call.Content) != 1 {
		return call
	}
	text, isText := call.Content[0].(*mcp.TextContent)
	if !isText || isProblem(text.Text) {
		return call
	}
	detail := strings.TrimSpace(text.Text)
	if detail == "" {
		detail = "the arguments were rejected"
	}
	if neverRepeated[name] {
		detail = "the arguments do not match the input schema of " + name
	}
	return errorResult(problem.BadRequest(name, detail,
		"Send arguments that match the tool's input schema, which tools/list publishes."))
}

// isProblem reports whether a text block already carries problem details.
func isProblem(text string) bool {
	var p problem.Problem
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		return false
	}
	return strings.HasPrefix(p.Type, problem.Base)
}

// toolName reads the tool a call was for, for use as the problem's instance.
func toolName(req mcp.Request) string {
	call, ok := req.(*mcp.CallToolRequest)
	if !ok || call.Params == nil {
		return ""
	}
	return call.Params.Name
}
