package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// kitbashd records every build so that an /org Package built by one member is
// not built again by the next: the record says who has the image, and
// images_fetch copies it, see PLAN.md section 2.2 and issue 68.

// OrgRoot is the root that belongs to the whole organization. A build under it
// is readable by every member, because every member may run what it names; a
// build under a home is that member's alone.
const OrgRoot = "/org"

// MaxBuildsPerPath is how many build records one Package path keeps. Beyond it
// the oldest are pruned: the table is a directory of who holds which image
// now, not an archive, and a path built four thousand times has nothing useful
// left in its oldest row.
const MaxBuildsPerPath = 4096

// MaxBuildsPerBuilder is how many records one member keeps for one path. It is
// the bound that matters: without it a member could record enough rows of one
// path to prune every other member's out of the table, and a fetch would then
// find nothing where another member's build used to be. Sixty four builds of
// one Package is far more than a member has images for.
const MaxBuildsPerBuilder = 64

// commitSha is the shape of a Files commit, which is what a build record names
// as the version it was built from.
var commitSha = regexp.MustCompile(`^[0-9a-f]{40}$`)

// buildRequest is the builds_record input of spec/kitbashd-api.yaml. The
// builder is not in it: it is the peer, the same rule every other write on
// this socket follows.
type buildRequest struct {
	Path   string `json:"path"`
	Commit string `json:"commit"`
	Digest string `json:"digest"`
	Size   int64  `json:"size,omitempty"`
}

// buildList is what builds_list answers, newest build first.
type buildList struct {
	Builds []store.Build `json:"builds"`
}

// builds answers POST and GET on /kitbash/v1/builds.
func (s *Server) builds(w http.ResponseWriter, r *http.Request) {
	caller, prob := s.caller(r)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.recordBuild(w, r, caller)
	case http.MethodGet:
		s.listBuilds(w, r, caller)
	default:
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s %s is not part of the kitbashd API", r.Method, r.URL.Path),
			"Call POST to record a build, or GET with a path to list them."))
	}
}

// recordBuild writes one build for the peer. A member records what they built,
// never what somebody else did: the row is what another member's fetch is
// answered from, so a builder nobody can check would be a way to point a copy
// at any image on the host.
func (s *Server) recordBuild(w http.ResponseWriter, r *http.Request, caller Caller) {
	var req buildRequest
	if prob := decodeBody(w, r, &req); prob != nil {
		writeProblem(w, prob)
		return
	}
	folder, prob := s.buildPath(r, caller, req.Path)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	if !commitSha.MatchString(req.Commit) {
		writeProblem(w, problem.BadRequest(r.URL.Path,
			fmt.Sprintf("%q is not a commit", req.Commit),
			"Send the commit the Package was built from, as 40 hexadecimal characters."))
		return
	}
	if !imageDigest.MatchString(req.Digest) {
		writeProblem(w, problem.BadRequest(r.URL.Path,
			fmt.Sprintf("%q is not an image digest", req.Digest),
			"Send the digest as sha256: followed by 64 hexadecimal characters."))
		return
	}
	if req.Size < 0 {
		writeProblem(w, problem.BadRequest(r.URL.Path,
			"an image does not have a negative size",
			"Send the size of the image in bytes, or none at all."))
		return
	}
	// A record is a claim about an image, and every other member's pkg_build
	// acts on it, so it is checked against the caller's own store before it is
	// written. Without this a member could record any digest against any /org
	// path and commit, and the next member's build of that commit would answer
	// the digest they named instead of building, see PLAN.md section 2.2.
	info, prob := s.recordedImage(r, caller, folder, req)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	size := req.Size
	if size == 0 {
		size = info.Size
	}
	record := store.Build{
		Path:    folder,
		Commit:  req.Commit,
		Digest:  req.Digest,
		Builder: caller.User,
		BuiltAt: s.now().UTC(),
		Size:    size,
	}
	if err := s.store.RecordBuild(r.Context(), record,
		store.BuildLimits{PerPath: MaxBuildsPerPath, PerBuilder: MaxBuildsPerBuilder}); err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	writeJSON(w, r.URL.Path, record)
}

// listBuilds answers the builds of one Package path, newest first. A path
// under /org is every member's to read, because every member may run what it
// names; a path under a home is read by that member and by an admin.
func (s *Server) listBuilds(w http.ResponseWriter, r *http.Request, caller Caller) {
	query := r.URL.Query()
	folder := path.Clean(query.Get("path"))
	if prob := checkPackagePath(r.URL.Path, query.Get("path")); prob != nil {
		writeProblem(w, prob)
		return
	}
	if prob := s.mayRead(r, caller, folder); prob != nil {
		writeProblem(w, prob)
		return
	}
	filter := store.BuildFilter{Path: folder, Commit: query.Get("commit"), Digest: query.Get("digest")}
	if filter.Commit != "" && !commitSha.MatchString(filter.Commit) {
		writeProblem(w, problem.BadRequest(r.URL.Path,
			fmt.Sprintf("%q is not a commit", filter.Commit),
			"Ask for a commit as 40 hexadecimal characters, or for none and read them all."))
		return
	}
	if filter.Digest != "" && !imageDigest.MatchString(filter.Digest) {
		writeProblem(w, problem.BadRequest(r.URL.Path,
			fmt.Sprintf("%q is not an image digest", filter.Digest),
			"Ask for a digest as sha256: followed by 64 hexadecimal characters, or for none."))
		return
	}
	builds, err := s.store.Builds(r.Context(), filter)
	if err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	writeJSON(w, r.URL.Path, buildList{Builds: builds})
}

// recordedImage is the image a member says they built, read out of their own
// store. A record is accepted only when they hold the digest and the image
// itself says it is a build of that Package at that commit: the labels are
// inside the image and were written by the build, so they are the one thing
// here that a caller cannot simply assert.
func (s *Server) recordedImage(r *http.Request, caller Caller, folder string, req buildRequest) (sysusers.ImageInfo, *problem.Problem) {
	m, found, prob := s.memberOf(r, caller.User)
	if prob != nil {
		return sysusers.ImageInfo{}, prob
	}
	if !found || !m.IsMember() {
		return sysusers.ImageInfo{}, problem.NotPermitted(r.URL.Path,
			fmt.Sprintf("%s is not a member of this host", caller.User),
			"Ask an administrator to create a member for this account.")
	}
	info, err := s.runner.ImageInfo(r.Context(), m, req.Digest)
	if err != nil {
		if errors.Is(err, sysusers.ErrNoImage) {
			// A record of an image the caller does not hold is the caller's
			// mistake, not the host's: kitbashd would be asked to copy it out
			// of a store that does not have it.
			return sysusers.ImageInfo{}, problem.BadRequest(r.URL.Path,
				fmt.Sprintf("you do not hold the image %s", req.Digest),
				"Record the digest pkg_build answered, in the session that built it.")
		}
		logger.Printf("builds: reading %s in %s's store: %v", req.Digest, caller.User, err)
		return sysusers.ImageInfo{}, problem.Internal(r.URL.Path, err.Error(), "")
	}
	if prob := checkProvenance(r.URL.Path, info, folder, req.Commit,
		"the image you recorded is not a build of this commit"); prob != nil {
		return sysusers.ImageInfo{}, prob
	}
	return info, nil
}

// checkProvenance holds one image to what its labels say it is. A digest is a
// number a caller chose; the labels are what the build stamped inside the
// image, so this is the whole of what kitbashd knows about provenance, see
// PLAN.md section 2.2.
func checkProvenance(instance string, info sysusers.ImageInfo, folder, commit, detail string) *problem.Problem {
	if info.Label(podman.LabelPath) == folder && info.Label(podman.LabelCommit) == commit {
		return nil
	}
	return problem.BadRequest(instance, detail,
		fmt.Sprintf("Build the Package at %s from the commit you are naming; the image's own kitbash.path and kitbash.commit are what kitbashd reads.", folder))
}

// buildPath is the path a member may record a build for: under /org, which is
// the whole point of the record, or under their own home, which nobody else
// ever fetches and which is recorded only because a client that records
// everything is simpler than one that decides.
func (s *Server) buildPath(r *http.Request, caller Caller, given string) (string, *problem.Problem) {
	if prob := checkPackagePath(r.URL.Path, given); prob != nil {
		return "", prob
	}
	folder := path.Clean(given)
	if under(folder, OrgRoot) {
		return folder, nil
	}
	home, prob := s.homeOf(r, caller.User)
	if prob != nil {
		return "", prob
	}
	if home != "" && under(folder, home) {
		return folder, nil
	}
	return "", problem.NotPermitted(r.URL.Path,
		fmt.Sprintf("%s is neither under %s nor under your home", folder, OrgRoot),
		"Record builds of Packages under /org or under your own home.")
}

// mayRead decides who reads the builds of one path. Everything under /org is
// shared, so every member reads it; a home belongs to its member, and an admin
// reads every member's the same way tel_query does.
func (s *Server) mayRead(r *http.Request, caller Caller, folder string) *problem.Problem {
	if under(folder, OrgRoot) || caller.Admin {
		return nil
	}
	home, prob := s.homeOf(r, caller.User)
	if prob != nil {
		return prob
	}
	if home != "" && under(folder, home) {
		return nil
	}
	return problem.NotPermitted(r.URL.Path,
		fmt.Sprintf("%s is under another member's home", folder),
		"Read the builds of Packages under /org or under your own home.")
}

// homeOf is one member's home directory, empty for a caller this host has no
// member for. An account that is not a member is not refused here: it reaches
// /org like everybody else and nothing of a home.
func (s *Server) homeOf(r *http.Request, name string) (string, *problem.Problem) {
	m, found, err := s.users.Lookup(r.Context(), name)
	if err != nil {
		return "", problem.Internal(r.URL.Path, err.Error(), "")
	}
	if !found || m.Home == "" {
		return "", nil
	}
	return path.Clean(m.Home), nil
}

// checkPackagePath holds a Package path to the shape this API carries: an
// absolute path with no parent reference in it, because it is compared against
// roots and stored as it arrived.
func checkPackagePath(instance, given string) *problem.Problem {
	if given == "" || !strings.HasPrefix(given, "/") || len(given) > MaxPackageBytes {
		return problem.BadRequest(instance,
			fmt.Sprintf("%q is not a package path", given),
			"Send the package as the absolute path of the folder it was built from.")
	}
	for _, segment := range strings.Split(given, "/") {
		if segment == ".." {
			return problem.BadRequest(instance,
				"the package path contains a parent reference",
				"Send the package as an absolute path without any .. segment.")
		}
	}
	return nil
}

// under reports whether a cleaned path is a root or lies below it. It is a
// path comparison and not a stat: kitbashd never opens what a member names
// here, it only decides whose build record this is.
func under(folder, root string) bool {
	if root == "" {
		return false
	}
	return folder == root || strings.HasPrefix(folder, strings.TrimSuffix(root, "/")+"/")
}

// memberOf resolves one member by name for a call that runs the container
// runtime as them, which needs their uid, gid and home rather than their name.
func (s *Server) memberOf(r *http.Request, name string) (sysusers.Member, bool, *problem.Problem) {
	m, found, err := s.users.Lookup(r.Context(), name)
	if err != nil {
		return sysusers.Member{}, false, problem.Internal(r.URL.Path, err.Error(), "")
	}
	return m, found, nil
}
