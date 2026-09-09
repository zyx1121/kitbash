// Package problem implements the structured errors described in
// spec/mcp-surface.yaml: RFC 9457 Problem Details with a fix extension.
package problem

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
)

// Base is the prefix of every error type URI.
const Base = "https://kitbash.zyx.tw/errors/"

// Slugs of the error classes the fs family can return.
const (
	SlugNotFound        = "not-found"
	SlugNotVisible      = "not-visible"
	SlugNotPermitted    = "not-permitted"
	SlugBadRequest      = "bad-request"
	SlugInvalidPath     = "invalid-path"
	SlugInvalidManifest = "invalid-manifest"
	SlugUnsupported     = "unsupported-media-type"
	SlugTooLarge        = "too-large"
	SlugConflict        = "conflict"
	SlugQueued          = "queued"
	SlugInternal        = "internal"
)

// Problem is an RFC 9457 Problem Details object.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance,omitempty"`
	Fix      string `json:"fix,omitempty"`
}

// Error implements the error interface.
func (p *Problem) Error() string { return p.Detail }

// Slug is the last segment of the type URI.
func (p *Problem) Slug() string {
	if len(p.Type) <= len(Base) {
		return ""
	}
	return p.Type[len(Base):]
}

// JSON renders the problem as the single text content of a tool error result.
func (p *Problem) JSON() string {
	b, err := json.Marshal(p)
	if err != nil {
		return fmt.Sprintf(`{"type":%q,"title":"Internal error","status":500,"detail":%q}`,
			Base+SlugInternal, err.Error())
	}
	return string(b)
}

func newProblem(slug, title string, status int, instance, detail, fix string) *Problem {
	return &Problem{
		Type:     Base + slug,
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: instance,
		Fix:      fix,
	}
}

// NotFound reports a path that does not exist.
func NotFound(instance, detail string) *Problem {
	return newProblem(SlugNotFound, "Not found", http.StatusNotFound, instance, detail,
		"Call fs_list on the parent folder to see what exists.")
}

// NotFoundFix is NotFound with advice specific to what is missing.
func NotFoundFix(instance, detail, fix string) *Problem {
	p := NotFound(instance, detail)
	p.Fix = fix
	return p
}

// NotVisible reports a folder without a usable kitbash.yaml. It is not found as
// far as the MCP surface is concerned. An empty fix falls back to the advice
// that makes the folder visible.
func NotVisible(instance, detail, fix string) *Problem {
	if fix == "" {
		fix = "Write a kitbash.yaml with name and description into the folder to make it visible."
	}
	return newProblem(SlugNotVisible, "Not visible", http.StatusNotFound, instance, detail, fix)
}

// NotPermitted reports an operation the host refuses. An empty fix falls back
// to the access advice.
func NotPermitted(instance, detail, fix string) *Problem {
	if fix == "" {
		fix = "Ask an administrator for access, or work inside your own home folder."
	}
	return newProblem(SlugNotPermitted, "Not permitted", http.StatusForbidden, instance, detail, fix)
}

// NotAuthenticated reports a request that carried no identity kitbashd could
// use: the Process receiver's bearer token is missing, unknown or revoked. The
// slug stays not-permitted, so a client matches one class of refusal however
// it was refused, and the status is 401 because a different token would change
// the answer.
func NotAuthenticated(instance, detail, fix string) *Problem {
	if fix == "" {
		fix = "Send the token kitbashd minted for this Process as an Authorization bearer header."
	}
	return newProblem(SlugNotPermitted, "Not permitted", http.StatusUnauthorized, instance, detail, fix)
}

// BadRequest reports malformed input the tool's schema does not catch, such as
// content that is not base64.
func BadRequest(instance, detail, fix string) *Problem {
	if fix == "" {
		fix = "Send arguments that match the tool's input schema."
	}
	return newProblem(SlugBadRequest, "Bad request", http.StatusBadRequest, instance, detail, fix)
}

// UnsupportedMediaType reports a file kitbash cannot render in this milestone.
func UnsupportedMediaType(instance, detail string) *Problem {
	return newProblem(SlugUnsupported, "Unsupported media type", http.StatusUnsupportedMediaType, instance, detail,
		"Read this file from a Package that understands its format.")
}

// InvalidPath reports a path outside the roots or containing a parent reference.
func InvalidPath(instance, detail string) *Problem {
	return newProblem(SlugInvalidPath, "Invalid path", http.StatusBadRequest, instance, detail,
		"Use an absolute path under /org or your home folder, without any .. segment.")
}

// InvalidPathFix is InvalidPath with advice specific to the rule that was
// broken.
func InvalidPathFix(instance, detail, fix string) *Problem {
	p := InvalidPath(instance, detail)
	p.Fix = fix
	return p
}

// InvalidManifest reports a kitbash.yaml that fails spec/manifest.schema.json.
func InvalidManifest(instance, detail string) *Problem {
	return newProblem(SlugInvalidManifest, "Invalid manifest", http.StatusUnprocessableEntity, instance, detail,
		"Fix the manifest so it validates against spec/manifest.schema.json, then write it again.")
}

// InvalidManifestFix is InvalidManifest with advice specific to the part of
// the manifest that is wrong.
func InvalidManifestFix(instance, detail, fix string) *Problem {
	p := InvalidManifest(instance, detail)
	p.Fix = fix
	return p
}

// TooLarge reports content beyond the size the surface carries in one call.
func TooLarge(instance, detail, fix string) *Problem {
	if fix == "" {
		fix = "Read the file in windows with offset and limit."
	}
	return newProblem(SlugTooLarge, "Too large", http.StatusRequestEntityTooLarge, instance, detail, fix)
}

// Conflict reports a write that raced another commit.
func Conflict(instance, detail string) *Problem {
	return newProblem(SlugConflict, "Conflict", http.StatusConflict, instance, detail,
		"Read the file again, merge the change, then write with the new expectedSha.")
}

// ConflictFix is Conflict with advice specific to the state that clashed.
func ConflictFix(instance, detail, fix string) *Problem {
	p := Conflict(instance, detail)
	p.Fix = fix
	return p
}

// TooManySessions reports a Process asking for one more MCP session than it
// may hold at once, see mcp_for_processes.limits in spec/kitbashd-api.yaml.
// The slug stays conflict, so a client matches one class of refusal however it
// was refused, and the status is 429 because the same request works once a
// session ends.
func TooManySessions(instance, detail, fix string) *Problem {
	if fix == "" {
		fix = "End a session with DELETE /mcp before opening another one."
	}
	return TooMany("Too many sessions", instance, detail, fix)
}

// TooMany reports a caller asking for more of something than kitbash accepts
// in a window, whatever that something is. The slug stays conflict, so a
// client matches one class of refusal however it was refused, and the status
// is 429 because the same request works later.
func TooMany(title, instance, detail, fix string) *Problem {
	return newProblem(SlugConflict, title, http.StatusTooManyRequests, instance, detail, fix)
}

// Queued reports a call that was not run but put in the approval queue, which
// is what a member's write under /org is, see PLAN.md section 2.1. It is the
// one problem that is not a failure: the status is 202, the instance is the
// approval id, and the outcome lands on the approval once an admin decides.
func Queued(id, detail string) *Problem {
	return newProblem(SlugQueued, "Queued", http.StatusAccepted, id, detail,
		fmt.Sprintf("Ask an admin to run approvals_approve %s, or call approvals_list to follow it.", id))
}

// logger writes the causes of internal errors where the operator can read them.
var logger = log.New(os.Stderr, "kitbash: ", log.LstdFlags)

// hook receives every internal cause besides the server log, so an admin can
// read it through Telemetry rather than on the host, see PLAN.md section 2.4.
// It is set once at start up by whoever can record a cause: kitbash-mcp's
// telemetry provider, which exports it, and kitbashd, which writes it to its
// own store.
var hook struct {
	mu sync.RWMutex
	fn func(instance, cause string)
}

// OnInternal sets that hook, replacing whatever was there. A nil argument
// removes it, which is how a process that recorded causes stops before its
// recorder is closed.
func OnInternal(fn func(instance, cause string)) {
	hook.mu.Lock()
	defer hook.mu.Unlock()
	hook.fn = fn
}

// record hands one cause to the hook, if there is one.
func record(instance, cause string) {
	hook.mu.RLock()
	fn := hook.fn
	hook.mu.RUnlock()
	if fn != nil {
		fn(instance, cause)
	}
}

// Internal reports a failure inside kitbash itself. The cause goes to the
// server log and to the hook, never to the agent: it carries host paths, git
// output and other detail the caller has no business seeing and cannot act on.
// The instance is what the agent is given, so a member quotes it to an admin
// and the admin finds the cause with tel_query.
func Internal(instance, cause, fix string) *Problem {
	if fix == "" {
		fix = "Retry the call. If it keeps failing, ask an administrator to read the cause with tel_query."
	}
	logger.Printf("internal error at %s: %s", instance, cause)
	record(instance, cause)
	return newProblem(SlugInternal, "Internal error", http.StatusInternalServerError, instance,
		"kitbash could not complete this call; the cause is recorded for administrators", fix)
}

// InternalDetail is Internal with a detail the caller can act on. The failure
// is still inside kitbash or inside a Package it ran, so the cause goes where
// only an admin reads it, but saying which of the two broke helps the agent
// decide what to do next.
func InternalDetail(instance, cause, detail, fix string) *Problem {
	p := Internal(instance, cause, fix)
	p.Detail = detail
	return p
}
