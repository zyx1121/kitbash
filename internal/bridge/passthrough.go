package bridge

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/problem"
)

// The bounds a Package's own problem is held to. A problem passed through is
// text a Package wrote, so it is input: without a bound, a hostile Package
// could answer every call with a megabyte of detail and bloat the surface the
// agent reads. The numbers are generous for a real message and useless as a
// channel, see $package_tools.package_errors in spec/mcp-surface.yaml.
const (
	// MaxPassedDetail is the detail a passed through problem may carry.
	MaxPassedDetail = 4 << 10
	// MaxPassedTitle is the title, in characters rather than bytes, because a
	// title is a phrase and not a payload.
	MaxPassedTitle = 200
	// MaxPassedInstance is the path or identifier the problem is about. It is
	// clipped rather than dropped: a truncated instance still says which of the
	// caller's paths the Package meant.
	MaxPassedInstance = 1 << 10
	// MaxPassedFix is the advice. A fix longer than this is not advice, so it
	// is dropped rather than clipped: half a sentence telling an agent what to
	// do next is worse than none.
	MaxPassedFix = 1 << 10
	// MaxPassedType is the type URI. It is not clipped, it is refused: a
	// clipped URI names a different error class, and the class is the one
	// field a client matches on. The type also reaches Telemetry as a span
	// attribute, so an invented class of any length is a Package writing into
	// the caller's traces.
	MaxPassedType = 200
)

// passable are the error classes a Package may answer with. Everything a kit
// can legitimately report about its own work is here, and nothing the surface
// says about itself is: queued in particular is the approval answer, and a
// Package returning it would tell the agent an admin is about to run a call
// that was never queued. A slug outside this set is a Package reaching for a
// meaning that is not its to give, so its text is wrapped instead.
//
// invalid-path is here because a Package can hold paths of its own: an
// import-cli Package whose unit mounts a folder of Files is given a path by its
// caller and has to answer what the fs family answers for a path that leaves
// the tree it may read, see issue #122. A bad-request with that document in its
// detail would tell the agent the call was malformed when the call was well
// formed and the path was not.
//
// The value is the status that class carries. A Package that pairs a class
// with another status is describing something else, and is wrapped too.
var passable = map[string]int{
	problem.SlugNotFound:        http.StatusNotFound,
	problem.SlugBadRequest:      http.StatusBadRequest,
	problem.SlugNotPermitted:    http.StatusForbidden,
	problem.SlugInvalidPath:     http.StatusBadRequest,
	problem.SlugInvalidManifest: http.StatusUnprocessableEntity,
	problem.SlugUnsupported:     http.StatusUnsupportedMediaType,
	problem.SlugTooLarge:        http.StatusRequestEntityTooLarge,
	problem.SlugConflict:        http.StatusConflict,
	problem.SlugInternal:        http.StatusInternalServerError,
}

// packageProblem is the Package's own problem, or nil when the error it
// returned is not one.
//
// A kit that already speaks the surface's error language should reach the
// agent in that language: its not-found is a not-found, not a bad-request with
// JSON buried in the detail, which is what issue #58 asked for. The bar is
// deliberately narrow. The result must be one text block, it must parse as an
// object with no field the surface does not define, its class must be one a
// Package may claim, and its status must be the one that class carries.
// Anything else is a Package reporting an error in its own words, and those
// keep the wrapper.
func packageProblem(res *mcp.CallToolResult) *problem.Problem {
	text, ok := singleText(res)
	if !ok {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	// An unknown field means this is not the surface's problem shape, and
	// passing it through would silently drop what the Package meant by it.
	decoder.DisallowUnknownFields()
	var p problem.Problem
	if err := decoder.Decode(&p); err != nil {
		return nil
	}
	if decoder.More() {
		return nil
	}
	if len(p.Type) > MaxPassedType || !strings.HasPrefix(p.Type, problem.Base) {
		return nil
	}
	status, ok := passable[p.Slug()]
	if !ok || p.Status != status {
		return nil
	}
	if p.Title == "" || p.Detail == "" {
		return nil
	}
	p.Title = clipRunes(p.Title, MaxPassedTitle)
	p.Detail = clipBytes(p.Detail, MaxPassedDetail)
	p.Instance = clipBytes(p.Instance, MaxPassedInstance)
	if len(p.Fix) > MaxPassedFix {
		p.Fix = ""
	}
	return &p
}

// singleText is the one text block a problem arrives as. A result carrying an
// image, or two blocks, is not a problem document however the first block
// parses.
func singleText(res *mcp.CallToolResult) (string, bool) {
	if len(res.Content) != 1 {
		return "", false
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(text.Text)
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

// clipRunes cuts a string to a number of characters.
func clipRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit])
}

// clipBytes cuts a string to a number of bytes, on a character boundary, so
// what is passed through is still text.
func clipBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}
