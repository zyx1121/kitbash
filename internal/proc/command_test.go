package proc_test

import (
	"context"
	"strings"
	"testing"
)

// commandManifest is a unit that runs something other than the command of its
// image, which is the whole of what deploy.units[].command is.
const commandManifest = `name: cache
description: A key value store started with the flags this Package wants rather than the image's.
deploy:
  units:
    - type: container
      build: .
      expose: none
      command: [redis-server, --save, "60 1"]
`

// The words a unit declares reach kitbashd in the start request, because
// kitbashd is what runs the container: a command that stopped here would be a
// manifest saying one thing and a Process doing another.
func TestRunSendsTheUnitsCommandToTheDaemon(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "cache", commandManifest)
	f.build(folder, "cache")

	if _, prob := f.processes.Run(ctx, folder, "", ""); prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	starts := f.daemon.Starts()
	if len(starts) != 1 {
		t.Fatalf("kitbashd was asked for %d starts, want one", len(starts))
	}
	if got := strings.Join(starts[0].Options.Command, "|"); got != "redis-server|--save|60 1" {
		t.Errorf("the start request carries the command %q, want the words the unit declared", got)
	}
}

// jobCommandManifest is the same command on a job, which kitbashd starts at
// each tick with nothing but the registration to read it from.
const jobCommandManifest = `name: weather
description: Fetch the forecast every morning with the command this Package declares.
deploy:
  units:
    - type: container
      build: .
      expose: none
      schedule: "0 8 * * *"
      command: [node, fetch.js, --once]
`

// A job's command travels with its registration, because a tick has no session
// behind it to send a start request.
func TestAJobRegistersTheUnitsCommand(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "weather", jobCommandManifest)
	f.build(folder, "weather")

	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	reg, held := f.daemon.Registration(process.ID)
	if !held {
		t.Fatal("the job was not registered")
	}
	if reg.Schedule == nil {
		t.Fatal("the registration carries no schedule")
	}
	if got := strings.Join(reg.Schedule.Command, "|"); got != "node|fetch.js|--once" {
		t.Errorf("the registration carries the command %q, want the words the unit declared", got)
	}
}

// A job whose command changed is not the job kitbashd is holding, whatever its
// digest did: the next tick would run the old words, so proc_run registers it
// again rather than answering that nothing changed.
func TestChangingOnlyTheCommandRegistersTheJobAgain(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "weather", jobCommandManifest)
	f.build(folder, "weather")

	first, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("first Run: %s", prob.Detail)
	}
	// The image is untouched and the expression is untouched: the command is
	// the one thing that differs.
	f.pack(t, "weather", strings.Replace(jobCommandManifest, "--once", "--twice", 1))
	second, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("second Run: %s", prob.Detail)
	}
	if second.ID != first.ID {
		t.Errorf("the job was given a new id %s, want the same job %s moved", second.ID, first.ID)
	}
	reg, held := f.daemon.Registration(second.ID)
	if !held || reg.Schedule == nil {
		t.Fatal("the job is no longer registered")
	}
	if got := strings.Join(reg.Schedule.Command, "|"); got != "node|fetch.js|--twice" {
		t.Errorf("the registration carries the command %q, want the words the manifest now declares", got)
	}
}
