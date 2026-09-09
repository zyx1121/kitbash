package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/user"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/cgroups"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// fetch calls images_fetch for one digest.
func (h *harness) fetch(digest string, req fetchRequest) (*http.Response, []byte) {
	h.t.Helper()
	return h.postJSON(http.MethodPost, imagesPath+"/"+digest+"/fetch", req)
}

// TestFetchCopiesTheImageBetweenTwoMembers is the acceptance sentence of issue
// 68: an /org Package one member built is copied into another member's store
// rather than built again, as a save by the first into a load by the second.
func TestFetchCopiesTheImageBetweenTwoMembers(t *testing.T) {
	h, fake := sharing(t, false)
	h.recordFor(builderName, "/org/ffmpeg", testCommit, testDigest)
	fake.AddImage(builderName, testDigest, 4096)

	res, body := h.fetch(testDigest, fetchRequest{Path: "/org/ffmpeg", From: builderName})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("fetch = %d %s, want 200", res.StatusCode, body)
	}
	var answer fetchResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if answer.Digest != testDigest || answer.From != builderName || answer.Bytes != 4096 {
		t.Errorf("the answer is %+v, want the image, its size and who it came from", answer)
	}

	copies := fake.Copies()
	if len(copies) != 1 {
		t.Fatalf("the runtime was asked for %d copies, want one", len(copies))
	}
	got := copies[0]
	if got.From != builderName || got.To != h.user || got.Digest != testDigest {
		t.Errorf("the copy is %+v, want %s to %s", got, builderName, h.user)
	}
	// Each child runs in its own member's leaf: a copy is that member's work
	// and spends that member's memory, not the other's and not the daemon's.
	if got.FromCgroup != cgroups.LeafDir(h.cgroups.Base, builderName) {
		t.Errorf("the save ran in %q, want the builder's leaf", got.FromCgroup)
	}
	if got.ToCgroup != cgroups.LeafDir(h.cgroups.Base, h.user) {
		t.Errorf("the load ran in %q, want the requester's leaf", got.ToCgroup)
	}
	if !fake.HasImage(h.user, testDigest) {
		t.Error("the requester does not have the image the copy answered for")
	}
}

// TestFetchWithoutABuildRecordIsNotFound is what keeps this path from being a
// way to copy any image on the host by digest: only a build somebody declared
// may be copied.
func TestFetchWithoutABuildRecordIsNotFound(t *testing.T) {
	h, fake := sharing(t, false)
	fake.AddImage(builderName, testDigest, 4096)

	res, body := h.fetch(testDigest, fetchRequest{Path: "/org/ffmpeg", From: builderName})
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("fetch = %d %s, want 404", res.StatusCode, body)
	}
	if len(fake.Copies()) != 0 {
		t.Errorf("the runtime was asked to copy %+v, want nothing", fake.Copies())
	}
}

// TestFetchOfAnImageTheBuilderNoLongerHasIsNotFound: the record is history and
// the store is the truth, so the caller is told to build it themselves.
func TestFetchOfAnImageTheBuilderNoLongerHasIsNotFound(t *testing.T) {
	h, fake := sharing(t, false)
	h.recordFor(builderName, "/org/ffmpeg", testCommit, testDigest)

	res, body := h.fetch(testDigest, fetchRequest{Path: "/org/ffmpeg", From: builderName})
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("fetch = %d %s, want 404", res.StatusCode, body)
	}
	p := h.problemOf(res, body)
	if p.Fix == "" {
		t.Error("the problem carries no fix, and building it is the one the caller has")
	}
	if len(fake.Copies()) != 0 {
		t.Errorf("the runtime was asked to copy %+v, want nothing", fake.Copies())
	}
}

// TestFetchOfAnotherMembersHomeIsNotPermitted: kitbashd is root, so it must
// not read a private image store on behalf of somebody the kernel keeps out.
func TestFetchOfAnotherMembersHomeIsNotPermitted(t *testing.T) {
	h, fake := sharing(t, false)
	h.recordFor(builderName, "/home/"+builderName+"/secret", testCommit, testDigest)
	fake.AddImage(builderName, testDigest, 4096)

	res, body := h.fetch(testDigest, fetchRequest{
		Path: "/home/" + builderName + "/secret", From: builderName,
	})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("fetch = %d %s, want 403", res.StatusCode, body)
	}
	if len(fake.Copies()) != 0 {
		t.Errorf("the runtime was asked to copy %+v, want nothing", fake.Copies())
	}
}

// TestFetchOfTheCallersOwnBuildCopiesNothing is the no-op: a member asking for
// an image they built is told what they have, and no child runs.
func TestFetchOfTheCallersOwnBuildCopiesNothing(t *testing.T) {
	h, fake := sharing(t, false)
	h.recordFor(h.user, "/org/ffmpeg", testCommit, testDigest)
	fake.AddImage(h.user, testDigest, 512)

	res, body := h.fetch(testDigest, fetchRequest{Path: "/org/ffmpeg", From: h.user})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("fetch = %d %s, want 200", res.StatusCode, body)
	}
	var answer fetchResponse
	json.Unmarshal(body, &answer)
	if answer.Bytes != 512 || answer.From != h.user {
		t.Errorf("the answer is %+v, want the image this member already has", answer)
	}
	if len(fake.Copies()) != 0 {
		t.Errorf("the runtime was asked to copy %+v, want nothing", fake.Copies())
	}
}

// TestFetchThatDeliversNothingIsInternal is the verification step: a save and
// a load that both exited zero and moved nothing is a failure only this call
// can see.
func TestFetchThatDeliversNothingIsInternal(t *testing.T) {
	h, fake := sharing(t, false)
	h.recordFor(builderName, "/org/ffmpeg", testCommit, testDigest)
	fake.AddImage(builderName, testDigest, 4096)
	fake.LoseCopies = true

	res, body := h.fetch(testDigest, fetchRequest{Path: "/org/ffmpeg", From: builderName})
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("fetch = %d %s, want 500", res.StatusCode, body)
	}
	p := h.problemOf(res, body)
	if p.Slug() != "internal" {
		t.Errorf("the problem is %s, want internal", p.Slug())
	}
	// The cause of an internal problem is the operator's, never the agent's.
	if p.Detail == "" || p.Detail == p.Instance {
		t.Errorf("the problem details are %+v, want a sentence the agent can act on", p)
	}
}

// TestFetchOfAnImageThatIsNotADigestIsRefused before anything runs. A name
// that carries slashes is not this path at all, which the API answers as the
// path it does not serve.
func TestFetchOfAnImageThatIsNotADigestIsRefused(t *testing.T) {
	h, _ := sharing(t, false)
	res, body := h.fetch("sha256:ffmpeg", fetchRequest{Path: "/org/ffmpeg"})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("fetch = %d %s, want 400", res.StatusCode, body)
	}
	res, body = h.fetch("localhost/kitbash/ffmpeg:abc", fetchRequest{Path: "/org/ffmpeg"})
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("fetch of a tag = %d %s, want 404", res.StatusCode, body)
	}
}

// blockingRunner holds one copy open, so a second fetch of the same image by
// the same member arrives while the first is still running.
type blockingRunner struct {
	*sysusers.Fake
	started chan struct{}
	release chan struct{}
}

func (b *blockingRunner) CopyImage(ctx context.Context, from, to sysusers.Member, digest, fromCgroup, toCgroup string) error {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-b.release
	return b.Fake.CopyImage(ctx, from, to, digest, fromCgroup, toCgroup)
}

// TestOneFetchPerMemberAndImageAtATime: two loads into one store for one image
// is work the runtime does twice, so the second caller is refused rather than
// queued behind minutes of copying.
func TestOneFetchPerMemberAndImageAtATime(t *testing.T) {
	fake := sysusers.NewFake()
	runner := &blockingRunner{Fake: fake, started: make(chan struct{}, 1), release: make(chan struct{})}
	h := serveWith(t, Options{
		Admin:  func(*user.User) (bool, error) { return false, nil },
		Users:  fake,
		Runner: runner,
	})
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid()})
	fake.Add(sysusers.Member{Name: builderName})
	h.recordFor(builderName, "/org/ffmpeg", testCommit, testDigest)
	fake.AddImage(builderName, testDigest, 4096)

	first := make(chan int, 1)
	go func() {
		res, _ := h.fetch(testDigest, fetchRequest{Path: "/org/ffmpeg", From: builderName})
		first <- res.StatusCode
	}()
	<-runner.started

	res, body := h.fetch(testDigest, fetchRequest{Path: "/org/ffmpeg", From: builderName})
	if res.StatusCode != http.StatusConflict {
		t.Errorf("the second fetch = %d %s, want 409", res.StatusCode, body)
	}
	close(runner.release)
	if status := <-first; status != http.StatusOK {
		t.Errorf("the first fetch = %d, want 200", status)
	}
	// Once the first is over the second is served like any other.
	res, body = h.fetch(testDigest, fetchRequest{Path: "/org/ffmpeg", From: builderName})
	if res.StatusCode != http.StatusOK {
		t.Errorf("the fetch after = %d %s, want 200", res.StatusCode, body)
	}
}

// TestFetchReportsARuntimeFailureAsInternal keeps the runtime's own output in
// the daemon log: it names host paths and another member's store.
func TestFetchReportsARuntimeFailureAsInternal(t *testing.T) {
	h, fake := sharing(t, false)
	h.recordFor(builderName, "/org/ffmpeg", testCommit, testDigest)
	fake.AddImage(builderName, testDigest, 4096)
	fake.CopyErr = errors.New("podman save: the store is locked by /var/lib/containers/lock")

	res, body := h.fetch(testDigest, fetchRequest{Path: "/org/ffmpeg", From: builderName})
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("fetch = %d %s, want 500", res.StatusCode, body)
	}
	if p := h.problemOf(res, body); strings.Contains(p.Detail, "/var/lib/containers/lock") {
		t.Errorf("the detail %q carries the runtime's own output", p.Detail)
	}
}
