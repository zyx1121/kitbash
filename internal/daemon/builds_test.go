package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/user"
	"strings"
	"testing"
	"time"

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
	if err := h.store.RecordBuild(context.Background(), store.Build{
		Path: path, Commit: commit, Digest: digest, Builder: builder,
		BuiltAt: time.Now().UTC(), Size: 4096,
	}, MaxBuildsPerPath); err != nil {
		h.t.Fatalf("RecordBuild: %v", err)
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
	h, _ := sharing(t, false)

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
	h, _ := sharing(t, false)

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
	h, _ := sharing(t, false)

	res, body := h.postJSON(http.MethodPost, buildsPath, buildRequest{
		Path: "/home/" + h.user + "/echo", Commit: testCommit, Digest: testDigest,
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("record = %d %s, want 200", res.StatusCode, body)
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
