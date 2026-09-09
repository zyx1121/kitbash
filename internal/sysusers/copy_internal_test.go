package sysusers

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// The copy that shares an image between two members is two podman children
// joined by a pipe, see PLAN.md section 2.2. What is tested here is the pipe
// and the reading of an inspect: dropping to another member needs root, and
// the drop itself is the same one every other command of this file makes.

// TestPipelineJoinsTheOutputOfOneChildToTheInputOfTheNext is the copy itself:
// what the save writes is what the load reads, without the archive touching
// the disk or this process.
func TestPipelineJoinsTheOutputOfOneChildToTheInputOfTheNext(t *testing.T) {
	save := exec.Command("sh", "-c", "echo 'archive of sha256:abc'")
	load := exec.Command("cat")
	var loaded, loadErr bytes.Buffer
	load.Stdout = &loaded
	load.Stderr = &loadErr

	saveErr, loadWait := pipeline(save, load)
	if saveErr != nil || loadWait != nil {
		t.Fatalf("pipeline = %v, %v, want both children to finish", saveErr, loadWait)
	}
	if strings.TrimSpace(loaded.String()) != "archive of sha256:abc" {
		t.Errorf("the second child read %q, want what the first wrote", loaded.String())
	}
}

// TestPipelineReportsEachChildSeparately is what lets a copy blame the save
// rather than the load it starved: a load that failed because the archive
// stopped mid stream is the consequence, not the cause.
func TestPipelineReportsEachChildSeparately(t *testing.T) {
	save := exec.Command("sh", "-c", "echo 'no such image' >&2; exit 125")
	load := exec.Command("cat")
	load.Stdout = &bytes.Buffer{}

	saveErr, loadErr := pipeline(save, load)
	if saveErr == nil {
		t.Error("a save that exited 125 was reported as one that worked")
	}
	if !exitCode(saveErr, 125) {
		t.Errorf("the save error is %v, want the exit status the runtime gave", saveErr)
	}
	// The load read an empty archive and said nothing, which is why the save
	// is the one reported.
	if loadErr != nil {
		t.Errorf("the load error is %v, want the failure to be the save's", loadErr)
	}

	save = exec.Command("sh", "-c", "echo archive")
	load = exec.Command("sh", "-c", "cat > /dev/null; exit 1")
	saveErr, loadErr = pipeline(save, load)
	if saveErr != nil {
		t.Errorf("the save error is %v, want a save that worked", saveErr)
	}
	if loadErr == nil {
		t.Error("a load that exited 1 was reported as one that worked")
	}
}

// TestPipelineEndsTheFirstChildWhenTheSecondCannotStart, so a save is never
// left writing into a pipe nobody drains.
func TestPipelineEndsTheFirstChildWhenTheSecondCannotStart(t *testing.T) {
	save := exec.Command("sh", "-c", "sleep 30")
	load := exec.Command("/nonexistent/podman")

	saveErr, loadErr := pipeline(save, load)
	if loadErr == nil {
		t.Fatal("a child that could not be started was reported as one that ran")
	}
	if saveErr != nil {
		t.Errorf("the first child answered %v, want it killed and waited for silently", saveErr)
	}
	if save.ProcessState == nil {
		t.Error("the first child was left running when the second could not start")
	}
}

// TestImageSizeReadsAnInspect and answers ErrNoImage for a store that does not
// have it, which is what both ends of a copy are checked with.
func TestImageSizeReadsAnInspect(t *testing.T) {
	size, err := imageSize("4096\n", nil)
	if err != nil || size != 4096 {
		t.Errorf("imageSize = %d, %v, want the size the inspect printed", size, err)
	}
	// An image that is there and whose size is unreadable is still there.
	if size, err := imageSize("<nil>", nil); err != nil || size != 0 {
		t.Errorf("imageSize of an unreadable size = %d, %v, want no size and no failure", size, err)
	}
	for _, status := range []int{1, usageExit} {
		_, err := imageSize("no such image", exitError(t, status))
		if !errors.Is(err, ErrNoImage) {
			t.Errorf("imageSize after exit %d = %v, want ErrNoImage", status, err)
		}
	}
	// Anything else is the host's failure and is not turned into a missing
	// image, which would send the caller to build something that is there.
	other := errors.New("the runtime could not be started")
	if _, err := imageSize("", other); !errors.Is(err, other) {
		t.Errorf("imageSize after a host failure = %v, want it reported as it is", err)
	}
}

// TestLoadedNamesPicksOnlyTheNameTheLoadInvented. An archive of one image
// saved by digest carries no name, so podman load invents one and the copied
// image would show up twice in podman images and in pkg_list. The tag its new
// owner writes, and every other name that member put there, are theirs.
func TestLoadedNamesPicksOnlyTheNameTheLoadInvented(t *testing.T) {
	out := `["localhost/sha256:d503eb2f7a0d","localhost/kitbash/observe-count:cb3e14906035","localhost/mine:v1"]`
	names := loadedNames(out)
	if len(names) != 1 || names[0] != "localhost/sha256:d503eb2f7a0d" {
		t.Errorf("the names to drop are %v, want the one the load invented", names)
	}
	// An image with no names at all, and output that is not a list, are both
	// nothing to untag rather than a reason to fail a copy that worked.
	for _, empty := range []string{"null", "[]", "", "<no value>"} {
		if names := loadedNames(empty); len(names) != 0 {
			t.Errorf("loadedNames(%q) = %v, want nothing to untag", empty, names)
		}
	}
}

// exitError is a real command that exited with one status, because the mapping
// under test reads the exit status of an *exec.ExitError and nothing else.
func exitError(t *testing.T, status int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", status)).Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("running a command that exits %d: %v", status, err)
	}
	return err
}
