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
	"time"

	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// The commit and the image of the build record every test here writes.
const (
	testCommit = "0123456789abcdef0123456789abcdef01234567"
	otherImage = "sha256:" + "cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12"
)

// sharing is a daemon with two members: the caller, who is whoever runs the
// tests, and one other member who built something. It is the shape every
// question about sharing an image has, see PLAN.md section 2.2.
func sharing(t *testing.T, admin bool) (*harness, *sysusers.Fake) {
	t.Helper()
	fake := sysusers.NewFake()
	h := serveWith(t, Options{
		Admin:  func(*user.User) (bool, error) { return admin, nil },
		Users:  fake,
		Runner: fake,
	})
	fake.Add(sysusers.Member{Name: h.user, UID: os.Getuid(), GID: os.Getgid()})
	fake.Add(sysusers.Member{Name: builderName})
	return h, fake
}

// builderName is the other member: the one who built the image this host is
// asked to copy.
const builderName = "kim"

// recordFor writes one build straight to the store, the way a session of that
// member would have recorded it.
func (h *harness) recordFor(builder, path, commit, digest string) {
	h.t.Helper()
	if _, err := h.store.RecordBuild(context.Background(), store.Build{
		Path: path, Commit: commit, Digest: digest, Builder: builder,
		BuiltAt: time.Now().UTC(), Size: 4096,
	}, store.BuildLimits{PerPath: MaxBuildsPerPath, PerBuilder: MaxBuildsPerBuilder}); err != nil {
		h.t.Fatalf("RecordBuild: %v", err)
	}
}

// labelsOfBuild is what a build of one Package at one commit stamps inside the
// image. Every test that stages an image states them, because they are the
// provenance kitbashd checks a record and a copy against.
func labelsOfBuild(path, commit, builder string) map[string]string {
	return map[string]string{
		podman.LabelPath:   path,
		podman.LabelName:   "ffmpeg",
		podman.LabelCommit: commit,
		podman.LabelUser:   builder,
	}
}

// builds calls builds_list.
func (h *harness) builds(query string) (*http.Response, []byte) {
	h.t.Helper()
	return h.do(http.MethodGet, buildsPath+"?"+query, "", nil)
}

// TestABuildIsRecordedForThePeerAndReadBack is the record the whole feature
// rests on: kitbashd knows who built which image of which commit.
func TestABuildIsRecordedForThePeerAndReadBack(t *testing.T) {
	h, fake := sharing(t, false)
	// The record is a claim about an image, so it is only accepted from a
	// member who holds it and whose image says it is that build.
	fake.AddImage(h.user, testDigest, 4096, labelsOfBuild("/org/ffmpeg", testCommit, h.user))

	res, body := h.postJSON(http.MethodPost, buildsPath, buildRequest{
		Path: "/org/ffmpeg", Commit: testCommit, Digest: testDigest, Size: 2048,
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("record = %d %s, want 200", res.StatusCode, body)
	}
	var recorded store.Build
	if err := json.Unmarshal(body, &recorded); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if recorded.Builder != h.user {
		t.Errorf("the builder is %q, want the peer %q", recorded.Builder, h.user)
	}

	res, body = h.builds("path=/org/ffmpeg&commit=" + testCommit)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list = %d %s, want 200", res.StatusCode, body)
	}
	var list buildList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(list.Builds) != 1 {
		t.Fatalf("the commit has %d builds, want the one just recorded", len(list.Builds))
	}
	got := list.Builds[0]
	if got.Digest != testDigest || got.Builder != h.user || got.Size != 2048 {
		t.Errorf("the build is %+v, want the one this member recorded", got)
	}
}

// TestBuildsOfOrgAreReadByEveryMember is why the record exists: another
// member's build is what this member copies instead of building.
func TestBuildsOfOrgAreReadByEveryMember(t *testing.T) {
	h, _ := sharing(t, false)
	h.recordFor(builderName, "/org/ffmpeg", testCommit, testDigest)

	res, body := h.builds("path=/org/ffmpeg")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list = %d %s, want 200", res.StatusCode, body)
	}
	var list buildList
	json.Unmarshal(body, &list)
	if len(list.Builds) != 1 || list.Builds[0].Builder != builderName {
		t.Errorf("the builds are %+v, want the other member's", list.Builds)
	}
}

// TestBuildsOfAnotherMembersHomeAreNotRead keeps a member's private Packages
// private: what somebody has in their home is not a list this API hands out.
func TestBuildsOfAnotherMembersHomeAreNotRead(t *testing.T) {
	h, _ := sharing(t, false)
	h.recordFor(builderName, "/home/"+builderName+"/secret", testCommit, testDigest)

	res, body := h.builds("path=/home/" + builderName + "/secret")
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("list = %d %s, want 403", res.StatusCode, body)
	}
	if p := h.problemOf(res, body); p.Slug() != "not-permitted" {
		t.Errorf("the problem is %s, want not-permitted", p.Slug())
	}
}

// TestAnAdminReadsEveryMembersBuilds is the same rule tel_query follows: an
// admin reads the whole machine.
func TestAnAdminReadsEveryMembersBuilds(t *testing.T) {
	h, _ := sharing(t, true)
	h.recordFor(builderName, "/home/"+builderName+"/secret", testCommit, testDigest)

	res, body := h.builds("path=/home/" + builderName + "/secret")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list = %d %s, want 200", res.StatusCode, body)
	}
	var list buildList
	json.Unmarshal(body, &list)
	if len(list.Builds) != 1 {
		t.Errorf("the admin read %d builds, want the one there is", len(list.Builds))
	}
}

// TestABuildOfAnotherMembersHomeIsNotRecorded stops a member from claiming a
// build in a place they cannot write, which is what a fetch would then be
// pointed at.
func TestABuildOfAnotherMembersHomeIsNotRecorded(t *testing.T) {
	h, fake := sharing(t, false)
	fake.AddImage(h.user, testDigest, 4096,
		labelsOfBuild("/home/"+builderName+"/secret", testCommit, h.user))

	res, body := h.postJSON(http.MethodPost, buildsPath, buildRequest{
		Path: "/home/" + builderName + "/secret", Commit: testCommit, Digest: testDigest,
	})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("record = %d %s, want 403", res.StatusCode, body)
	}
}

// TestAMemberRecordsBuildsOfTheirOwnHome is the other end of the same rule: a
// client that records every build it makes is simpler than one that decides,
// and nothing ever fetches one of these.
func TestAMemberRecordsBuildsOfTheirOwnHome(t *testing.T) {
	h, fake := sharing(t, false)
	home := "/home/" + h.user + "/echo"
	fake.AddImage(h.user, testDigest, 4096, labelsOfBuild(home, testCommit, h.user))

	res, body := h.postJSON(http.MethodPost, buildsPath, buildRequest{
		Path: home, Commit: testCommit, Digest: testDigest,
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("record = %d %s, want 200", res.StatusCode, body)
	}
}

// TestRecordingADigestYouDoNotHoldIsRefused is the first half of what makes a
// record worth reading: a member can only record an image out of their own
// store, which is the store kitbashd would copy it from.
func TestRecordingADigestYouDoNotHoldIsRefused(t *testing.T) {
	h, fake := sharing(t, false)
	// The other member has it; this member does not.
	fake.AddImage(builderName, testDigest, 4096, labelsOfBuild("/org/ffmpeg", testCommit, builderName))

	res, body := h.postJSON(http.MethodPost, buildsPath, buildRequest{
		Path: "/org/ffmpeg", Commit: testCommit, Digest: testDigest,
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("record = %d %s, want 400", res.StatusCode, body)
	}
	// The caller is told what is wrong with what they sent, which is a thing
	// they can act on. It reaches the host as podman 4 exiting 1 and podman 5
	// exiting 125 with "image not known", see sysusers.NoImageOutput.
	if p := h.problemOf(res, body); !strings.Contains(p.Detail, "you do not hold the image") {
		t.Errorf("the detail is %q, want it to say the caller does not hold the image", p.Detail)
	}
	held, err := h.store.Builds(context.Background(), store.BuildFilter{Path: "/org/ffmpeg"})
	if err != nil || len(held) != 0 {
		t.Errorf("the store holds %+v, %v, want nothing recorded", held, err)
	}
}

// TestRecordingWhenTheRuntimeFailsIsInternal is the other side of the same
// mapping: a runtime that could not answer is the host's failure and not the
// caller's, so it is not reported as an image they do not hold.
func TestRecordingWhenTheRuntimeFailsIsInternal(t *testing.T) {
	h, fake := sharing(t, false)
	fake.ImageInfoErr = errors.New("podman: unknown flag: --format")

	res, body := h.postJSON(http.MethodPost, buildsPath, buildRequest{
		Path: "/org/ffmpeg", Commit: testCommit, Digest: testDigest,
	})
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("record = %d %s, want 500", res.StatusCode, body)
	}
	if p := h.problemOf(res, body); p.Slug() != "internal" {
		t.Errorf("the problem is %s, want internal", p.Slug())
	}
}

// TestRecordingAnImageOfAnotherCommitIsRefused is the attack this check exists
// for: recording an old image against the commit somebody else is about to
// build would have their pkg_build answer that image and never build their
// change. The labels are inside the image and the build wrote them.
func TestRecordingAnImageOfAnotherCommitIsRefused(t *testing.T) {
	h, fake := sharing(t, false)
	fake.AddImage(h.user, testDigest, 4096,
		labelsOfBuild("/org/ffmpeg", "1111111111111111111111111111111111111111", h.user))

	res, body := h.postJSON(http.MethodPost, buildsPath, buildRequest{
		Path: "/org/ffmpeg", Commit: testCommit, Digest: testDigest,
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("record = %d %s, want 400", res.StatusCode, body)
	}
	if p := h.problemOf(res, body); !strings.Contains(p.Detail, "not a build of this commit") {
		t.Errorf("the detail is %q, want it to name the commit as the mismatch", p.Detail)
	}
}

// TestRecordingAnImageOfAnotherPackageIsRefused, for the same reason: the
// image's own kitbash.path is what says which Package it is a build of.
func TestRecordingAnImageOfAnotherPackageIsRefused(t *testing.T) {
	h, fake := sharing(t, false)
	fake.AddImage(h.user, testDigest, 4096, labelsOfBuild("/org/elsewhere", testCommit, h.user))

	res, body := h.postJSON(http.MethodPost, buildsPath, buildRequest{
		Path: "/org/ffmpeg", Commit: testCommit, Digest: testDigest,
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("record = %d %s, want 400", res.StatusCode, body)
	}
}

// TestOneMemberCannotTakeOverAnothersRecord: the row names who kitbashd asks
// for a copy, so a member who could rewrite it could point every other
// member's fetch at themselves.
func TestOneMemberCannotTakeOverAnothersRecord(t *testing.T) {
	h, fake := sharing(t, false)
	h.recordFor(builderName, "/org/ffmpeg", testCommit, testDigest)
	// This member holds the same image, correctly labelled, and records it.
	fake.AddImage(h.user, testDigest, 4096, labelsOfBuild("/org/ffmpeg", testCommit, builderName))

	res, body := h.postJSON(http.MethodPost, buildsPath, buildRequest{
		Path: "/org/ffmpeg", Commit: testCommit, Digest: testDigest,
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("record = %d %s, want 200: recording an image you hold is not an error", res.StatusCode, body)
	}
	// The answer is the row as it stands. A caller told they are the builder
	// while the table says otherwise would go on to fetch from themselves.
	var answered store.Build
	if err := json.Unmarshal(body, &answered); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if answered.Builder != builderName {
		t.Errorf("the answer names the builder %q, want the stored %q", answered.Builder, builderName)
	}
	held, err := h.store.Builds(context.Background(), store.BuildFilter{Path: "/org/ffmpeg"})
	if err != nil {
		t.Fatalf("Builds: %v", err)
	}
	if len(held) != 1 || held[0].Builder != builderName {
		t.Errorf("the record is %+v, want the member who recorded it first", held)
	}
	if !answered.BuiltAt.Equal(held[0].BuiltAt) || answered.Digest != held[0].Digest {
		t.Errorf("the answer is %+v and the row is %+v; they must not disagree", answered, held[0])
	}
}

// TestABuildRecordIsValidated refuses everything a fetch could not act on.
func TestABuildRecordIsValidated(t *testing.T) {
	h, _ := sharing(t, false)
	for name, req := range map[string]buildRequest{
		"a relative path":  {Path: "org/ffmpeg", Commit: testCommit, Digest: testDigest},
		"a parent segment": {Path: "/org/../etc/ffmpeg", Commit: testCommit, Digest: testDigest},
		"a short commit":   {Path: "/org/ffmpeg", Commit: "abc", Digest: testDigest},
		"a tag as commit":  {Path: "/org/ffmpeg", Commit: strings.Repeat("z", 40), Digest: testDigest},
		"no digest":        {Path: "/org/ffmpeg", Commit: testCommit},
		"a name as digest": {Path: "/org/ffmpeg", Commit: testCommit, Digest: "localhost/kitbash/ffmpeg"},
		"a negative size":  {Path: "/org/ffmpeg", Commit: testCommit, Digest: testDigest, Size: -1},
	} {
		res, body := h.postJSON(http.MethodPost, buildsPath, req)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", name, res.StatusCode, body)
		}
	}
}

// TestListingBuildsNeedsAPath keeps this path from answering the whole table
// to anybody who asks for nothing.
func TestListingBuildsNeedsAPath(t *testing.T) {
	h, _ := sharing(t, false)
	res, body := h.builds("")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("list = %d %s, want 400", res.StatusCode, body)
	}
}

// TestRemovingAMemberRemovesTheirBuilds: a row naming an account that is gone
// points at an image store that went with it.
func TestRemovingAMemberRemovesTheirBuilds(t *testing.T) {
	h, fake := sharing(t, true)
	h.recordFor(builderName, "/org/ffmpeg", testCommit, testDigest)

	res, body := h.do(http.MethodDelete, usersPath+"/"+builderName, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("remove = %d %s, want 200", res.StatusCode, body)
	}
	if len(fake.Removed) != 1 {
		t.Fatalf("the host removed %v, want the one member", fake.Removed)
	}
	builds, err := h.store.Builds(context.Background(), store.BuildFilter{Path: "/org/ffmpeg"})
	if err != nil {
		t.Fatalf("Builds: %v", err)
	}
	if len(builds) != 0 {
		t.Errorf("the builds left are %+v, want none of a member who is gone", builds)
	}
}
