package daemon

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/mounts"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// The words a start request carries reach podman, after the image, which is
// where the command that replaces an image's own goes. kitbashd runs the
// container, so a command that stopped at this API would never run.
func TestStartGivesTheRuntimeTheUnitsCommand(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervise(h.user, "kitbash-echo-echo")

	res, body := h.start(id, startRequest{
		Image:   testDigest,
		Command: []string{"node", "server.js", "--port", "8080"},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("start = %d %s, want 200", res.StatusCode, body)
	}
	runs := fake.Runs()
	if len(runs) != 1 {
		t.Fatalf("the runtime was asked for %d runs, want one", len(runs))
	}
	if got := strings.Join(runs[0].Options.Command, "|"); got != "node|server.js|--port|8080" {
		t.Errorf("the container was made with the command %q, want the words the request carried", got)
	}
	// And they land on the command line where podman reads them, which is
	// after the image and nowhere else.
	args := runs[0].Args
	image := -1
	for i, arg := range args {
		if arg == testDigest {
			image = i
		}
	}
	if image < 0 {
		t.Fatalf("the command line %v names no image", args)
	}
	if got := strings.Join(args[image+1:], "|"); got != "node|server.js|--port|8080" {
		t.Errorf("the words after the image are %q, want the command", got)
	}
}

// A job's command is registered with it and read back at every tick, because
// a tick has no request body behind it.
func TestATickGivesTheRuntimeTheJobsCommand(t *testing.T) {
	h, fake := supervised(t)
	req := scheduleRegistration(everyMinute)
	req.Schedule.Command = []string{"node", "fetch.js", "--once"}
	fake.Exits = map[string]int{req.Container: 0}
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, body %s", res.StatusCode, body)
	}
	// The registration is what a tick reads, so the words have to be in the
	// row and not only in the request that wrote it.
	if got := strings.Join(h.process(req.ID).Schedule.Command, "|"); got != "node|fetch.js|--once" {
		t.Errorf("the row holds the command %q, want the words the registration carried", got)
	}

	run, _ := h.tick(time.Now().Add(time.Hour))
	if len(run) != 1 {
		t.Fatalf("%d jobs were due, want 1", len(run))
	}
	runs := fake.Runs()
	if len(runs) != 1 {
		t.Fatalf("the tick made %d containers, want 1", len(runs))
	}
	if got := strings.Join(runs[0].Options.Command, "|"); got != "node|fetch.js|--once" {
		t.Errorf("the tick ran the command %q, want the job's own", got)
	}
}

// A heal makes the container again from what the old one was made with, so
// the command goes back on it: a Process that came back running the command of
// its image would be running something nobody declared.
func TestHealMakesTheContainerWithItsCommand(t *testing.T) {
	captureDaemonLog(t)
	h, fake, owner, _ := legacyHarness(t)
	config := fake.Configs["kitbash-echo-legacy"]
	config.Command = []string{"node", "server.js"}
	fake.Configs = map[string]sysusers.ContainerConfig{"kitbash-echo-legacy": config}

	counts := h.server.Restore(context.Background())
	if counts.Healed != 1 {
		t.Fatalf("counts = %+v, want one healed", counts)
	}
	runs := fake.Runs()
	if len(runs) != 1 {
		t.Fatalf("the heal made %d containers, want one", len(runs))
	}
	if runs[0].Member != owner {
		t.Errorf("the container was made as %s, want its owner", runs[0].Member)
	}
	if got := strings.Join(runs[0].Options.Command, "|"); got != "node|server.js" {
		t.Errorf("the container was made again with the command %q, want the one it had", got)
	}
}

// A container that has to be made again, rather than healed, keeps the words
// it was made with as well: restore reads the runtime and the runtime is what
// remembers, see remakeMounted.
func TestRemakeMakesTheContainerWithItsCommand(t *testing.T) {
	h, fake, home, _ := filesHost(t)
	const container = "kitbash-reader-reader"
	h.superviseWithMounts(container, []mounts.Resolved{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: mounts.ModeRO},
	})
	fake.Configs = map[string]sysusers.ContainerConfig{
		container: {State: podman.StateCreated, Image: testDigest, Command: []string{"node", "reader.js"}},
	}
	fake.AddImage(h.user, testDigest, 1, nil)

	counts := h.server.Restore(context.Background())
	if counts.Started != 1 || counts.Failed != 0 {
		t.Fatalf("the restore is %+v, want the container made again and started", counts)
	}
	runs := fake.Runs()
	if len(runs) != 1 {
		t.Fatalf("the restore made %d containers, want one", len(runs))
	}
	if got := strings.Join(runs[0].Options.Command, "|"); got != "node|reader.js" {
		t.Errorf("the container was made again with the command %q, want the one it had", got)
	}
}

// The words come back onto the image they were read off and no other: a
// registration that moved to another digest is a Process whose command belongs
// to that image, and what the old container reported may be the old image's
// own CMD.
func TestRemakeDropsTheCommandOfAnotherImage(t *testing.T) {
	h, fake, home, _ := filesHost(t)
	const container = "kitbash-reader-reader"
	h.superviseWithMounts(container, []mounts.Resolved{
		{Source: filepath.Join(home, "notes"), Target: "/files/notes", Mode: mounts.ModeRO},
	})
	fake.Configs = map[string]sysusers.ContainerConfig{
		container: {
			State:   podman.StateCreated,
			Image:   "sha256:" + strings.Repeat("b", 64),
			Command: []string{"node", "reader.js"},
		},
	}
	fake.AddImage(h.user, testDigest, 1, nil)

	if counts := h.server.Restore(context.Background()); counts.Started != 1 {
		t.Fatalf("the restore is %+v, want the container made again and started", counts)
	}
	runs := fake.Runs()
	if len(runs) != 1 {
		t.Fatalf("the restore made %d containers, want one", len(runs))
	}
	if runs[0].Options.Image != testDigest {
		t.Fatalf("the container was made from %s, want the registration's digest", runs[0].Options.Image)
	}
	if len(runs[0].Options.Command) != 0 {
		t.Errorf("the container was made with the command %v, want the image's own", runs[0].Options.Command)
	}
}

// checkCommand is the bound on what a start request may carry, the same kind
// of bound checkEnv is: a request body is not a place to build an arbitrarily
// long argument list out of.
func TestCheckCommandBounds(t *testing.T) {
	long := strings.Repeat("a", MaxCommandBytes)
	cases := map[string]struct {
		command []string
		refused bool
	}{
		"no command at all":       {nil, false},
		"one word":                {[]string{"true"}, false},
		"the most words there is": {words(MaxCommandWords), false},
		"one word too many":       {words(MaxCommandWords + 1), true},
		"the longest word":        {[]string{long}, false},
		"one byte too long":       {[]string{long + "a"}, true},
		"an empty word":           {[]string{"node", ""}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			prob := checkCommand("/kitbash/v1/processes/x/start", tc.command)
			if tc.refused && prob == nil {
				t.Fatal("the command was accepted")
			}
			if !tc.refused && prob != nil {
				t.Fatalf("the command was refused: %s", prob.Detail)
			}
		})
	}
}

// words is a command of n harmless words.
func words(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, "word")
	}
	return out
}

// The bound is read on the way in, so a body over it is refused before
// anything is created rather than handed to podman.
func TestStartRefusesACommandOverTheBound(t *testing.T) {
	h, fake := supervised(t)
	id := h.supervise(h.user, "kitbash-echo-echo")

	res, body := h.start(id, startRequest{Image: testDigest, Command: words(MaxCommandWords + 1)})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("start = %d %s, want 400", res.StatusCode, body)
	}
	if got := len(fake.Runs()); got != 0 {
		t.Errorf("the runtime made %d containers for a request that was refused, want none", got)
	}
}

// The bound is the same one on a job, which is registered rather than started,
// so a job cannot carry what a start may not.
func TestARegistrationRefusesACommandOverTheBound(t *testing.T) {
	h, _ := supervised(t)
	req := scheduleRegistration(everyMinute)
	req.Schedule.Command = words(MaxCommandWords + 1)
	_, res, body := h.register(req)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("register = %d %s, want 400", res.StatusCode, body)
	}
}
