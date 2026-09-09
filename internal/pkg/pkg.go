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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/safeopen"
	"github.com/zyx1121/kitbash/internal/safepath"
	"github.com/zyx1121/kitbash/internal/telemetry"
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

// Build is one entry of a Package's build history. Builder is the member who
// made it, which only kitbashd knows: an image in this member's store says
// nothing about who else has it, see PLAN.md section 2.2. It is empty for a
// build nobody recorded, which is every build of a Package in a home.
type Build struct {
	Digest  string `json:"digest"`
	Commit  string `json:"commit"`
	BuiltAt string `json:"builtAt"`
	Builder string `json:"builder,omitempty"`
}

// InspectResult is the output of pkg_inspect.
type InspectResult struct {
	Path     string         `json:"path"`
	Manifest map[string]any `json:"manifest"`
	Builds   []Build        `json:"builds"`
}

// ImportRequest is the input of pkg_import. Author and ApprovedBy are what
// they are on fs.WriteRequest: empty on a call an agent made, set when an
// admin's session executes an approved import, so the one commit it writes is
// authored by the member who asked for it.
type ImportRequest struct {
	Path       string
	Source     string
	Author     string
	ApprovedBy string
}

// input is this request as the pkg_import input of spec/mcp-surface.yaml,
// which is what an approval stores. The tool's input has no property this
// struct does not hold and refuses every other one.
func (req ImportRequest) input() (json.RawMessage, *problem.Problem) {
	body, err := json.Marshal(struct {
		Path   string `json:"path"`
		Source string `json:"source"`
	}{Path: req.Path, Source: req.Source})
	if err != nil {
		return nil, problem.Internal(req.Path, err.Error(), "")
	}
	return body, nil
}

// ImportResult is the output of pkg_import.
type ImportResult struct {
	Path   string    `json:"path"`
	Commit fs.Commit `json:"commit"`
}

// Builds is the build registry of kitbashd: who built which image of which
// Package, and the copy of one image between two members' stores. It is an
// interface here so this package depends on the daemon's contract rather than
// on a socket, the same way proc.Registry does.
//
// A session without one is a host without kitbashd: every build is a local
// build and nothing is shared, which is what pkg_build did before this
// existed, see PLAN.md section 2.2.
type Builds interface {
	// Builds is what kitbashd knows about one Package path, newest first. An
	// empty commit and digest ask for every build of that path.
	Builds(ctx context.Context, path, commit, digest string) ([]telemetry.Build, *problem.Problem)
	// RecordBuild tells kitbashd what this member built.
	RecordBuild(ctx context.Context, b telemetry.Build) *problem.Problem
	// FetchImage asks kitbashd to copy one image into this member's store
	// from the member who built it.
	FetchImage(ctx context.Context, digest, path, from string) (*telemetry.FetchResult, *problem.Problem)
}

// Service answers the pkg family for one caller.
type Service struct {
	files  *fs.Service
	runner podman.Runner
	kits   Kits
	builds Builds
}

// New builds the Service the server runs with. The kits argument is the MCP
// bridge, which pkg_import needs and the other tools do not; it may be nil.
func New(files *fs.Service, runner podman.Runner, kits Kits) *Service {
	return &Service{files: files, runner: runner, kits: kits}
}

// SetBuilds gives the service the build registry of kitbashd. Without it a
// build of an /org Package is a local build and is not recorded, which is a
// host whose daemon is not running, the same shape fs.SetApprovals has.
func (s *Service) SetBuilds(builds Builds) {
	s.builds = builds
}

// Build answers pkg_build: it builds the Package at path from its current
// commit and returns the image ID as the digest.
func (s *Service) Build(ctx context.Context, path string) (*BuildResult, *problem.Problem) {
	// A build is the one operation on the surface that runs another program
	// for minutes, so it carries its own span under the tool's, and the log
	// tail it produces is the one log record per build that PLAN.md section
	// 2.4 promises.
	ctx, span := telemetry.Start(ctx, "build")
	defer span.End()
	result, prob := s.build(ctx, span, path)
	if prob != nil {
		span.Fail(prob.Slug(), prob.Title)
		return nil, prob
	}
	span.OK()
	return result, nil
}

func (s *Service) build(ctx context.Context, span *telemetry.Span, path string) (*BuildResult, *problem.Problem) {
	m, folder, prob := s.files.Manifest(ctx, path)
	if prob != nil {
		return nil, prob
	}
	// The Package is what the whole call is about, so it goes on the tool's
	// span as well as this one: a query by package has to find pkg_build.
	telemetry.SetPackage(ctx, folder)
	telemetry.SetPath(ctx, folder)
	unit, ok := m.Unit()
	if !ok || unit.Type != manifest.UnitContainer {
		return nil, problem.InvalidManifest(folder,
			"version 1 builds container units")
	}
	contextDir, containerfile, cleanup, prob := s.source(folder, unit)
	if cleanup != nil {
		defer cleanup()
	}
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
	// A Package under /org is one Package for the whole organization, so a
	// build of this commit that another member already made is copied rather
	// than made again: one commit is then one digest for everybody, see
	// PLAN.md section 2.2.
	if result := s.copied(ctx, span, folder, head.Sha, tag); result != nil {
		return result, nil
	}
	labels := map[string]string{
		podman.LabelPath:   folder,
		podman.LabelName:   m.Name,
		podman.LabelCommit: head.Sha,
		podman.LabelUser:   s.files.User(),
	}
	digest, log, err := s.runner.Build(ctx, contextDir, containerfile, tag, labels)
	if err != nil {
		if errors.Is(err, podman.ErrBuildFailed) {
			// A build that ran and failed is the caller's to fix, so the tail
			// of the log goes back with the error instead of only to the
			// server log. The full log still never leaves the host.
			span.Error(buildFailure(log))
			return nil, problem.BadRequest(folder, buildFailure(log),
				"Fix the build context and call pkg_build again.")
		}
		// The runtime itself could not run the build, which the caller can do
		// nothing about.
		return nil, problem.Internal(folder, err.Error(),
			"Ask an administrator to check the container runtime on this host.")
	}
	span.SetDigest(digest)
	span.Info(tail(log))
	// The record is what the next member's build reads instead of building.
	// It is best effort: a build that is not recorded is a build somebody
	// repeats, which is what every build did before this record existed.
	s.record(ctx, span, folder, head.Sha, digest)
	return &BuildResult{
		Path:   folder,
		Digest: digest,
		Commit: head.Sha,
		Log:    tail(log),
	}, nil
}

// copied is the build another member already made, copied into this member's
// store instead of made again. It answers nil when there is nothing to copy
// and when the copy failed, which are the same thing to the caller: build it.
//
// Only /org Packages take part. A Package in a home is that member's alone,
// so nothing about it is recorded and nothing about it is fetched.
func (s *Service) copied(ctx context.Context, span *telemetry.Span, folder, commit, tag string) *BuildResult {
	if s.builds == nil || !s.files.UnderShared(folder) {
		return nil
	}
	builds, prob := s.builds.Builds(ctx, folder, commit, "")
	if prob != nil {
		// kitbashd is not answering, which costs a build that could have been
		// a copy and nothing else.
		span.Error("the builds of this Package could not be read from kitbashd: " + prob.Detail)
		return nil
	}
	local, err := s.runner.Images(ctx, podman.Filter{podman.LabelPath: folder})
	if err != nil {
		span.Error("the local images of this Package could not be read: " + err.Error())
		return nil
	}
	held := map[string]bool{}
	for _, image := range local {
		held[image.ID] = true
	}
	for _, build := range builds {
		// A build of this member's own is not fetched: either they have the
		// image, or they removed it and are building it again on purpose.
		if build.Builder == "" || build.Builder == s.files.User() || held[build.Digest] {
			continue
		}
		result, prob := s.builds.FetchImage(ctx, build.Digest, folder, build.Builder)
		if prob != nil {
			span.Error("the image " + build.Digest + " could not be copied from " +
				build.Builder + ": " + prob.Detail)
			return nil
		}
		// The tag is the one a local build would have written, so podman
		// images reads the same whether this Package was built here or
		// copied. A tag that fails is a name, not an image: the copy stands.
		if err := s.runner.Tag(ctx, build.Digest, tag); err != nil {
			span.Error("the copied image could not be tagged " + tag + ": " + err.Error())
		}
		span.SetDigest(build.Digest)
		log := fmt.Sprintf("copied from %s", build.Builder)
		span.Info(log)
		return &BuildResult{Path: folder, Digest: result.Digest, Commit: commit, Log: log}
	}
	return nil
}

// record tells kitbashd what this member built, so the next member copies it.
// Only /org Packages are recorded: a Package in a home is never fetched, and a
// record of one would be a list of a member's private Packages that every
// admin reads.
func (s *Service) record(ctx context.Context, span *telemetry.Span, folder, commit, digest string) {
	if s.builds == nil || !s.files.UnderShared(folder) {
		return
	}
	if prob := s.builds.RecordBuild(ctx, telemetry.Build{
		Path: folder, Commit: commit, Digest: digest,
	}); prob != nil {
		span.Error("this build was not recorded with kitbashd: " + prob.Detail)
	}
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
			// The image's own label says who built it, which for a copied
			// image is the member it was copied from. kitbashd's record is
			// merged over it below when there is one.
			Builder: image.Labels[podman.LabelUser],
		})
	}
	s.merge(ctx, folder, result)
	return result, nil
}

// merge folds kitbashd's build records into the local image list, so an agent
// reading an /org Package sees the builds of every member and not only the
// ones that reached this store. A record for an image that is here names its
// builder; one for an image that is not is a build this member can fetch by
// running it, see PLAN.md section 2.2.
func (s *Service) merge(ctx context.Context, folder string, result *InspectResult) {
	if s.builds == nil || !s.files.UnderShared(folder) {
		return
	}
	records, prob := s.builds.Builds(ctx, folder, "", "")
	if prob != nil {
		// The local history is still the answer. A daemon that is not
		// answering is not a reason to refuse to describe a Package.
		return
	}
	at := map[string]int{}
	for i, build := range result.Builds {
		at[build.Digest] = i
	}
	for _, record := range records {
		if i, held := at[record.Digest]; held {
			result.Builds[i].Builder = record.Builder
			if result.Builds[i].Commit == "" {
				result.Builds[i].Commit = record.Commit
			}
			continue
		}
		result.Builds = append(result.Builds, Build{
			Digest:  record.Digest,
			Commit:  record.Commit,
			BuiltAt: record.BuiltAt,
			Builder: record.Builder,
		})
		at[record.Digest] = len(result.Builds) - 1
	}
	// Newest first, which is what the tool publishes and what the local list
	// already was before anything was merged into it.
	sort.SliceStable(result.Builds, func(i, j int) bool {
		return result.Builds[i].BuiltAt > result.Builds[j].BuiltAt
	})
}

// source is what the build reads: the unit's own context, or, for a unit that
// names an existing image, a generated one line Containerfile in a temporary
// directory. The second return of cleanup removes that directory; it is never
// nil to check before calling.
//
// A unit with image is not pulled and tagged, it is built: the result then
// carries kitbash.path and its own image ID, so a Package that wraps an
// upstream image has the same build record as one that has a context. The
// manifest schema pins image by digest, so the FROM line is reproducible.
func (s *Service) source(folder string, unit manifest.Unit) (contextDir, containerfile string, cleanup func(), prob *problem.Problem) {
	switch {
	case unit.Build != "":
		contextDir, prob := s.buildContext(folder, unit.Build)
		if prob != nil {
			return "", "", nil, prob
		}
		containerfile, prob := s.findContainerfile(folder, unit.Build, contextDir)
		if prob != nil {
			return "", "", nil, prob
		}
		return contextDir, containerfile, nil, nil
	case unit.Image != "":
		dir, err := os.MkdirTemp("", "kitbash-image-")
		if err != nil {
			return "", "", nil, problem.Internal(folder, err.Error(),
				"Ask an administrator to check the disk on this host.")
		}
		remove := func() { os.RemoveAll(dir) }
		file := filepath.Join(dir, containerfiles[0])
		// The generated context is the caller's alone: nothing else on the
		// host has to read a file that exists for one build.
		if err := os.WriteFile(file, []byte("FROM "+unit.Image+"\n"), 0o600); err != nil {
			return "", "", remove, problem.Internal(folder, err.Error(),
				"Ask an administrator to check the disk on this host.")
		}
		return dir, file, remove, nil
	default:
		return "", "", nil, problem.InvalidManifestFix(folder,
			"the container unit has neither a build context nor an image",
			"Give deploy.units[0] a build context or an image pinned by digest.")
	}
}

// buildContext resolves deploy.units[0].build against the Package folder. A
// Package cannot reach outside its own tree at build time, which is rule 4 of
// PLAN.md section 2.5, and a symlink is a way out of the tree that looks like
// a way in: safepath applies the lexical rules and names what is wrong,
// safeopen asks the kernel, which refuses a link at any component as part of
// the syscall that reads it.
//
// The stat is made below the root the Package belongs to, not below the
// Package folder, because a root is the only thing RESOLVE_BENEATH can be
// anchored to: anchoring it to the Package folder would leave the path to that
// folder unguarded.
func (s *Service) buildContext(folder, build string) (string, *problem.Problem) {
	dir, err := safepath.Inside(folder, build)
	if err != nil {
		return "", problem.InvalidManifest(folder,
			fmt.Sprintf("the build context %q is outside the Package folder: %s", build, err))
	}
	info, err := s.statBelowRoot(folder, build)
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
func (s *Service) findContainerfile(folder, build, contextDir string) (string, *problem.Problem) {
	for _, name := range containerfiles {
		candidate, err := safepath.Inside(contextDir, name)
		if err != nil {
			continue
		}
		info, err := s.statBelowRoot(folder, filepath.Join(build, name))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		return candidate, nil
	}
	return "", problem.NotFoundFix(contextDir,
		"the build context has no Containerfile and no Dockerfile",
		"Write a Containerfile into the build context with fs_write, then build again.")
}

// statBelowRoot describes a path inside a Package folder, resolved from the
// root that folder belongs to. It is the one way this package touches the
// filesystem, so nothing here anchors a resolution to a path a caller chose.
func (s *Service) statBelowRoot(folder, rel string) (os.FileInfo, error) {
	root, folderRel, ok := s.files.RootOf(folder)
	if !ok {
		return nil, &os.PathError{Op: "stat", Path: folder, Err: syscall.EXDEV}
	}
	return safeopen.Stat(root, filepath.Join(folderRel, rel))
}

// buildFailure is what the caller reads when a build fails: the same tail the
// successful build returns, or a plain sentence when the build printed nothing.
func buildFailure(log string) string {
	if out := tail(log); out != "" {
		return out
	}
	return "the build failed and printed nothing"
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
