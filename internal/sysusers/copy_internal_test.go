package sysusers

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/podman"
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

// TestImageInfoReadsAnInspect: the size and the labels of one image, which are
// what both ends of a copy and every build record are checked against.
func TestImageInfoReadsAnInspect(t *testing.T) {
	const out = `[{"Size":4096,"Labels":{"kitbash.path":"/org/ffmpeg","kitbash.commit":"abc"}}]`
	info, err := imageInfo(out, nil)
	if err != nil {
		t.Fatalf("imageInfo: %v", err)
	}
	if info.Size != 4096 {
		t.Errorf("the size is %d, want the one the inspect printed", info.Size)
	}
	if info.Label(podman.LabelPath) != "/org/ffmpeg" || info.Label(podman.LabelCommit) != "abc" {
		t.Errorf("the labels are %v, want the provenance the build stamped", info.Labels)
	}
	// An older runtime answers the labels under Config and nothing at the top
	// level, and an image with no labels at all is a map to read, not a nil.
	nested := `[{"Size":1,"Config":{"Labels":{"kitbash.path":"/org/ffmpeg"}}}]`
	if info, err := imageInfo(nested, nil); err != nil || info.Label(podman.LabelPath) != "/org/ffmpeg" {
		t.Errorf("imageInfo of Config labels = %+v, %v, want the labels read", info, err)
	}
	if info, err := imageInfo(`[{"Size":1}]`, nil); err != nil || info.Labels == nil || len(info.Labels) != 0 {
		t.Errorf("imageInfo of an image with no labels = %+v, %v, want an empty label set", info, err)
	}

	// Exit 1 is the image the member does not have, which is what podman 4
	// answers an inspect of an unknown image with.
	if _, err := imageInfo("", exitError(t, 1)); !errors.Is(err, ErrNoImage) {
		t.Errorf("imageInfo after exit 1 = %v, want ErrNoImage", err)
	}
	// podman 5 answers the same question with 125, which is otherwise the
	// status it refuses a command line with, so the message decides. The
	// runtime's own words arrive in the error, which is where the runner puts
	// its standard error.
	notKnown := fmt.Errorf("sysusers: podman image inspect as loki: %w: Error: sha256:abc: image not known",
		exitError(t, usageExit))
	if _, err := imageInfo("", notKnown); !errors.Is(err, ErrNoImage) {
		t.Errorf("imageInfo after podman 5 said image not known = %v, want ErrNoImage", err)
	}
	if _, err := imageInfo("Error: no such image sha256:abc", exitError(t, usageExit)); !errors.Is(err, ErrNoImage) {
		t.Errorf("imageInfo after no such image = %v, want ErrNoImage", err)
	}
	// Every other 125 is podman refusing the command itself. Reporting it as a
	// missing image would send a member off to build something that is
	// already there.
	refused := fmt.Errorf("sysusers: podman image inspect as loki: %w: Error: unknown flag: --format",
		exitError(t, usageExit))
	if _, err := imageInfo("", refused); errors.Is(err, ErrNoImage) {
		t.Errorf("imageInfo after exit %d = %v, want the failure as it is", usageExit, err)
	}
	// Anything else is the host's failure and is reported as it is.
	other := errors.New("the runtime could not be started")
	if _, err := imageInfo("", other); !errors.Is(err, other) {
		t.Errorf("imageInfo after a host failure = %v, want it reported as it is", err)
	}
	// Output this process cannot read is not a missing image either.
	if _, err := imageInfo("<no value>", nil); err == nil || errors.Is(err, ErrNoImage) {
		t.Errorf("imageInfo of output that is not JSON = %v, want a failure that is not ErrNoImage", err)
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
