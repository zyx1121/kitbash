// Package pkg implements the pkg tool family from spec/mcp-surface.yaml: the
// Packages object, a folder in Files built into an OCI image. It holds no MCP
// types, the way internal/fs holds none.
//
// The image store is the build history. A build stamps the image with
// kitbash.path, kitbash.name, kitbash.commit and kitbash.user, and no second
// record is kept, see PLAN.md section 2.2.
package pkg

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
)

// Containerfile names a build context may carry, most preferred first.
var containerfiles = []string{"Containerfile", "Dockerfile"}

// LogTailLines and LogTailBytes cap the build log the surface returns. The
// full log never leaves the host.
const (
	LogTailLines = 40
	LogTailBytes = 4 << 10
)

// TagPrefix is the local repository every built image is tagged under. The
// digest is the version; the tag is for a human reading podman images.
const TagPrefix = "localhost/kitbash/"

// BuildResult is the output of pkg_build.
type BuildResult struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Commit string `json:"commit"`
	Log    string `json:"log,omitempty"`
}

// Entry is one Package in the output of pkg_list.
type Entry struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Digest  string `json:"digest,omitempty"`
	BuiltAt string `json:"builtAt,omitempty"`
	Running int    `json:"running"`
}

// ListResult is the output of pkg_list.
type ListResult struct {
	Packages []Entry `json:"packages"`
}

// Build is one entry of a Package's build history.
type Build struct {
	Digest  string `json:"digest"`
	Commit  string `json:"commit"`
	BuiltAt string `json:"builtAt"`
}

// InspectResult is the output of pkg_inspect.
type InspectResult struct {
	Path     string         `json:"path"`
	Manifest map[string]any `json:"manifest"`
	Builds   []Build        `json:"builds"`
}

// ImportResult is the output of pkg_import.
type ImportResult struct {
	Path   string    `json:"path"`
	Commit fs.Commit `json:"commit"`
}

// Service answers the pkg family for one caller.
type Service struct {
	files  *fs.Service
	runner podman.Runner
	kits   Kits
}

// New builds the Service the server runs with. The kits argument is the MCP
// bridge, which pkg_import needs and the other tools do not; it may be nil.
func New(files *fs.Service, runner podman.Runner, kits Kits) *Service {
	return &Service{files: files, runner: runner, kits: kits}
}

// Build answers pkg_build: it builds the Package at path from its current
// commit and returns the image ID as the digest.
func (s *Service) Build(ctx context.Context, path string) (*BuildResult, *problem.Problem) {
	m, folder, prob := s.files.Manifest(ctx, path)
	if prob != nil {
		return nil, prob
	}
	unit, ok := m.Unit()
	if !ok || unit.Type != manifest.UnitContainer || unit.Build == "" {
		return nil, problem.InvalidManifest(folder,
			"version 1 builds container units with a build context")
	}
	contextDir, prob := buildContext(folder, unit.Build)
	if prob != nil {
		return nil, prob
	}
	containerfile, prob := findContainerfile(contextDir)
	if prob != nil {
		return nil, prob
	}

	head, prob := s.files.Head(ctx, folder)
	if prob != nil {
		return nil, prob
	}
	if !head.Clean {
		return nil, problem.ConflictFix(folder,
			fmt.Sprintf("uncommitted changes under %s; kitbash builds from a commit", folder),
			"Commit the folder with fs_write, then build again.")
	}

	tag := TagPrefix + m.Name + ":" + shortSha(head.Sha)
	labels := map[string]string{
		podman.LabelPath:   folder,
		podman.LabelName:   m.Name,
		podman.LabelCommit: head.Sha,
		podman.LabelUser:   s.files.User(),
	}
	digest, log, err := s.runner.Build(ctx, contextDir, containerfile, tag, labels)
	if err != nil {
		// The build log is the cause, and the cause goes to the server log:
		// it carries host paths and the runtime's own output.
		return nil, problem.Internal(folder, err.Error(),
			"Read the Containerfile and the build context, fix the build, and try again.")
	}
	return &BuildResult{
		Path:   folder,
		Digest: digest,
		Commit: head.Sha,
		Log:    tail(log),
	}, nil
}

// List answers pkg_list: the visible Packages with their newest build.
func (s *Service) List(ctx context.Context) (*ListResult, *problem.Problem) {
	entries, prob := s.files.Packages(ctx)
	if prob != nil {
		return nil, prob
	}
	images, err := s.runner.Images(ctx, nil)
	if err != nil {
		return nil, problem.Internal("pkg", err.Error(), "")
	}
	newest := map[string]podman.Image{}
	for _, image := range images {
		path := image.Labels[podman.LabelPath]
		if path == "" {
			continue
		}
		if got, ok := newest[path]; !ok || image.Created.After(got.Created) {
			newest[path] = image
		}
	}
	containers, err := s.runner.Containers(ctx, podman.Filter{podman.LabelUser: s.files.User()}, false)
	if err != nil {
		return nil, problem.Internal("pkg", err.Error(), "")
	}
	running := map[string]int{}
	for _, container := range containers {
		if container.State == podman.StateRunning {
			running[container.Labels[podman.LabelPackage]]++
		}
	}

	result := &ListResult{Packages: []Entry{}}
	for _, entry := range entries {
		out := Entry{
			Path:    entry.Path,
			Name:    entry.Manifest.Name,
			Running: running[entry.Path],
		}
		if image, ok := newest[entry.Path]; ok {
			out.Digest = image.ID
			out.BuiltAt = image.Created.UTC().Format(time.RFC3339)
		}
		result.Packages = append(result.Packages, out)
	}
	return result, nil
}

// Inspect answers pkg_inspect: the manifest and the build history, newest
// build first.
func (s *Service) Inspect(ctx context.Context, path string) (*InspectResult, *problem.Problem) {
	m, folder, prob := s.files.Manifest(ctx, path)
	if prob != nil {
		return nil, prob
	}
	images, err := s.runner.Images(ctx, podman.Filter{podman.LabelPath: folder})
	if err != nil {
		return nil, problem.Internal(folder, err.Error(), "")
	}
	result := &InspectResult{Path: folder, Manifest: m.Raw, Builds: []Build{}}
	for _, image := range images {
		result.Builds = append(result.Builds, Build{
			Digest:  image.ID,
			Commit:  image.Labels[podman.LabelCommit],
			BuiltAt: image.Created.UTC().Format(time.RFC3339),
		})
	}
	return result, nil
}

// buildContext resolves deploy.units[0].build against the Package folder. A
// Package cannot reach outside its own tree at build time, which is rule 4 of
// PLAN.md section 2.5.
func buildContext(folder, build string) (string, *problem.Problem) {
	dir := filepath.Clean(filepath.Join(folder, build))
	rel, err := filepath.Rel(folder, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", problem.InvalidManifest(folder,
			fmt.Sprintf("the build context %q is outside the Package folder", build))
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", problem.NotFoundFix(dir,
			fmt.Sprintf("the build context %q is not a folder", build),
			"Point deploy.units[0].build at a folder inside this Package.")
	}
	return dir, nil
}

// findContainerfile picks the file the build reads. Containerfile wins, which
// is the OCI spelling; Dockerfile is accepted because that is what an agent
// writes by habit.
func findContainerfile(contextDir string) (string, *problem.Problem) {
	for _, name := range containerfiles {
		candidate := filepath.Join(contextDir, name)
		info, err := os.Lstat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		return candidate, nil
	}
	return "", problem.NotFoundFix(contextDir,
		"the build context has no Containerfile and no Dockerfile",
		"Write a Containerfile into the build context with fs_write, then build again.")
}

// shortSha is the twelve character commit the image tag carries.
func shortSha(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// tail is the end of the build log, bounded by lines and by bytes.
func tail(log string) string {
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if len(lines) > LogTailLines {
		lines = lines[len(lines)-LogTailLines:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > LogTailBytes {
		out = out[len(out)-LogTailBytes:]
	}
	return out
}
