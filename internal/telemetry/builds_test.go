package telemetry_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/telemetry"
	"github.com/zyx1121/kitbash/internal/telemetry/teltest"
)

// The build registry of kitbashd as a session speaks to it, see PLAN.md
// section 2.2.

const (
	buildDigest = "sha256:abababababababababababababababababababababababababababababababab"
	buildCommit = "0123456789abcdef0123456789abcdef01234567"
)

func TestRecordBuildSendsThePathTheCommitAndTheDigest(t *testing.T) {
	daemon := newDaemon(t)
	client := telemetry.NewClient(daemon.Socket)

	prob := client.RecordBuild(context.Background(), telemetry.Build{
		Path: "/org/ffmpeg", Commit: buildCommit, Digest: buildDigest, Size: 2048,
	})
	if prob != nil {
		t.Fatalf("RecordBuild: %s", prob.Detail)
	}
	calls := daemon.Calls()
	if len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Path != telemetry.BuildsPath {
		t.Fatalf("calls are %+v, want one POST to %s", calls, telemetry.BuildsPath)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(calls[0].Body), &sent); err != nil {
		t.Fatalf("the request body is not JSON: %v", err)
	}
	if sent["path"] != "/org/ffmpeg" || sent["commit"] != buildCommit || sent["digest"] != buildDigest {
		t.Errorf("the body is %v, want the Package, its commit and its image", sent)
	}
	// The builder is the peer, so a session never sends one.
	if _, claimed := sent["builder"]; claimed {
		t.Errorf("the body carries a builder, and kitbashd stamps the peer: %v", sent)
	}
	held := daemon.Builds()
	if len(held) != 1 || held[0].Digest != buildDigest {
		t.Errorf("the daemon holds %+v, want the build just recorded", held)
	}
}

func TestBuildsReadsWhatOtherMembersBuilt(t *testing.T) {
	daemon := newDaemon(t)
	client := telemetry.NewClient(daemon.Socket)
	daemon.AddBuild(teltest.Build{
		Path: "/org/ffmpeg", Commit: buildCommit, Digest: buildDigest, Builder: "kim",
	})
	daemon.AddBuild(teltest.Build{
		Path: "/org/other", Commit: buildCommit, Digest: buildDigest, Builder: "kim",
	})

	builds, prob := client.Builds(context.Background(), "/org/ffmpeg", buildCommit, "")
	if prob != nil {
		t.Fatalf("Builds: %s", prob.Detail)
	}
	if len(builds) != 1 || builds[0].Builder != "kim" || builds[0].Digest != buildDigest {
		t.Fatalf("the builds are %+v, want the one of that Package and commit", builds)
	}
	calls := daemon.Calls()
	if len(calls) != 1 || !strings.Contains(calls[0].Path, "commit="+buildCommit) {
		t.Errorf("the call is %+v, want the commit in the query", calls)
	}
}

func TestFetchImageAsksForTheCopyAndReadsTheAnswer(t *testing.T) {
	daemon := newDaemon(t)
	client := telemetry.NewClient(daemon.Socket)
	daemon.AddBuild(teltest.Build{
		Path: "/org/ffmpeg", Commit: buildCommit, Digest: buildDigest, Builder: "kim", Size: 4096,
	})

	result, prob := client.FetchImage(context.Background(), buildDigest, "/org/ffmpeg", "kim")
	if prob != nil {
		t.Fatalf("FetchImage: %s", prob.Detail)
	}
	if result.Digest != buildDigest || result.From != "kim" || result.Bytes != 4096 {
		t.Errorf("the answer is %+v, want the image, its size and who it came from", result)
	}
	fetches := daemon.Fetches()
	if len(fetches) != 1 || fetches[0].Path != "/org/ffmpeg" || fetches[0].From != "kim" {
		t.Errorf("the fetches are %+v, want the one that was asked for", fetches)
	}
}

func TestFetchImageReturnsTheDaemonsProblem(t *testing.T) {
	daemon := newDaemon(t)
	client := telemetry.NewClient(daemon.Socket)
	daemon.AnswerFetch(teltest.Problem(http.StatusNotFound, "not-found", "Not found",
		"kim no longer has the image", "Build this Package yourself with pkg_build."))

	_, prob := client.FetchImage(context.Background(), buildDigest, "/org/ffmpeg", "kim")
	if prob == nil {
		t.Fatal("a refused fetch was reported as a copy that worked")
	}
	// kitbashd answers in the same problem details the surface speaks, so the
	// refusal reaches the agent unchanged, slug and fix and all.
	if prob.Slug() != "not-found" || prob.Fix == "" {
		t.Errorf("the problem is %+v, want the daemon's own not-found", prob)
	}
}
