package pkg

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
)

// imageDigest is the shape of an OCI image id, which is what a build answers
// with whoever made it: the digest is the Package version, see PLAN.md section
// 2.2, and a build kit is held to the same shape the built in path produces.
var imageDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// buildOutput is what a build kit answers, see $package_tools.build_hook in
// spec/mcp-surface.yaml.
type buildOutput struct {
	Digest string `json:"digest"`
	Log    string `json:"log"`
}

// buildWithKit answers pkg_build for a unit whose manifest names a builder.
// The kit turns the folder into an image wherever it builds, and what comes
// back is recorded the way a local build is: the digest is the version, the
// log tail is the one log record per build of PLAN.md section 2.4, and the
// span is the same span the built in path opens.
//
// Dispatch is routing and not a third built in: the OCI path below is still
// what builds every Package that names no builder, see PLAN.md section 3.
func (s *Service) buildWithKit(ctx context.Context, span *telemetry.Span, folder string, unit manifest.Unit, commit string) (*BuildResult, *problem.Problem) {
	if s.kits == nil {
		return nil, problem.Internal(folder,
			"this session has no MCP bridge, and a build kit is a Process reached over it",
			"Open a new session and build this Package again.")
	}
	kit, prob := s.kits.KitAt(ctx, unit.Builder, manifest.HookBuild, manifest.ToolBuild)
	if prob != nil {
		return nil, prob
	}
	// The context is resolved here rather than by the kit: rule 4 of PLAN.md
	// section 2.5 is that a Package cannot reach outside its own tree at build
	// time, and that is the core's rule whoever does the building.
	contextDir := folder
	if unit.Build != "" {
		contextDir, prob = s.buildContext(folder, unit.Build)
		if prob != nil {
			return nil, prob
		}
	}
	args := map[string]any{"path": folder, "context": contextDir}
	if err := kit.Tool.AcceptsArgs(args); err != nil {
		return nil, problem.InvalidManifestFix(unit.Builder, fmt.Sprintf(
			"the build tool of the kit at %s does not accept what the hook is called with: %s", unit.Builder, err),
			"Give the kit's build tool an input schema that accepts path and context.")
	}
	raw, prob := s.kits.CallTool(ctx, kit.Process, manifest.ToolBuild, args)
	if prob != nil {
		return nil, prob
	}
	out, prob := kitBuild(unit.Builder, raw)
	if prob != nil {
		return nil, prob
	}

	span.SetDigest(out.Digest)
	span.Info(tail(out.Log))
	// The build is recorded with kitbashd only when this member's own store
	// holds the image, because that is the record's whole promise: kitbashd
	// serves a copy of it to the next member from the store of the member who
	// recorded it, and it checks the image's labels before it does, see
	// PLAN.md section 2.2. A kit that built somewhere else built an image no
	// copy on this host could answer for.
	if _, held := s.localImage(ctx, folder, out.Digest); held {
		s.record(ctx, span, folder, commit, out.Digest)
	}
	return &BuildResult{
		Path:   folder,
		Digest: out.Digest,
		Commit: commit,
		Log:    tail(out.Log),
	}, nil
}

// kitBuild decodes what a build kit answered and holds it to the hook before
// the digest is handed to the caller as the version of their Package.
func kitBuild(path string, raw json.RawMessage) (buildOutput, *problem.Problem) {
	var out buildOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, problem.InternalDetail(path, err.Error(),
			"the build kit returned output that is not a build",
			"Ask the kit's author to return the digest and log its manifest declares.")
	}
	if !imageDigest.MatchString(out.Digest) {
		return out, problem.InternalDetail(path,
			fmt.Sprintf("the build kit returned the digest %q", out.Digest),
			"the build kit returned something that is not an OCI image id",
			"Ask the kit's author to answer with sha256: followed by 64 hexadecimal characters.")
	}
	return out, nil
}
