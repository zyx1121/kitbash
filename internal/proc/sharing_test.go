package proc_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/telemetry/teltest"
)

// A digest a member does not have may be one another member built, which
// kitbashd copies over rather than leaving them to build the same commit
// again, see PLAN.md section 2.2 and issue 68.

// sharedDigest is the image the other member built.
const sharedDigest = "sha256:abababababababababababababababababababababababababababababababab"

// TestRunFetchesADigestThisMemberDoesNotHave is the acceptance sentence of
// issue 68 for proc_run: naming a digest another member built runs it here.
func TestRunFetchesADigestThisMemberDoesNotHave(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	// This member has one build of their own, and the digest asked for is
	// another member's.
	f.build(folder, "ffmpeg")
	f.daemon.AddBuild(teltest.Build{
		Path: folder, Commit: strings.Repeat("c", 40), Digest: sharedDigest, Builder: "kim",
	})

	process, prob := f.processes.Run(ctx, folder, sharedDigest, "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if process.Digest != sharedDigest {
		t.Errorf("the Process runs %s, want the digest that was asked for", process.Digest)
	}
	fetches := f.daemon.Fetches()
	if len(fetches) != 1 {
		t.Fatalf("kitbashd was asked for %d copies, want the one image", len(fetches))
	}
	if fetches[0].Digest != sharedDigest || fetches[0].Path != folder || fetches[0].From != "kim" {
		t.Errorf("the fetch is %+v, want that image of that Package from that member", fetches[0])
	}
	// The container runs the copied image, which is now in this member's own
	// store like any other.
	if len(f.runner.Runs) != 1 || f.runner.Runs[0].Image != sharedDigest {
		t.Errorf("the runtime ran %+v, want the copied image", f.runner.Runs)
	}
	if f.runner.Runs[0].Labels[podman.LabelDigest] != sharedDigest {
		t.Errorf("the container is labelled %q, want the digest it runs",
			f.runner.Runs[0].Labels[podman.LabelDigest])
	}
}

// TestRunOfADigestNobodyBuiltIsStillNotFound: a copy is offered for a build
// kitbashd has a record of, and for nothing else.
func TestRunOfADigestNobodyBuiltIsStillNotFound(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")

	absent := "sha256:" + strings.Repeat("b", 64)
	_, prob := f.processes.Run(ctx, folder, absent, "")
	if prob == nil {
		t.Fatal("a Process was run from a digest nobody has")
	}
	if prob.Status != http.StatusNotFound {
		t.Errorf("the problem is %d %s, want 404 not-found", prob.Status, prob.Slug())
	}
	if len(f.daemon.Fetches()) != 0 {
		t.Errorf("kitbashd was asked to copy %+v, want nothing", f.daemon.Fetches())
	}
}

// TestRunReportsNotFoundWhenTheCopyFails: the member is told the same thing
// they would have been told without a daemon, which is to build it.
func TestRunReportsNotFoundWhenTheCopyFails(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")
	f.daemon.AddBuild(teltest.Build{
		Path: folder, Commit: strings.Repeat("c", 40), Digest: sharedDigest, Builder: "kim",
	})
	f.daemon.AnswerFetch(teltest.Problem(http.StatusInternalServerError, "internal", "Internal error",
		"the image could not be copied into your store", "Call pkg_build for this Package instead."))

	_, prob := f.processes.Run(ctx, folder, sharedDigest, "")
	if prob == nil {
		t.Fatal("a Process was run from an image that was never copied")
	}
	if prob.Status != http.StatusNotFound {
		t.Errorf("the problem is %d %s, want 404 not-found", prob.Status, prob.Slug())
	}
	if len(f.runner.Runs) != 0 {
		t.Errorf("the runtime ran %+v, want nothing", f.runner.Runs)
	}
}
