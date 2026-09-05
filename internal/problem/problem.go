// Package problem implements the structured errors described in
// spec/mcp-surface.yaml: RFC 9457 Problem Details with a fix extension.
package problem

import (
	"encoding/json"
	"fmt"
	"net/http"
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

// NotVisible reports a folder without a usable kitbash.yaml. It is not found as
// far as the MCP surface is concerned.
func NotVisible(instance, detail string) *Problem {
	return newProblem(SlugNotVisible, "Not visible", http.StatusNotFound, instance, detail,
		"Write a kitbash.yaml with name and description into the folder to make it visible.")
}

// NotPermitted reports an operation the host refuses. An empty fix falls back
// to the access advice.
func NotPermitted(instance, detail, fix string) *Problem {
	if fix == "" {
		fix = "Ask an administrator for access, or work inside your own home folder."
	}
	return newProblem(SlugNotPermitted, "Not permitted", http.StatusForbidden, instance, detail, fix)
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

// InvalidManifest reports a kitbash.yaml that fails spec/manifest.schema.json.
func InvalidManifest(instance, detail string) *Problem {
	return newProblem(SlugInvalidManifest, "Invalid manifest", http.StatusUnprocessableEntity, instance, detail,
		"Fix the manifest so it validates against spec/manifest.schema.json, then write it again.")
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

// Internal reports a failure inside kitbash itself.
func Internal(instance, detail, fix string) *Problem {
	if fix == "" {
		fix = "Retry the call. If it keeps failing, report the detail to an administrator."
	}
	return newProblem(SlugInternal, "Internal error", http.StatusInternalServerError, instance, detail, fix)
}
