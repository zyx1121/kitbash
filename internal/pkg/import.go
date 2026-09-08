package pkg

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zyx1121/kitbash/internal/bridge"
	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/proc"
	"github.com/zyx1121/kitbash/internal/safepath"
	"github.com/zyx1121/kitbash/internal/telemetry"
)

// Kits is what pkg_import needs from the MCP bridge: the caller's running
// import kits and a way to call one. internal/bridge implements it, which
// keeps every MCP type out of this package.
type Kits interface {
	Kits(ctx context.Context) ([]bridge.Kit, *problem.Problem)
	CallTool(ctx context.Context, p *proc.Process, tool string, args map[string]any) (json.RawMessage, *problem.Problem)
}

// importOutput is the shape an import kit returns, see $defs.importFile in
// spec/mcp-surface.yaml.
type importOutput struct {
	Files []struct {
		Path          string  `json:"path"`
		Content       *string `json:"content,omitempty"`
		ContentBase64 *string `json:"contentBase64,omitempty"`
	} `json:"files"`
}

// Import answers pkg_import: it wraps something external as a Package folder
// using a running import kit, and writes the folder as one commit.
//
// The kit is chosen by schema, never by name: the source string is validated
// against each running kit's import input schema, and exactly one kit has to
// accept it. Two kits accepting the same source is a conflict the caller
// resolves by stopping one, see PLAN.md section 3.
func (s *Service) Import(ctx context.Context, req ImportRequest) (*ImportResult, *problem.Problem) {
	path, source := req.Path, req.Source
	exists, prob := s.files.Exists(ctx, path)
	if prob != nil {
		return nil, prob
	}
	if exists {
		return nil, problem.ConflictFix(path, "this path already exists",
			"Import into a new folder, or write into this one with fs_write.")
	}
	// A member importing into /org queues the call rather than running a kit
	// of their own: the admin who approves it runs the kit in their session,
	// see PLAN.md section 2.1. The path is validated above, so a queued import
	// is one that could have run.
	input, prob := req.input()
	if prob != nil {
		return nil, prob
	}
	if prob := s.files.Queue(ctx, path, telemetry.ToolPkgImport, input); prob != nil {
		return nil, prob
	}
	if s.kits == nil {
		return nil, problem.NotFoundFix(path, "no import kit is running",
			"Run an import kit that understands this source, for example /org/import-mcp.")
	}

	kits, prob := s.kits.Kits(ctx)
	if prob != nil {
		return nil, prob
	}
	accepting, prob := accept(kits, source)
	if prob != nil {
		return nil, prob
	}

	raw, prob := s.kits.CallTool(ctx, accepting.Process, bridge.ImportTool,
		map[string]any{"source": source})
	if prob != nil {
		return nil, prob
	}
	files, prob := importFiles(path, raw)
	if prob != nil {
		return nil, prob
	}
	written, prob := s.files.WriteFiles(ctx, fs.WriteFilesRequest{
		Path:       path,
		Files:      files,
		Message:    "Import " + source,
		Author:     req.Author,
		ApprovedBy: req.ApprovedBy,
	})
	if prob != nil {
		return nil, prob
	}
	// The Package the call is about is the one it just created, so the span
	// names it rather than nothing.
	telemetry.SetPackage(ctx, written.Path)
	return &ImportResult{Path: written.Path, Commit: written.Commit}, nil
}

// accept picks the one running kit whose import input schema accepts the
// source.
func accept(kits []bridge.Kit, source string) (*bridge.Kit, *problem.Problem) {
	args, err := json.Marshal(map[string]any{"source": source})
	if err != nil {
		return nil, problem.Internal(source, err.Error(), "")
	}
	var accepting []bridge.Kit
	for _, kit := range kits {
		if err := kit.Tool.ValidateInput(args); err == nil {
			accepting = append(accepting, kit)
		}
	}
	switch len(accepting) {
	case 0:
		return nil, problem.NotFoundFix(source, "no running import kit accepts this source",
			"Run an import kit that understands this source, for example /org/import-mcp.")
	case 1:
		return &accepting[0], nil
	default:
		var names []string
		for _, kit := range accepting {
			names = append(names, kit.Process.Package)
		}
		return nil, problem.ConflictFix(source,
			fmt.Sprintf("%d running import kits accept this source: %s",
				len(accepting), strings.Join(names, ", ")),
			"Stop all but one of them with proc_stop, then import again.")
	}
}

// importFiles decodes what a kit returned and applies the rules of
// $defs.importFile before a single byte is written.
func importFiles(path string, raw json.RawMessage) ([]fs.File, *problem.Problem) {
	var out importOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, problem.InternalDetail(path, err.Error(),
			"the import kit returned output that is not a file list",
			"Ask the kit's author to return the files its manifest declares.")
	}
	if len(out.Files) == 0 {
		return nil, problem.InternalDetail(path, "the import kit returned an empty file list",
			"the import kit returned no files",
			"Ask the kit's author to return the files its manifest declares.")
	}
	files := make([]fs.File, 0, len(out.Files))
	manifestFound := false
	for _, file := range out.Files {
		if file.Path == "." {
			return nil, problem.InvalidPathFix(path,
				"the import kit returned the folder itself as a file",
				"Ask the kit's author to return one path per file.")
		}
		if prob := checkImportPath(path, file.Path); prob != nil {
			return nil, prob
		}
		if filepath.Base(file.Path) == manifest.FileName && filepath.Dir(file.Path) == "." {
			manifestFound = true
		}
		files = append(files, fs.File{
			Path:          file.Path,
			Content:       file.Content,
			ContentBase64: file.ContentBase64,
		})
	}
	if !manifestFound {
		return nil, problem.InvalidManifest(path,
			"the import kit returned no kitbash.yaml, so the folder would not be visible")
	}
	return files, nil
}

// checkImportPath applies the path rules to one file a kit returned. fs
// applies them again when it writes; saying here that the kit is at fault is
// what makes the error actionable.
func checkImportPath(folder, rel string) *problem.Problem {
	if _, err := safepath.Inside(folder, rel); err != nil {
		return problem.InvalidPathFix(filepath.Join(folder, filepath.Base(rel)),
			fmt.Sprintf("the import kit returned a path kitbash will not write: %s", err),
			"Ask the kit's author to return plain relative paths inside the target folder.")
	}
	return nil
}
