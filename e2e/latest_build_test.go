package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The regression of issue #113. proc_run without a digest runs "the latest
// build", and podman reports image creation times at one second resolution, so
// two builds of one Package that land in the same second used to be ordered by
// nothing and the call picked an arbitrary one of them. It was found here: a
// Process converged onto the first of two builds while the surface published
// the tools of the second.
//
// This is the only place the ordering is proved against a real podman. The
// unit tests in internal/proc build the tie out of a fake image list; what
// they cannot show is that two real builds do land in the same second.

// The Package built twice. Its files are the fixture's, with marker.txt
// rewritten between the two builds: one changed byte is a new commit, a new
// layer and a new digest, and nothing else about the image moves.
const (
	rebuildName   = "rebuild"
	rebuildMarker = "marker.txt"
)

// rebuildFixtures are the two files that do not change between the builds,
// written in this order because the manifest is what makes the folder visible.
var rebuildFixtures = []string{"kitbash.yaml", "Dockerfile"}

func TestLatestBuild(t *testing.T) {
	requireHost(t)
	admin := dial(t, adminName())
	path := filepath.Join("/home", adminName(), rebuildName)
	for _, name := range rebuildFixtures {
		body, err := os.ReadFile(filepath.Join("fixtures", rebuildName, name))
		if err != nil {
			t.Fatalf("reading the fixture %s: %v", name, err)
		}
		writeFile(t, admin, filepath.Join(path, name), string(body))
	}

	marker := filepath.Join(path, rebuildMarker)
	writeFile(t, admin, marker, "1\n")
	started := time.Now()
	first := buildOnce(t, admin, path)
	// One byte, which is a second commit of the same Package, and then the
	// second build immediately: the two have to be close enough together to
	// land in the same second for this to be the regression it is named after.
	writeFile(t, admin, marker, "2\n")
	second := buildOnce(t, admin, path)
	t.Logf("two builds of %s took %s", path, time.Since(started).Round(time.Millisecond))
	if first == second {
		t.Fatalf("both builds answered %s, want two digests: the marker changed between them", first)
	}
	sameSecond(t, first, second)

	// The call under test. No digest, so the answer is whatever the resolution
	// picked, and it has to be the build that happened last.
	var out struct {
		ID     string `json:"id"`
		State  string `json:"state"`
		Digest string `json:"digest"`
	}
	admin.ok("proc_run", map[string]any{"package": path}, &out)
	t.Cleanup(func() { admin.ok("proc_stop", map[string]any{"id": out.ID}, nil) })
	if out.State != "running" {
		t.Fatalf("proc_run answered %+v, want a running Process", out)
	}
	if out.Digest != second {
		t.Fatalf("proc_run without a digest started %s, want the second build %s (the first was %s)",
			out.Digest, second, first)
	}

	// The registration is what kitbashd restores the Process from after a
	// reboot, so the image it holds has to be the one that was picked too.
	var listed struct {
		Processes []struct {
			ID     string `json:"id"`
			Digest string `json:"digest"`
		} `json:"processes"`
	}
	// Asked about this Package, so the answer is its Processes in full: a
	// listing of everything is one line each and carries no digest.
	admin.ok("proc_list", map[string]any{"package": path}, &listed)
	found := false
	for _, p := range listed.Processes {
		if p.ID != out.ID {
			continue
		}
		found = true
		if p.Digest != second {
			t.Fatalf("proc_list reports %s runs %s, want the second build %s", p.ID, p.Digest, second)
		}
	}
	if !found {
		t.Fatalf("proc_list does not hold the Process %s that proc_run answered with", out.ID)
	}
}

// buildOnce builds the Package and returns the digest pkg_build answered with.
func buildOnce(t *testing.T, session *session, path string) string {
	t.Helper()
	var out struct {
		Digest string `json:"digest"`
	}
	res := session.callWithin(buildTimeout, "pkg_build", map[string]any{"path": path})
	res.mustSucceed(t, "pkg_build")
	if err := json.Unmarshal(res.Structured, &out); err != nil {
		t.Fatalf("decoding pkg_build: %v", err)
	}
	if !strings.HasPrefix(out.Digest, "sha256:") || len(out.Digest) != 71 {
		t.Fatalf("pkg_build answered the digest %q, want sha256 and 64 hexadecimal digits", out.Digest)
	}
	return out.Digest
}

// sameSecond says whether the two images landed in the same second, which is
// the case the ordering exists for. It is reported rather than required: a
// runner slow enough to put a second between two builds still has to run the
// later one, and failing the job there would be failing it for being slow.
func sameSecond(t *testing.T, first, second string) {
	t.Helper()
	out, err := runAs(t, adminName(), "podman", "image", "inspect", "--format", "{{.Created}}", first, second)
	if err != nil {
		t.Logf("reading the creation times of the two builds: %v: %s", err, truncate(out))
		return
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Logf("podman reported the creation times as %q", truncate(out))
		return
	}
	if truncatedToSecond(lines[0]) == truncatedToSecond(lines[1]) {
		t.Logf("both builds landed in the same second: %s and %s", lines[0], lines[1])
		return
	}
	t.Logf("the two builds landed in different seconds: %s and %s", lines[0], lines[1])
}

// truncatedToSecond drops the fraction of a printed creation time, which is
// all podman puts in an image listing. It prints them as
// "2026-09-13 10:15:23.279892228 +0000 UTC", so the date and the time of day
// are the first two fields.
func truncatedToSecond(printed string) string {
	fields := strings.Fields(printed)
	if len(fields) < 2 {
		return printed
	}
	return fields[0] + " " + strings.Split(fields[1], ".")[0]
}
