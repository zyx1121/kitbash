package store_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/store"
)

// openBuildStore is a store in a temporary directory, closed when the test
// ends.
func openBuildStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "kitbashd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// sha is one commit of the shape a build record carries.
func sha(seed byte) string { return strings.Repeat(fmt.Sprintf("%02x", seed), 20) }

// imageID is one image id of the shape a build record carries.
func imageID(seed byte) string { return "sha256:" + strings.Repeat(fmt.Sprintf("%02x", seed), 32) }

func build(path, commit, digest, builder string, at time.Time) store.Build {
	return store.Build{
		Path: path, Commit: commit, Digest: digest, Builder: builder, BuiltAt: at, Size: 1024,
	}
}

// TestBuildsAreListedNewestFirst is what pkg_build reads: the newest record of
// this commit, so the copy it makes is of the image somebody has now.
func TestBuildsAreListedNewestFirst(t *testing.T) {
	st := openBuildStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	for i, b := range []store.Build{
		build("/org/ffmpeg", sha(1), imageID(1), "loki", now.Add(-2*time.Hour)),
		build("/org/ffmpeg", sha(1), imageID(2), "kim", now.Add(-time.Hour)),
		build("/org/other", sha(3), imageID(3), "loki", now),
	} {
		if err := st.RecordBuild(ctx, b, store.BuildLimits{}); err != nil {
			t.Fatalf("RecordBuild %d: %v", i, err)
		}
	}

	builds, err := st.Builds(ctx, store.BuildFilter{Path: "/org/ffmpeg"})
	if err != nil {
		t.Fatalf("Builds: %v", err)
	}
	if len(builds) != 2 {
		t.Fatalf("the path has %d builds, want the two of it and not the third path's", len(builds))
	}
	if builds[0].Digest != imageID(2) || builds[0].Builder != "kim" {
		t.Errorf("the first build is %+v, want the newest one", builds[0])
	}
	if !builds[0].BuiltAt.Equal(now.Add(-time.Hour)) {
		t.Errorf("builtAt is %s, want the time it was recorded with", builds[0].BuiltAt)
	}
	if builds[0].Size != 1024 {
		t.Errorf("size is %d, want the recorded one", builds[0].Size)
	}

	// The commit is the filter pkg_build sends, and the digest the one
	// proc_run sends.
	byCommit, err := st.Builds(ctx, store.BuildFilter{Path: "/org/ffmpeg", Commit: sha(1)})
	if err != nil || len(byCommit) != 2 {
		t.Fatalf("Builds by commit = %d, %v, want the two of that commit", len(byCommit), err)
	}
	byDigest, err := st.Builds(ctx, store.BuildFilter{Path: "/org/ffmpeg", Digest: imageID(1)})
	if err != nil || len(byDigest) != 1 || byDigest[0].Builder != "loki" {
		t.Fatalf("Builds by digest = %+v, %v, want the one build with that image", byDigest, err)
	}
}

// TestRecordingOneBuildTwiceReplacesTheRow is the unique index doing its work:
// two members who built the same commit to the same image are one build, and
// the row names the one most recently known to hold it.
func TestRecordingOneBuildTwiceReplacesTheRow(t *testing.T) {
	st := openBuildStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	if err := st.RecordBuild(ctx, build("/org/ffmpeg", sha(1), imageID(1), "loki", now), store.BuildLimits{}); err != nil {
		t.Fatalf("RecordBuild: %v", err)
	}
	// Another member recording the same build changes nothing. The row names
	// the member kitbashd asks for a copy of this image, so a member who could
	// take it over could point every other member's fetch at themselves.
	if err := st.RecordBuild(ctx,
		build("/org/ffmpeg", sha(1), imageID(1), "kim", now.Add(time.Hour)), store.BuildLimits{}); err != nil {
		t.Fatalf("RecordBuild again: %v", err)
	}

	builds, err := st.Builds(ctx, store.BuildFilter{Path: "/org/ffmpeg"})
	if err != nil {
		t.Fatalf("Builds: %v", err)
	}
	if len(builds) != 1 {
		t.Fatalf("the path has %d builds, want one row for one path, commit and digest", len(builds))
	}
	if builds[0].Builder != "loki" || !builds[0].BuiltAt.Equal(now) {
		t.Errorf("the build is %+v, want the member who recorded it first", builds[0])
	}

	// The member who owns the row re-recording it refreshes their own time and
	// size, which is what a second build of one commit does.
	refreshed := build("/org/ffmpeg", sha(1), imageID(1), "loki", now.Add(2*time.Hour))
	refreshed.Size = 8192
	if err := st.RecordBuild(ctx, refreshed, store.BuildLimits{}); err != nil {
		t.Fatalf("RecordBuild by the owner: %v", err)
	}
	builds, err = st.Builds(ctx, store.BuildFilter{Path: "/org/ffmpeg"})
	if err != nil {
		t.Fatalf("Builds: %v", err)
	}
	if len(builds) != 1 || builds[0].Builder != "loki" ||
		!builds[0].BuiltAt.Equal(now.Add(2*time.Hour)) || builds[0].Size != 8192 {
		t.Errorf("the build is %+v, want the owner's own record refreshed", builds)
	}
}

// TestOneMembersRecordsCannotPruneAnothers is the other half of the same rule:
// a member who records enough rows of one path prunes only their own, so
// nobody can push another member's build out of the table and have a fetch
// find nothing where it used to be.
func TestOneMembersRecordsCannotPruneAnothers(t *testing.T) {
	st := openBuildStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	if err := st.RecordBuild(ctx, build("/org/ffmpeg", sha(1), imageID(1), "kim", now),
		store.BuildLimits{PerPath: 4, PerBuilder: 2}); err != nil {
		t.Fatalf("RecordBuild: %v", err)
	}
	for i := range 6 {
		at := now.Add(time.Duration(i+1) * time.Second)
		digest := fmt.Sprintf("sha256:%064x", i)
		if err := st.RecordBuild(ctx,
			store.Build{Path: "/org/ffmpeg", Commit: sha(2), Digest: digest, Builder: "loki", BuiltAt: at},
			store.BuildLimits{PerPath: 4, PerBuilder: 2}); err != nil {
			t.Fatalf("RecordBuild %d: %v", i, err)
		}
	}

	mine, err := st.Builds(ctx, store.BuildFilter{Path: "/org/ffmpeg", Builder: "loki"})
	if err != nil {
		t.Fatalf("Builds: %v", err)
	}
	if len(mine) != 2 {
		t.Errorf("the busy member holds %d builds, want the per builder cap of 2", len(mine))
	}
	theirs, err := st.Builds(ctx, store.BuildFilter{Path: "/org/ffmpeg", Builder: "kim"})
	if err != nil {
		t.Fatalf("Builds: %v", err)
	}
	if len(theirs) != 1 || theirs[0].Digest != imageID(1) {
		t.Errorf("the other member holds %+v, want their one build untouched", theirs)
	}
}

// TestBuildsArePrunedBeyondTheCap keeps the table a directory of what exists
// rather than an archive of everything that ever did. The cap is the caller's
// argument, the same shape RegisterProcess takes, so this exercises the prune
// without writing four thousand rows.
func TestBuildsArePrunedBeyondTheCap(t *testing.T) {
	st := openBuildStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	// Recording the cap plus two leaves exactly the cap, and the two oldest
	// are the ones that go.
	const keep = 5
	total := keep + 2
	for i := range total {
		at := now.Add(time.Duration(i) * time.Second)
		digest := fmt.Sprintf("sha256:%064x", i)
		if err := st.RecordBuild(ctx,
			store.Build{Path: "/org/ffmpeg", Commit: sha(1), Digest: digest, Builder: "loki", BuiltAt: at},
			store.BuildLimits{PerPath: keep}); err != nil {
			t.Fatalf("RecordBuild %d: %v", i, err)
		}
	}
	held, err := st.BuildCount(ctx, "/org/ffmpeg")
	if err != nil {
		t.Fatalf("BuildCount: %v", err)
	}
	if held != keep {
		t.Errorf("the path holds %d builds, want the cap of %d", held, keep)
	}
	// The oldest two are gone and the newest is there.
	gone, err := st.Builds(ctx, store.BuildFilter{Path: "/org/ffmpeg", Digest: fmt.Sprintf("sha256:%064x", 0)})
	if err != nil || len(gone) != 0 {
		t.Errorf("the oldest build is %+v, %v, want it pruned", gone, err)
	}
	newest, err := st.Builds(ctx, store.BuildFilter{Path: "/org/ffmpeg", Limit: 1})
	if err != nil || len(newest) != 1 || newest[0].Digest != fmt.Sprintf("sha256:%064x", total-1) {
		t.Errorf("the newest build is %+v, %v, want the last one recorded", newest, err)
	}
}

// TestABuildNeedsEveryField refuses a record nothing can be fetched from.
func TestABuildNeedsEveryField(t *testing.T) {
	st := openBuildStore(t)
	ctx := context.Background()
	for _, b := range []store.Build{
		{Commit: sha(1), Digest: imageID(1), Builder: "loki"},
		{Path: "/org/ffmpeg", Digest: imageID(1), Builder: "loki"},
		{Path: "/org/ffmpeg", Commit: sha(1), Builder: "loki"},
		{Path: "/org/ffmpeg", Commit: sha(1), Digest: imageID(1)},
	} {
		if err := st.RecordBuild(ctx, b, store.BuildLimits{}); err == nil {
			t.Errorf("RecordBuild(%+v) was accepted, want a refusal", b)
		}
	}
}

// TestBuildsGoWithTheirBuilder is part of removing a member: a row naming an
// account that is gone points at an image store that went with it.
func TestBuildsGoWithTheirBuilder(t *testing.T) {
	st := openBuildStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	if err := st.RecordBuild(ctx, build("/org/ffmpeg", sha(1), imageID(1), "loki", now), store.BuildLimits{}); err != nil {
		t.Fatalf("RecordBuild: %v", err)
	}
	if err := st.RecordBuild(ctx, build("/org/ffmpeg", sha(2), imageID(2), "kim", now), store.BuildLimits{}); err != nil {
		t.Fatalf("RecordBuild: %v", err)
	}
	n, err := st.DeleteBuildsByBuilder(ctx, "loki")
	if err != nil || n != 1 {
		t.Fatalf("DeleteBuildsByBuilder = %d, %v, want the one build of that member", n, err)
	}
	left, err := st.Builds(ctx, store.BuildFilter{Path: "/org/ffmpeg"})
	if err != nil || len(left) != 1 || left[0].Builder != "kim" {
		t.Errorf("the builds left are %+v, %v, want the other member's", left, err)
	}
}
