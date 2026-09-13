package proc_test

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/telemetry/teltest"
)

// proc_run without a digest runs "the latest build", and podman images reports
// creation times at one second resolution, so two builds of one Package inside
// one second used to be ordered by nothing at all and the call picked an
// arbitrary one of them, see issue #113. These tests pin the three steps that
// decide it now: the nanosecond creation time, the build record kitbashd
// holds, and the digest.

// oneSecond is the instant the images of these tests were all created in.
var oneSecond = time.Date(2026, 9, 13, 10, 15, 23, 0, time.UTC)

// digest is a well formed image id whose first byte names it, so a failure
// message says which image was picked.
func digest(b byte) string {
	return "sha256:" + strings.Repeat(string(b), 64)
}

// seedImage puts one image of a Package in the runtime at an exact instant,
// which is what a build inside the same second as another one produces.
func seedImage(f *fixture, folder, id string, nanos int) {
	f.runner.AddImage(podman.Image{
		ID:      id,
		Created: oneSecond.Add(time.Duration(nanos)),
		Labels: map[string]string{
			podman.LabelPath:   folder,
			podman.LabelName:   "ffmpeg",
			podman.LabelCommit: strings.Repeat("a", 40),
			podman.LabelUser:   "tester",
		},
	})
}

func TestRunWithoutADigestPicksTheLaterNanosecondOfOneSecond(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	// The earlier image has the higher digest, so a run that answers with it
	// is one that fell back to the digest instead of reading the time.
	seedImage(f, folder, digest('b'), 279892228)
	seedImage(f, folder, digest('a'), 615318587)

	var lines bytes.Buffer
	f.processes.SetLogger(log.New(&lines, "", 0))

	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if process.Digest != digest('a') {
		t.Errorf("the Process runs %s, want the image of the later nanosecond %s", process.Digest, digest('a'))
	}
	// The registration is what kitbashd restores the Process from, so the
	// image it names has to be the one that was picked, not the one the
	// caller left unsaid.
	reg, ok := f.daemon.Registration(process.ID)
	if !ok {
		t.Fatalf("kitbashd holds no registration for Process %s", process.ID)
	}
	if reg.Digest != digest('a') {
		t.Errorf("the registration names %s, want %s", reg.Digest, digest('a'))
	}
	// The choice is said out loud, so an operator reading the log knows which
	// build a call without a digest ran and why.
	if !strings.Contains(lines.String(), digest('a')) || !strings.Contains(lines.String(), "newest image") {
		t.Errorf("the log says %q, want the digest it picked and why", lines.String())
	}
}

func TestRunWithoutADigestIgnoresTheOrderTheRuntimeListed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	// The same two images as above, seeded the other way round. The answer is
	// the image of the later nanosecond either way: the order a listing comes
	// back in decides nothing.
	seedImage(f, folder, digest('a'), 615318587)
	seedImage(f, folder, digest('b'), 279892228)

	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if process.Digest != digest('a') {
		t.Errorf("the Process runs %s, want the image of the later nanosecond %s", process.Digest, digest('a'))
	}
}

func TestRunWithoutADigestBreaksAnExactTieOnTheBuildRecord(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	// Two images of the same nanosecond, which is what a runtime that cannot
	// report better than a second leaves behind. The lower digest was built
	// last, so the record is the only thing that says so.
	seedImage(f, folder, digest('c'), 0)
	seedImage(f, folder, digest('a'), 0)
	f.daemon.AddBuild(teltest.Build{
		Path: folder, Commit: strings.Repeat("a", 40), Digest: digest('c'),
		BuiltAt: "2026-09-13T10:15:23Z",
	})
	f.daemon.AddBuild(teltest.Build{
		Path: folder, Commit: strings.Repeat("b", 40), Digest: digest('a'),
		BuiltAt: "2026-09-13T10:15:24Z",
	})

	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if process.Digest != digest('a') {
		t.Errorf("the Process runs %s, want the image kitbashd recorded built last %s", process.Digest, digest('a'))
	}
}

func TestRunWithoutADigestIsTheSameAnswerTwiceWithNothingToSeparateTheImages(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	// No nanoseconds between them and no build records, which leaves the
	// digest. It says nothing about time; it only makes the answer the same
	// one on every call rather than whichever the runtime listed first.
	seedImage(f, folder, digest('a'), 0)
	seedImage(f, folder, digest('c'), 0)

	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if process.Digest != digest('c') {
		t.Errorf("the Process runs %s, want the higher digest %s", process.Digest, digest('c'))
	}
	again, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run again: %s", prob.Detail)
	}
	if again.Digest != process.Digest {
		t.Errorf("the second call runs %s and the first ran %s, want one answer", again.Digest, process.Digest)
	}
}
