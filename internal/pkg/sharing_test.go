package pkg_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/pkg"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
)

// An /org Package built by one member is copied to the next instead of being
// built again, see PLAN.md section 2.2 and issue 68. These tests stand a
// temporary folder in for /org, the same way the approvals tests do.

// otherDigest is the image the other member built, and otherCommit the commit
// its labels carry.
const (
	otherDigest = "sha256:abababababababababababababababababababababababababababababababab"
	otherCommit = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
)

// fakeBuilds is the build registry of kitbashd as this package sees it. It
// records what it was asked and answers what the test staged, which is what a
// second member's session would have left behind.
type fakeBuilds struct {
	// known is what kitbashd holds, newest first.
	known []telemetry.Build
	// runtime is the image store a fetch loads into, so the session reads the
	// copied image back the way it would from its own podman.
	runtime *podman.Fake
	// delivers is the image a fetch of one digest puts in that store, labels
	// and all. It is stated by the test rather than derived from the record:
	// the labels are the provenance the session checks, so a fake that wrote
	// them from the record would prove nothing about the check.
	delivers map[string]podman.Image

	// The failures a test stages.
	listErr   *problem.Problem
	fetchErr  *problem.Problem
	recordErr *problem.Problem

	// What this registry was asked for, and the counter that gives a record
	// the time the daemon would have stamped on it.
	asked    []string
	fetched  []string
	recorded []telemetry.Build
	stamps   int
}

func (f *fakeBuilds) Builds(_ context.Context, path, commit, digest string) ([]telemetry.Build, *problem.Problem) {
	f.asked = append(f.asked, path+" "+commit+" "+digest)
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := []telemetry.Build{}
	for _, b := range f.known {
		if b.Path != path {
			continue
		}
		if commit != "" && b.Commit != commit {
			continue
		}
		if digest != "" && b.Digest != digest {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

func (f *fakeBuilds) RecordBuild(_ context.Context, b telemetry.Build) *problem.Problem {
	f.recorded = append(f.recorded, b)
	if f.recordErr != nil {
		return f.recordErr
	}
	// The daemon stamps the peer and the time, so the record a later call
	// reads back carries both.
	b.Builder = "tester"
	if b.BuiltAt == "" {
		f.stamps++
		b.BuiltAt = fmt.Sprintf("2026-01-01T00:00:%02dZ", f.stamps)
	}
	f.known = append([]telemetry.Build{b}, f.known...)
	return nil
}

func (f *fakeBuilds) FetchImage(_ context.Context, digest, path, from string) (*telemetry.FetchResult, *problem.Problem) {
	f.fetched = append(f.fetched, digest+" "+path+" "+from)
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	// kitbashd loads the image into this member's own store, which is where
	// the session reads it back from. What arrives is what the test staged,
	// labels included: nothing here derives them from the record.
	if image, staged := f.delivers[digest]; staged && f.runtime != nil {
		f.runtime.AddImage(image)
	}
	return &telemetry.FetchResult{Digest: digest, Bytes: 4096, From: from}, nil
}

// deliver stages the image one fetch of a digest puts in the caller's store.
func (f *fakeBuilds) deliver(image podman.Image) {
	if f.delivers == nil {
		f.delivers = map[string]podman.Image{}
	}
	f.delivers[image.ID] = image
}

// shared is a fixture whose root stands in for /org and whose service talks to
// a build registry.
func shared(t *testing.T) (*fixture, *fakeBuilds) {
	t.Helper()
	f := newFixture(t)
	f.files.SetShared(f.root)
	builds := &fakeBuilds{runtime: f.runner}
	f.packages.SetBuilds(builds)
	return f, builds
}

// buildOf is one image as a build of this Package at this commit would have
// been labelled. Every test that stages an image states its labels this way,
// or states different ones on purpose.
func buildOf(digest, folder, commit, builder string) podman.Image {
	return podman.Image{ID: digest, Labels: map[string]string{
		podman.LabelPath:   folder,
		podman.LabelName:   "ffmpeg",
		podman.LabelCommit: commit,
		podman.LabelUser:   builder,
	}}
}

// pack writes one Package and answers its folder and its commit.
func pack(t *testing.T, f *fixture) (folder, commit string) {
	t.Helper()
	folder = f.commit(t, "ffmpeg",
		fs.File{Path: "kitbash.yaml", Content: text(containerManifest)},
		fs.File{Path: "Containerfile", Content: text("FROM alpine\n")})
	head, prob := f.files.Head(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Head: %s", prob.Detail)
	}
	return folder, head.Sha
}

// TestBuildCopiesAnotherMembersImageInsteadOfBuilding is the acceptance
// sentence of issue 68 on the session side: one commit is one digest for
// everybody.
func TestBuildCopiesAnotherMembersImageInsteadOfBuilding(t *testing.T) {
	f, builds := shared(t)
	folder, commit := pack(t, f)
	builds.known = []telemetry.Build{
		{Path: folder, Commit: commit, Digest: otherDigest, Builder: "kim", BuiltAt: "2026-01-01T00:00:00Z"},
	}
	// What kitbashd copies in is an image whose own labels say it is a build
	// of this Package at this commit, which is what the session checks.
	builds.deliver(buildOf(otherDigest, folder, commit, "kim"))

	out, prob := f.packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if out.Digest != otherDigest {
		t.Errorf("the digest is %s, want the one the other member built", out.Digest)
	}
	if out.Commit != commit {
		t.Errorf("the commit is %s, want the head of this Package", out.Commit)
	}
	if out.Log != "copied from kim" {
		t.Errorf("the log is %q, want it to say the image was copied and from whom", out.Log)
	}
	if len(f.runner.Builds) != 0 {
		t.Errorf("the runtime built %+v, want the copy instead", f.runner.Builds)
	}
	if len(builds.fetched) != 1 || !strings.HasPrefix(builds.fetched[0], otherDigest+" "+folder+" kim") {
		t.Errorf("the fetches are %v, want the one image of the other member", builds.fetched)
	}
	// The copied image is tagged the way a local build would have tagged it,
	// so podman images reads the same either way.
	if len(f.runner.Tags) != 1 {
		t.Fatalf("the runtime was asked for %d tags, want one for the copied image", len(f.runner.Tags))
	}
	tag := f.runner.Tags[0]
	if tag.Image != otherDigest || tag.Tag != pkg.TagPrefix+"ffmpeg:"+commit[:12] {
		t.Errorf("the tag is %+v, want the copied image under the tag a build writes", tag)
	}
	// A copy is not recorded: the record still names the member who built it.
	if len(builds.recorded) != 0 {
		t.Errorf("the copy was recorded as %+v, want the builder's record left alone", builds.recorded)
	}
}

// TestBuildRecordsWhatItBuilt is the other half: the next member reads this
// and copies instead of building.
func TestBuildRecordsWhatItBuilt(t *testing.T) {
	f, builds := shared(t)
	folder, commit := pack(t, f)

	out, prob := f.packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if len(f.runner.Builds) != 1 {
		t.Fatalf("the runtime was asked for %d builds, want the one it had to make", len(f.runner.Builds))
	}
	if len(builds.recorded) != 1 {
		t.Fatalf("the builds recorded are %+v, want the one just made", builds.recorded)
	}
	got := builds.recorded[0]
	if got.Path != folder || got.Commit != commit || got.Digest != out.Digest {
		t.Errorf("the record is %+v, want this Package, this commit and this image", got)
	}
	if got.Builder != "" {
		t.Errorf("the record names the builder %q; kitbashd stamps the peer", got.Builder)
	}
}

// TestBuildFallsBackToBuildingWhenTheCopyFails: a member is never left without
// an image because another member's store could not be read.
func TestBuildFallsBackToBuildingWhenTheCopyFails(t *testing.T) {
	f, builds := shared(t)
	folder, commit := pack(t, f)
	builds.known = []telemetry.Build{
		{Path: folder, Commit: commit, Digest: otherDigest, Builder: "kim"},
	}
	builds.fetchErr = problem.Internal(folder, "the runtime is not answering", "")

	out, prob := f.packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if len(f.runner.Builds) != 1 {
		t.Fatalf("the runtime was asked for %d builds, want the fallback build", len(f.runner.Builds))
	}
	if out.Digest == otherDigest {
		t.Error("the digest is the other member's, and that copy failed")
	}
	if out.Log == "" || strings.HasPrefix(out.Log, "copied from") {
		t.Errorf("the log is %q, want the log of a build that ran here", out.Log)
	}
}

// TestBuildBuildsWhenTheDaemonWillNotAnswer: a host whose kitbashd is down
// builds the way it did before any of this existed.
func TestBuildBuildsWhenTheDaemonWillNotAnswer(t *testing.T) {
	f, builds := shared(t)
	folder, _ := pack(t, f)
	builds.listErr = problem.Internal(folder, "no kitbashd on this host", telemetry.NotRunningFix)
	builds.recordErr = problem.Internal(folder, "no kitbashd on this host", telemetry.NotRunningFix)

	out, prob := f.packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if len(f.runner.Builds) != 1 || out.Digest == "" {
		t.Errorf("the build made %+v, want one local build all the same", f.runner.Builds)
	}
}

// TestBuildOfAHomePackageIsNeitherFetchedNorRecorded: a Package in a home is
// that member's alone, so kitbashd is not asked about it and not told about it.
func TestBuildOfAHomePackageIsNeitherFetchedNorRecorded(t *testing.T) {
	f, builds := shared(t)
	// The service's shared root is the fixture root, so a Package outside it
	// is a Package in a home as far as this rule is concerned.
	f.files.SetShared("/org")
	folder, _ := pack(t, f)

	if _, prob := f.packages.Build(context.Background(), folder); prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if len(builds.asked) != 0 || len(builds.recorded) != 0 || len(builds.fetched) != 0 {
		t.Errorf("kitbashd was asked %v, told %+v and fetched %v for a Package in a home",
			builds.asked, builds.recorded, builds.fetched)
	}
}

// TestBuildReturnsTheImageOfThisCommitItAlreadyHas: one commit is one digest,
// so a commit whose image is in this member's store is neither copied nor
// built again. Building it again would answer a second digest for a commit
// that already has one.
func TestBuildReturnsTheImageOfThisCommitItAlreadyHas(t *testing.T) {
	f, builds := shared(t)
	folder, commit := pack(t, f)
	f.runner.AddImage(buildOf(otherDigest, folder, commit, "kim"))
	builds.known = []telemetry.Build{
		{Path: folder, Commit: commit, Digest: otherDigest, Builder: "kim", BuiltAt: "2026-01-01T00:00:00Z"},
	}

	out, prob := f.packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if out.Digest != otherDigest || out.Commit != commit {
		t.Errorf("the build is %+v, want the image of this commit that is already here", out)
	}
	if out.Log != "already built at "+otherDigest {
		t.Errorf("the log is %q, want it to say the image was already built", out.Log)
	}
	if len(builds.fetched) != 0 {
		t.Errorf("the session fetched %v, want nothing for an image it has", builds.fetched)
	}
	if len(f.runner.Builds) != 0 {
		t.Errorf("the runtime built %+v, want the image that was already here", f.runner.Builds)
	}
	// Nothing was built, so there is nothing new to record: the row that named
	// this digest still names the member who made it.
	if len(builds.recorded) != 0 {
		t.Errorf("the build was recorded as %+v, want the record left alone", builds.recorded)
	}
}

// TestBuildReturnsItsOwnImageOfThisCommit is the same rule for the member who
// made it: a second pkg_build of one commit answers the digest of the first.
func TestBuildReturnsItsOwnImageOfThisCommit(t *testing.T) {
	f, builds := shared(t)
	folder, commit := pack(t, f)

	first, prob := f.packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	// The first build recorded itself, which is what the second one reads.
	builds.known[0].Builder = "tester"

	second, prob := f.packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build again: %s", prob.Detail)
	}
	if second.Digest != first.Digest || second.Commit != commit {
		t.Errorf("the second build is %+v, want the digest of the first", second)
	}
	if second.Log != "already built at "+first.Digest {
		t.Errorf("the log is %q, want it to say the image was already built", second.Log)
	}
	if len(f.runner.Builds) != 1 {
		t.Errorf("the runtime made %d builds, want the one the first call asked for", len(f.runner.Builds))
	}
}

// TestBuildRebuildsAnImageThisMemberRemoved: the record names this member and
// the image is not in their store, so they are building it again on purpose
// and nothing is copied from themselves.
func TestBuildRebuildsAnImageThisMemberRemoved(t *testing.T) {
	f, builds := shared(t)
	folder, commit := pack(t, f)
	builds.known = []telemetry.Build{
		{Path: folder, Commit: commit, Digest: otherDigest, Builder: "tester"},
	}

	out, prob := f.packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if len(f.runner.Builds) != 1 || out.Digest == otherDigest {
		t.Errorf("the build is %+v after %d local builds, want a build that ran here",
			out, len(f.runner.Builds))
	}
	if len(builds.fetched) != 0 {
		t.Errorf("the session fetched %v, want nothing of its own", builds.fetched)
	}
}

// TestAPoisonedRecordDoesNotSkipTheBuild is the attack the record makes
// possible if nothing checks it: another member records the current commit
// against an old image this member happens to hold, and this member's
// pkg_build would answer that image and never build the change they just
// wrote. The image's own labels say which commit it is a build of, and they
// are what decides.
func TestAPoisonedRecordDoesNotSkipTheBuild(t *testing.T) {
	f, builds := shared(t)
	folder, commit := pack(t, f)
	// An image this member holds, which is a build of an older commit of the
	// same Package.
	f.runner.AddImage(buildOf(otherDigest, folder, otherCommit, "tester"))
	// Another member records it against the commit that is being built now.
	builds.known = []telemetry.Build{
		{Path: folder, Commit: commit, Digest: otherDigest, Builder: "kim", BuiltAt: "2026-01-01T00:00:00Z"},
	}

	out, prob := f.packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if out.Digest == otherDigest {
		t.Errorf("the build answered %s, which is a build of another commit", out.Digest)
	}
	if len(f.runner.Builds) != 1 {
		t.Errorf("the runtime made %d builds, want the build this member asked for", len(f.runner.Builds))
	}
	if len(builds.fetched) != 0 {
		t.Errorf("the session fetched %v for an image it holds and does not trust", builds.fetched)
	}
}

// TestACopiedImageWithTheWrongLabelsIsNotAccepted: kitbashd checks the far
// end, and this is the end that hands the digest to the caller as a build of
// their commit.
func TestACopiedImageWithTheWrongLabelsIsNotAccepted(t *testing.T) {
	f, builds := shared(t)
	folder, commit := pack(t, f)
	builds.known = []telemetry.Build{
		{Path: folder, Commit: commit, Digest: otherDigest, Builder: "kim", BuiltAt: "2026-01-01T00:00:00Z"},
	}
	// What arrives is a build of another Package altogether.
	builds.deliver(buildOf(otherDigest, "/org/elsewhere", commit, "kim"))

	out, prob := f.packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if out.Digest == otherDigest {
		t.Errorf("the build answered %s, which is a build of another Package", out.Digest)
	}
	if len(builds.fetched) != 1 {
		t.Errorf("the fetches are %v, want the one that was tried", builds.fetched)
	}
	if len(f.runner.Builds) != 1 {
		t.Errorf("the runtime made %d builds, want the fallback build", len(f.runner.Builds))
	}
	if len(f.runner.Tags) != 0 {
		t.Errorf("the runtime tagged %+v, want nothing tagged as this Package", f.runner.Tags)
	}
}

// TestACopyThatDeliveredNothingIsNotAccepted: the answer is not the image, and
// only this member's own store says whether it arrived.
func TestACopyThatDeliveredNothingIsNotAccepted(t *testing.T) {
	f, builds := shared(t)
	folder, commit := pack(t, f)
	builds.known = []telemetry.Build{
		{Path: folder, Commit: commit, Digest: otherDigest, Builder: "kim", BuiltAt: "2026-01-01T00:00:00Z"},
	}
	// Nothing is staged, so the fetch answers and no image arrives.

	out, prob := f.packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if out.Digest == otherDigest || len(f.runner.Builds) != 1 {
		t.Errorf("the build answered %+v after %d builds, want a build that ran here",
			out, len(f.runner.Builds))
	}
}

// TestTheEarliestRecordOfACommitWins, so two members reading the same records
// pick the same digest. Whoever recorded last deciding for everybody is how
// one commit ends up with two digests.
func TestTheEarliestRecordOfACommitWins(t *testing.T) {
	f, builds := shared(t)
	folder, commit := pack(t, f)
	first := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	// The registry answers newest first, and the oldest record is the one
	// that wins.
	builds.known = []telemetry.Build{
		{Path: folder, Commit: commit, Digest: otherDigest, Builder: "dana", BuiltAt: "2026-01-02T00:00:00Z"},
		{Path: folder, Commit: commit, Digest: first, Builder: "kim", BuiltAt: "2026-01-01T00:00:00Z"},
	}
	builds.deliver(buildOf(first, folder, commit, "kim"))
	builds.deliver(buildOf(otherDigest, folder, commit, "dana"))

	out, prob := f.packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if out.Digest != first {
		t.Errorf("the digest is %s, want the earliest record %s", out.Digest, first)
	}
	if out.Log != "copied from kim" {
		t.Errorf("the log is %q, want the copy from the member who recorded it first", out.Log)
	}
	if len(builds.fetched) != 1 || !strings.HasPrefix(builds.fetched[0], first+" ") {
		t.Errorf("the fetches are %v, want the one image every member converges on", builds.fetched)
	}
}

// TestACopyThatCannotBeTaggedIsStillACopy: the tag is a name, not an image.
func TestACopyThatCannotBeTaggedIsStillACopy(t *testing.T) {
	f, builds := shared(t)
	folder, commit := pack(t, f)
	builds.known = []telemetry.Build{
		{Path: folder, Commit: commit, Digest: otherDigest, Builder: "kim", BuiltAt: "2026-01-01T00:00:00Z"},
	}
	builds.deliver(buildOf(otherDigest, folder, commit, "kim"))
	f.runner.TagErr = errors.New("podman tag: the store is busy")

	out, prob := f.packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	if out.Digest != otherDigest {
		t.Errorf("the digest is %s, want the copied image", out.Digest)
	}
}

// TestInspectMergesTheBuildsOfEveryMember: an agent reading an /org Package
// sees who built what, which the local image store cannot say.
func TestInspectMergesTheBuildsOfEveryMember(t *testing.T) {
	f, builds := shared(t)
	folder, commit := pack(t, f)
	out, prob := f.packages.Build(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Build: %s", prob.Detail)
	}
	// One build of this member's, recorded by the build above, and one of
	// another member's that never reached this store.
	builds.known = append(builds.known, telemetry.Build{
		Path: folder, Commit: commit, Digest: otherDigest, Builder: "kim",
		BuiltAt: "2020-01-01T00:00:00Z",
	})
	builds.recorded[0].Builder = "tester"
	builds.known[0].Builder = "tester"

	inspected, prob := f.packages.Inspect(context.Background(), folder)
	if prob != nil {
		t.Fatalf("Inspect: %s", prob.Detail)
	}
	if len(inspected.Builds) != 2 {
		t.Fatalf("the builds are %+v, want the local one and the other member's", inspected.Builds)
	}
	byDigest := map[string]pkg.Build{}
	for _, b := range inspected.Builds {
		byDigest[b.Digest] = b
	}
	local, held := byDigest[out.Digest]
	if !held || local.Builder != "tester" {
		t.Errorf("the local build is %+v, want it named after this member", local)
	}
	remote, held := byDigest[otherDigest]
	if !held || remote.Builder != "kim" || remote.Commit != commit {
		t.Errorf("the remote build is %+v, want the other member's record", remote)
	}
	// Newest first is what the tool publishes, and the other member's build is
	// the older of the two.
	if inspected.Builds[0].Digest != out.Digest {
		t.Errorf("the first build is %+v, want the newest", inspected.Builds[0])
	}
}

// TestInspectOfAHomePackageAsksKitbashdNothing keeps a member's own Packages
// out of the registry entirely.
func TestInspectOfAHomePackageAsksKitbashdNothing(t *testing.T) {
	f, builds := shared(t)
	f.files.SetShared("/org")
	folder, _ := pack(t, f)

	if _, prob := f.packages.Inspect(context.Background(), folder); prob != nil {
		t.Fatalf("Inspect: %s", prob.Detail)
	}
	if len(builds.asked) != 0 {
		t.Errorf("kitbashd was asked %v about a Package in a home", builds.asked)
	}
}
