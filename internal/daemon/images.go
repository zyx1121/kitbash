package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// A member's images live in that member's rootless store, which no other
// member can read. kitbashd is root, so it is the one that can run a save as
// one member into a load as another, which is what makes one commit one digest
// for everybody, see PLAN.md section 2.2.

// actionFetch is the one verb this API serves on an image.
const actionFetch = "fetch"

// FetchDeadline is the response deadline of one fetch. It is wider than the
// budget sysusers gives the two children, so the answer arrives rather than
// the deadline, the same shape processes_start uses.
const FetchDeadline = 6 * time.Minute

// fetchRequest is the images_fetch input of spec/kitbashd-api.yaml. The
// requester is the peer; From names the member kitbashd asks for the image,
// and it has to be the one the build record names.
type fetchRequest struct {
	Path string `json:"path"`
	From string `json:"from,omitempty"`
}

// fetchResponse is what a fetch answers: the image the requester now has and
// how big it is in their own store.
type fetchResponse struct {
	Digest string `json:"digest"`
	Bytes  int64  `json:"bytes"`
	From   string `json:"from"`
}

// image answers everything under /kitbash/v1/images/{digest}: today only the
// fetch that copies one into the caller's own store.
func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, imagesPath+"/")
	digest, action, _ := strings.Cut(rest, "/")
	if action != actionFetch {
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s is not part of the kitbashd API", r.URL.Path),
			"Call POST fetch on the image digest to copy it into your own store."))
		return
	}
	if r.Method != http.MethodPost {
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s %s is not part of the kitbashd API", r.Method, r.URL.Path),
			fmt.Sprintf("Call POST %s instead.", r.URL.Path)))
		return
	}
	s.fetchImage(w, r, digest)
}

// fetchImage copies one image into the caller's store from the member who
// built it. Everything it needs is checked before a child runs: the digest is
// a digest, the path is one the caller may reach, kitbashd has a record of
// that build, and both members are members of this host.
func (s *Server) fetchImage(w http.ResponseWriter, r *http.Request, digest string) {
	caller, prob := s.caller(r)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	if !imageDigest.MatchString(digest) {
		writeProblem(w, problem.BadRequest(r.URL.Path,
			fmt.Sprintf("%q is not an image digest", digest),
			"Name the image as sha256: followed by 64 hexadecimal characters."))
		return
	}
	var req fetchRequest
	if prob := decodeBody(w, r, &req); prob != nil {
		writeProblem(w, prob)
		return
	}
	if prob := checkPackagePath(r.URL.Path, req.Path); prob != nil {
		writeProblem(w, prob)
		return
	}
	folder := path.Clean(req.Path)

	// A Package under /org is every member's to run, so its image is every
	// member's to fetch. A Package under a home is not: a copy out of another
	// member's home would be kitbashd reading a private image store on behalf
	// of somebody the kernel keeps out of it.
	if !under(folder, OrgRoot) {
		home, prob := s.homeOf(r, caller.User)
		if prob != nil {
			writeProblem(w, prob)
			return
		}
		if home == "" || !under(folder, home) {
			writeProblem(w, problem.NotPermitted(r.URL.Path,
				fmt.Sprintf("%s is under another member's home", folder),
				"Fetch images of Packages under /org; a Package in a home is built where it is run."))
			return
		}
	}

	build, prob := s.buildRecord(r, folder, digest, req.From)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	// A member fetching their own image has nothing to copy. It is answered
	// rather than refused: a client that asks for what it turns out to have
	// is describing a desired state, which is the invariant of PLAN.md 2.6.
	if build.Builder == caller.User {
		s.answerLocal(w, r, caller, digest, build.Builder)
		return
	}

	// One fetch per requester and digest. Two at once would run two loads
	// into one store for one image, which is work the runtime does twice and
	// a lock it takes against itself.
	release, taken := s.fetches.take(caller.User + " " + digest)
	if !taken {
		writeProblem(w, problem.ConflictFix(r.URL.Path,
			fmt.Sprintf("%s is already being copied into %s's store", digest, caller.User),
			"Wait for the copy that is running to finish, then read the answer of that one."))
		return
	}
	defer release()

	builder, found, prob := s.memberOf(r, build.Builder)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	if !found || !builder.IsMember() {
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s built this image and is no longer a member of this host", build.Builder),
			"Build this Package yourself with pkg_build."))
		return
	}
	requester, found, prob := s.memberOf(r, caller.User)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	if !found || !requester.IsMember() {
		writeProblem(w, problem.NotPermitted(r.URL.Path,
			fmt.Sprintf("%s is not a member of this host", caller.User),
			"Ask an administrator to create a member for this account."))
		return
	}

	extendResponse(w, FetchDeadline)
	// The image the record names has to still be there. A member who removed
	// it is a build record that is now only history, and the caller's answer
	// is to build the Package themselves.
	if _, err := s.runner.ImageSize(r.Context(), builder, digest); err != nil {
		if errors.Is(err, sysusers.ErrNoImage) {
			logger.Printf("images: %s no longer has %s", builder.Name, digest)
			writeProblem(w, problem.NotFoundFix(r.URL.Path,
				fmt.Sprintf("%s no longer has the image %s", builder.Name, digest),
				"Build this Package yourself with pkg_build."))
			return
		}
		writeProblem(w, s.fetchProblem(r, err, digest, builder.Name))
		return
	}

	// Each child runs in its own member's leaf: a copy is that member's work
	// and spends that member's memory, see internal/cgroups. A host that
	// cannot place them copies anyway, the same fail open a start makes.
	if err := s.runner.CopyImage(r.Context(), builder, requester, digest,
		s.memberLeaf(r, builder), s.memberLeaf(r, requester)); err != nil {
		writeProblem(w, s.fetchProblem(r, err, digest, builder.Name))
		return
	}
	// The copy is not believed until the requester's own store answers for
	// it: a save and a load that both exited zero and moved nothing is a
	// failure only this call can see.
	size, err := s.runner.ImageSize(r.Context(), requester, digest)
	if err != nil {
		logger.Printf("images: %s was copied from %s to %s and is not in their store: %v",
			digest, builder.Name, requester.Name, err)
		writeProblem(w, problem.InternalDetail(r.URL.Path,
			fmt.Sprintf("copy %s from %s to %s: %v", digest, builder.Name, requester.Name, err),
			"the image was copied and is not in your store",
			"Call pkg_build for this Package instead."))
		return
	}
	logger.Printf("images: %s copied %s from %s (%d bytes)", requester.Name, digest, builder.Name, size)
	writeJSON(w, r.URL.Path, fetchResponse{Digest: digest, Bytes: size, From: builder.Name})
}

// answerLocal answers a fetch of an image the caller built themselves. There
// is nothing to copy, so the one thing left to say is whether they still have
// it.
func (s *Server) answerLocal(w http.ResponseWriter, r *http.Request, caller Caller, digest, builder string) {
	m, found, prob := s.memberOf(r, caller.User)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	if !found || !m.IsMember() {
		writeProblem(w, problem.NotPermitted(r.URL.Path,
			fmt.Sprintf("%s is not a member of this host", caller.User),
			"Ask an administrator to create a member for this account."))
		return
	}
	size, err := s.runner.ImageSize(r.Context(), m, digest)
	if err != nil {
		if errors.Is(err, sysusers.ErrNoImage) {
			writeProblem(w, problem.NotFoundFix(r.URL.Path,
				fmt.Sprintf("you recorded this build and no longer have the image %s", digest),
				"Call pkg_build for this Package again."))
			return
		}
		writeProblem(w, s.fetchProblem(r, err, digest, caller.User))
		return
	}
	writeJSON(w, r.URL.Path, fetchResponse{Digest: digest, Bytes: size, From: builder})
}

// buildRecord is the row that says this image may be copied at all. Without
// one kitbashd has been asked to move an image nobody declared, which is a
// path to any image on the host by digest.
func (s *Server) buildRecord(r *http.Request, folder, digest, from string) (store.Build, *problem.Problem) {
	builds, err := s.store.Builds(r.Context(), store.BuildFilter{
		Path: folder, Digest: digest, Builder: from, Limit: 1,
	})
	if err != nil {
		return store.Build{}, problem.Internal(r.URL.Path, err.Error(), "")
	}
	if len(builds) == 0 {
		detail := fmt.Sprintf("kitbashd has no build of %s with the digest %s", folder, digest)
		if from != "" {
			detail += " by " + from
		}
		return store.Build{}, problem.NotFoundFix(r.URL.Path, detail,
			"Call pkg_build for this Package, which records the build it makes.")
	}
	return builds[0], nil
}

// memberLeaf is the cgroup leaf one member's own processes run in, empty on a
// host that could not make one. A copy that runs unplaced still copies: the
// limits of a member's leaf are not what makes an image arrive.
func (s *Server) memberLeaf(r *http.Request, m sysusers.Member) string {
	leaf, err := s.cgroups.EnsureMember(r.Context(), m.Name, m.UID, m.GID)
	if err != nil {
		logger.Printf("cgroups: the copy for %s runs outside their cgroup: %v", m.Name, err)
		return ""
	}
	return leaf
}

// fetchProblem turns a runtime failure into the answer the agent reads. The
// runtime's own output stays in the daemon log: it carries host paths and the
// image store of a member the caller cannot see.
func (s *Server) fetchProblem(r *http.Request, err error, digest, builder string) *problem.Problem {
	logger.Printf("images: copying %s from %s: %v", digest, builder, err)
	if errors.Is(err, sysusers.ErrTimeout) {
		return problem.InternalDetail(r.URL.Path,
			fmt.Sprintf("copying %s from %s: %v", digest, builder, err),
			"the container runtime did not answer in time",
			"Call pkg_build for this Package, or try the fetch again.")
	}
	return problem.InternalDetail(r.URL.Path,
		fmt.Sprintf("copying %s from %s: %v", digest, builder, err),
		"this image could not be copied into your store",
		"Call pkg_build for this Package instead.")
}

// fetchLock serialises the fetches of one image into one member's store. It
// refuses rather than waits: a caller holding an open request for the minutes
// a copy takes has an answer coming, and a second one would only wait for the
// first and then copy nothing.
type fetchLock struct {
	mu   sync.Mutex
	held map[string]bool
}

func newFetchLock() *fetchLock { return &fetchLock{held: map[string]bool{}} }

// take claims one key and answers what releases it. The second return is
// false when somebody else holds it, which is the 409 of images_fetch.
func (f *fetchLock) take(key string) (func(), bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held[key] {
		return func() {}, false
	}
	f.held[key] = true
	return func() {
		f.mu.Lock()
		delete(f.held, key)
		f.mu.Unlock()
	}, true
}
