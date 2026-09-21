package proc_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/proc"
)

// twoUnitManifest is the Package M12 is about: a face and a unit behind it,
// which run as one pod, see PLAN.md section 5.6.
// The sidecar is declared first and the face second, which is the order that
// tells a reader of these tests apart from a reader that happens to be right:
// what answers for the Process is the unit that declares the face, not the
// first container of the pod.
const twoUnitManifest = `name: board
description: A counter and the cache it keeps its count in, two units of one Package.
deploy:
  units:
    - name: cache
      type: container
      image: docker.io/library/redis@sha256:` + redisDigest + `
      command: [redis-server, --save, "60 1"]
    - name: web
      type: container
      build: .
      expose: http
      port: 8080
      limits: { memory: 256Mi }
`

const redisDigest = "1111111111111111111111111111111111111111111111111111111111111111"

// buildUnit puts one image of one unit of a Package in the runtime, the way
// pkg_build leaves it: labelled with the Package, the commit and the unit it
// was built for, which is what tells two units of one Package apart.
func (f *fixture) buildUnit(folder, name, unit string) string {
	id, _, _ := f.runner.Build(context.Background(), folder, folder+"/Containerfile",
		"localhost/kitbash/"+name+"-"+unit+":abcdef123456", map[string]string{
			podman.LabelPath:   folder,
			podman.LabelName:   name,
			podman.LabelCommit: strings.Repeat("a", 40),
			podman.LabelUser:   "tester",
			podman.LabelUnit:   unit,
		})
	return id
}

// A Package of two units is one Process that is one pod: one registration, one
// address, one line, and one container per unit under a name that says which
// unit it is.
func TestRunningSeveralUnitsStartsOnePod(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "board", twoUnitManifest)
	web := f.buildUnit(folder, "board", "web")
	cache := f.buildUnit(folder, "board", "cache")

	process, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if process.Container != "kitbash-board-board-web" {
		t.Errorf("the Process is the container %s, want the face unit's", process.Container)
	}
	if process.Digest != web {
		t.Errorf("the Process runs %s, want the face unit's image", process.Digest)
	}
	if process.State != proc.StateRunning {
		t.Errorf("state is %s, want running", process.State)
	}

	regs := f.daemon.Registrations()
	if len(regs) != 1 {
		t.Fatalf("kitbashd holds %d registrations, want one Process for one Package", len(regs))
	}
	reg := regs[0]
	if reg.Pod != "kitbash-board-board" {
		t.Errorf("the registration names the pod %q, want kitbash-board-board", reg.Pod)
	}
	if len(reg.Units) != 2 {
		t.Fatalf("the registration carries %d units, want two", len(reg.Units))
	}
	faces := 0
	for _, unit := range reg.Units {
		if unit.Container != "kitbash-board-board-"+unit.Name {
			t.Errorf("the unit %s is the container %s", unit.Name, unit.Container)
		}
		if unit.Face {
			faces++
			if unit.Name != "web" || unit.Digest != web {
				t.Errorf("the face is %+v, want the web unit at its own image", unit)
			}
		}
		if unit.Name == "cache" && unit.Digest != cache {
			t.Errorf("the cache runs %s, want its own image", unit.Digest)
		}
	}
	if faces != 1 {
		t.Errorf("%d units declare the face, want exactly one", faces)
	}
	// The Process's own mounts and secrets are empty: a composed Package
	// declares them per unit, and one declaration in two places is two.
	if len(reg.Mounts) != 0 || len(reg.Secrets) != 0 {
		t.Errorf("the registration carries the Process level mounts %+v and secrets %+v",
			reg.Mounts, reg.Secrets)
	}

	// One container per unit in the member's runtime, each carrying the unit
	// it is and the pod it belongs to.
	containers, err := f.runner.Containers(context.Background(), podman.Filter{podman.LabelID: process.ID}, true)
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	if len(containers) != 2 {
		t.Fatalf("the runtime holds %d containers of this Process, want one per unit", len(containers))
	}
	for _, container := range containers {
		if container.Labels[podman.LabelPod] != "kitbash-board-board" {
			t.Errorf("%s carries the pod label %q", container.Name, container.Labels[podman.LabelPod])
		}
		if container.Labels[podman.LabelUnit] == "" {
			t.Errorf("%s carries no unit label", container.Name)
		}
	}
}

// proc_list shows one line per Process and the units beside it, and the
// Process is running when every unit is.
func TestListShowsTheUnitsOfAPod(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "board", twoUnitManifest)
	f.buildUnit(folder, "board", "web")
	f.buildUnit(folder, "board", "cache")
	process, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}

	list, prob := f.processes.List(context.Background())
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	if len(list.Processes) != 1 {
		t.Fatalf("proc_list answers %d Processes, want one line for one pod", len(list.Processes))
	}
	listed := list.Processes[0]
	if listed.ID != process.ID {
		t.Errorf("the line is for %s, want the Process", listed.ID)
	}
	if len(listed.Units) != 2 {
		t.Fatalf("the line carries %d units, want two: %+v", len(listed.Units), listed.Units)
	}
	if listed.Units[0].Name != "cache" || listed.Units[1].Name != "web" {
		t.Errorf("the units are %+v, want them named", listed.Units)
	}
	if listed.State != proc.StateRunning {
		t.Errorf("the Process is %s while every unit is running", listed.State)
	}

	// Stopping one unit's container by hand is what a member does to see
	// which unit is down, which is the acceptance sentence of issue 162.
	if err := f.runner.Stop(context.Background(), "kitbash-board-board-cache", 10); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	list, prob = f.processes.List(context.Background())
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	listed = list.Processes[0]
	if listed.State == proc.StateRunning {
		t.Error("the Process is running while one of its units is not")
	}
	down := ""
	for _, unit := range listed.Units {
		if unit.State != proc.StateRunning {
			down = unit.Name
		}
	}
	if down != "cache" {
		t.Errorf("the listing says %q is down, want the cache", down)
	}
}

// A Package of two units where none declares a face has no address and no
// Process to be. The manifest layer refuses it, which makes the folder not a
// Package at all, so proc_run answers before it ever reaches the pod path and
// nothing is registered.
func TestRunningSeveralUnitsWithNoFaceIsRefused(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "board", strings.Replace(twoUnitManifest, "      expose: http\n", "", 1))

	_, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob == nil {
		t.Fatal("a Package of two units with no face was started")
	}
	if n := len(f.daemon.Registrations()); n != 0 {
		t.Errorf("kitbashd holds %d registrations for a Package that was refused", n)
	}
	containers, err := f.runner.Containers(context.Background(), podman.Filter{}, true)
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	if len(containers) != 0 {
		t.Errorf("the runtime holds %d containers of a Package that was refused", len(containers))
	}
}

// A unit that has not been built is named: a Package of several units has one
// image per unit, and "this Package has not been built" would not say which.
func TestRunningSeveralUnitsNamesTheUnitThatWasNotBuilt(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "board", twoUnitManifest)
	f.buildUnit(folder, "board", "web")

	_, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob == nil {
		t.Fatal("a Package whose sidecar has no image was started")
	}
	if !strings.Contains(prob.Detail, "cache") {
		t.Errorf("the refusal is %q, want it to name the unit with no image", prob.Detail)
	}
	if n := len(f.daemon.Registrations()); n != 0 {
		t.Errorf("kitbashd holds %d registrations for a Package that was refused", n)
	}
}

// A Package of one unit is registered exactly as it was before pods existed:
// no pod, no units, and the container named after the Package.
func TestASingleUnitPackageRegistersNoPod(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")

	process, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if process.Container != "kitbash-ffmpeg-ffmpeg" {
		t.Errorf("the container is %s, want the name a Package of one unit has always had", process.Container)
	}
	if len(process.Units) != 0 {
		t.Errorf("a Process of one unit lists the units %+v", process.Units)
	}
	regs := f.daemon.Registrations()
	if len(regs) != 1 {
		t.Fatalf("kitbashd holds %d registrations, want one", len(regs))
	}
	if regs[0].Pod != "" || len(regs[0].Units) != 0 {
		t.Errorf("a Package of one unit registered the pod %q with %d units",
			regs[0].Pod, len(regs[0].Units))
	}
}

// proc_logs of a Process that runs as a pod reads the face by default: it is
// the unit that answers for the Process, and #163 is what gives proc_logs the
// unit to ask for instead.
func TestLogsOfAPodReadTheFace(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "board", twoUnitManifest)
	f.buildUnit(folder, "board", "web")
	f.buildUnit(folder, "board", "cache")
	process, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	f.runner.LogLines["kitbash-board-board-web"] = []string{"the face said this"}
	f.runner.LogLines["kitbash-board-board-cache"] = []string{"the sidecar said this"}

	out, prob := f.processes.Logs(context.Background(), process.ID, "", 10)
	if prob != nil {
		t.Fatalf("Logs: %s", prob.Detail)
	}
	if len(out.Lines) != 1 || out.Lines[0] != "the face said this" {
		t.Errorf("proc_logs answered %v, want the face unit's output", out.Lines)
	}
}

// proc_logs of a named unit reads that unit's container, which is the whole of
// what #163 adds: a member reading a pod does not have to know the name the
// runtime gave each container.
func TestLogsOfAPodReadTheUnitThatWasNamed(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "board", twoUnitManifest)
	f.buildUnit(folder, "board", "web")
	f.buildUnit(folder, "board", "cache")
	process, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	f.runner.LogLines["kitbash-board-board-web"] = []string{"the face said this"}
	f.runner.LogLines["kitbash-board-board-cache"] = []string{"the sidecar said this"}

	for unit, want := range map[string]string{"web": "the face said this", "cache": "the sidecar said this"} {
		out, prob := f.processes.Logs(context.Background(), process.ID, unit, 10)
		if prob != nil {
			t.Fatalf("Logs of %s: %s", unit, prob.Detail)
		}
		if len(out.Lines) != 1 || out.Lines[0] != want {
			t.Errorf("proc_logs of the unit %s answered %v, want %q", unit, out.Lines, want)
		}
	}
}

// A unit this Process does not have is not-found naming the ones it does, so
// the next call is the right one rather than a second guess.
func TestLogsOfAUnitThisProcessDoesNotHaveIsNotFound(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "board", twoUnitManifest)
	f.buildUnit(folder, "board", "web")
	f.buildUnit(folder, "board", "cache")
	process, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}

	_, prob = f.processes.Logs(context.Background(), process.ID, "redis", 10)
	if prob == nil {
		t.Fatal("proc_logs read a unit this Process does not have")
	}
	if prob.Status != http.StatusNotFound {
		t.Errorf("the problem is %d, want not-found", prob.Status)
	}
	for _, unit := range []string{"cache", "web"} {
		if !strings.Contains(prob.Fix, unit) {
			t.Errorf("the fix is %q, want it to name the unit %s", prob.Fix, unit)
		}
	}
}

// A Package of one unit has no unit to name: it is the Package itself, and a
// name given for it is not-found rather than the logs of the only container
// there is, which would teach a member a name that does not exist.
func TestLogsOfAUnitOfASingleUnitProcessIsNotFound(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")
	process, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	f.runner.LogLines["kitbash-ffmpeg-ffmpeg"] = []string{"the only unit said this"}

	_, prob = f.processes.Logs(context.Background(), process.ID, "ffmpeg", 10)
	if prob == nil {
		t.Fatal("proc_logs answered for a unit of a Process that has none")
	}
	if prob.Status != http.StatusNotFound {
		t.Errorf("the problem is %d, want not-found", prob.Status)
	}
	// Without a unit the same Process reads as it always has.
	out, prob := f.processes.Logs(context.Background(), process.ID, "", 10)
	if prob != nil {
		t.Fatalf("Logs: %s", prob.Detail)
	}
	if len(out.Lines) != 1 || out.Lines[0] != "the only unit said this" {
		t.Errorf("proc_logs answered %v, want the container's output", out.Lines)
	}
}
