package proc_test

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/proc"
	"github.com/zyx1121/kitbash/internal/telemetry"
	"github.com/zyx1121/kitbash/internal/telemetry/teltest"
)

// observerManifest is the fan out subscriber of PLAN.md section 2.4: a
// Package with subscriptions, expose http and a port.
const observerManifest = `name: observer
description: Receives every stored Telemetry record and writes judgments back.
provides:
  subscriptions: [telemetry]
  kit: [observe]
deploy:
  units:
    - type: container
      build: .
      expose: http
      port: 9090
`

// forgedManifest declares the environment kitbashd owns, which a Package must
// not be able to speak for itself.
const forgedManifest = `name: forger
description: A Package that tries to name its own Process and its own token.
deploy:
  units:
    - type: container
      build: .
      expose: none
      environment:
        LOG_LEVEL: debug
        KITBASH_TELEMETRY_ENDPOINT: http://attacker.example
        KITBASH_TELEMETRY_TOKEN: forged
        KITBASH_PROCESS: someone-elses-process
        KITBASH_PACKAGE: /org/somewhere-else
        KITBASH_USER: root
        KITBASH_FANOUT_SECRET: forged
`

// A Process is registered before its container exists, and the token that
// registration mints is in the environment the container is created with.
func TestRunRegistersTheProcessBeforeItStarts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")

	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}

	registrations := f.daemon.Registrations()
	if len(registrations) != 1 {
		t.Fatalf("kitbashd holds %d registrations, want 1", len(registrations))
	}
	got := registrations[0]
	want := teltest.Registration{
		ID:      process.ID,
		Package: folder,
		Name:    "ffmpeg",
		Expose:  manifest.ExposeMCP,
	}
	if got.ID != want.ID || got.Package != want.Package || got.Name != want.Name || got.Expose != want.Expose {
		t.Errorf("registration is %+v, want %+v", got, want)
	}
	if got.Endpoint != "" {
		t.Errorf("endpoint is %q, want none for an mcp Process", got.Endpoint)
	}
	if len(got.Subscriptions) != 0 {
		t.Errorf("subscriptions are %v, want none", got.Subscriptions)
	}
	// Boot restore starts the container the registration names from the image
	// it names, so both are registered with the Process.
	if got.Container != proc.ContainerName("ffmpeg", "ffmpeg") {
		t.Errorf("container is %q, want the one the Process runs as", got.Container)
	}
	if got.Digest != process.Digest || got.Digest == "" {
		t.Errorf("digest is %q, want the image the Process runs, %q", got.Digest, process.Digest)
	}

	env := f.runner.Runs[0].Env
	if env[telemetry.EnvToken] != f.daemon.Token(process.ID) || env[telemetry.EnvToken] == "" {
		t.Errorf("the token is %q, want the one the registration minted", env[telemetry.EnvToken])
	}
	if env[telemetry.EnvEndpoint] != telemetry.ProcessEndpoint {
		t.Errorf("endpoint is %q, want %q", env[telemetry.EnvEndpoint], telemetry.ProcessEndpoint)
	}
	if env[telemetry.EnvProcess] != process.ID {
		t.Errorf("process is %q, want %q", env[telemetry.EnvProcess], process.ID)
	}
	if env[telemetry.EnvPackage] != folder {
		t.Errorf("package is %q, want %q", env[telemetry.EnvPackage], folder)
	}
	if env[telemetry.EnvUser] != "tester" {
		t.Errorf("user is %q, want tester", env[telemetry.EnvUser])
	}
	// Every Process can reach the MCP surface as its owner, so every Process
	// is told where, see PLAN.md section 2.3.
	if env[telemetry.EnvMCPEndpoint] != telemetry.ProcessEndpoint+telemetry.MCPPath {
		t.Errorf("the MCP endpoint is %q, want %q",
			env[telemetry.EnvMCPEndpoint], telemetry.ProcessEndpoint+telemetry.MCPPath)
	}
	// The fan out secret is minted with the token and reaches the container
	// the same way, so a subscriber knows a delivery came from kitbashd, see
	// fan_out.authentication in spec/kitbashd-api.yaml.
	if env[telemetry.EnvFanoutSecret] != f.daemon.FanoutSecret(process.ID) || env[telemetry.EnvFanoutSecret] == "" {
		t.Errorf("the fan out secret is %q, want the one the registration minted", env[telemetry.EnvFanoutSecret])
	}
	// The manifest's own environment is still there.
	if env["LOG_LEVEL"] != "debug" {
		t.Errorf("env is %v, want the manifest's LOG_LEVEL as well", env)
	}
}

// The endpoint a container is reached on is the caller's own choice, so it is
// known before the registration and one registration is enough.
func TestRunRegistersTheEndpointOfAnHTTPProcess(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "observer", observerManifest)
	f.build(folder, "observer")

	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	published := f.runner.Runs[0].Publish
	if len(published) != 1 || published[0].ContainerPort != 9090 || published[0].HostPort == 0 {
		t.Fatalf("published %v, want the container port 9090 on a host port kitbash chose", published)
	}
	endpoint := "http://127.0.0.1:" + strconv.Itoa(published[0].HostPort)

	got, ok := f.daemon.Registration(process.ID)
	if !ok {
		t.Fatalf("kitbashd does not know Process %s", process.ID)
	}
	if got.Endpoint != endpoint {
		t.Errorf("the registered endpoint is %q, want %q", got.Endpoint, endpoint)
	}
	if len(got.Subscriptions) != 1 || got.Subscriptions[0] != "telemetry" {
		t.Errorf("subscriptions are %v, want [telemetry] from the manifest", got.Subscriptions)
	}
	if process.Endpoint != endpoint {
		t.Errorf("the Process endpoint is %q, want %q", process.Endpoint, endpoint)
	}
	// The endpoint is on the container too, so a later session re-registers
	// the one this Process was started on.
	if label := f.runner.Runs[0].Labels[podman.LabelEndpoint]; label != endpoint {
		t.Errorf("the endpoint label is %q, want %q", label, endpoint)
	}
	// One registration, not two: a second one would mint a token the running
	// container does not have.
	if n := len(f.daemon.Registrations()); n != 1 {
		t.Errorf("kitbashd holds %d registrations, want 1", n)
	}
}

// The seven variables are the Process's identity, the two endpoints it reaches
// kitbashd on, and the secret that tells it a fan out request came from
// kitbashd. A manifest that declares them is overruled rather than trusted: a
// Package that could name its own fan out secret could take records from
// anything on the host.
func TestManifestEnvironmentCannotOverrideTheTelemetryEnvironment(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "forger", forgedManifest)
	f.build(folder, "forger")

	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	env := f.runner.Runs[0].Env
	want := map[string]string{
		telemetry.EnvEndpoint: telemetry.ProcessEndpoint,
		telemetry.EnvToken:    f.daemon.Token(process.ID),
		telemetry.EnvProcess:  process.ID,
		telemetry.EnvPackage:  folder,
		telemetry.EnvUser:     "tester",

		telemetry.EnvMCPEndpoint:  telemetry.ProcessEndpoint + telemetry.MCPPath,
		telemetry.EnvFanoutSecret: f.daemon.FanoutSecret(process.ID),
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s is %q, want %q", k, env[k], v)
		}
	}
	if env["LOG_LEVEL"] != "debug" {
		t.Errorf("env is %v, want the manifest's own entries kept", env)
	}
}

// The endpoint a Process exports to is kitbashd's to give: the daemon writes
// the environment of the container it starts, so a host that moves its
// receiver moves every Process with it and no session has an opinion.
func TestTheProcessEndpointIsTheOneKitbashdGives(t *testing.T) {
	f := newFixture(t)
	f.daemon.SetProcessEndpoint("http://127.0.0.1:4318")
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")

	if _, prob := f.processes.Run(context.Background(), folder, "", ""); prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if got := f.runner.Runs[0].Env[telemetry.EnvEndpoint]; got != "http://127.0.0.1:4318" {
		t.Errorf("endpoint is %q, want the one kitbashd gave", got)
	}
	// The two endpoints are one listener, so they move together.
	if got := f.runner.Runs[0].Env[telemetry.EnvMCPEndpoint]; got != "http://127.0.0.1:4318/mcp" {
		t.Errorf("the MCP endpoint is %q, want the receiver with the MCP path", got)
	}
}

// Running a Process that is already converged changes nothing, so it does not
// mint a second token either: the container is holding the first one.
func TestRunAgainDoesNotRegisterTwice(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")

	first, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	token := f.daemon.Token(first.ID)
	again, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run again: %s", prob.Detail)
	}
	if again.ID != first.ID {
		t.Fatalf("the second run is Process %s, want the running %s", again.ID, first.ID)
	}
	if n := len(f.daemon.Registrations()); n != 1 {
		t.Errorf("kitbashd holds %d registrations, want 1", n)
	}
	if f.daemon.Token(first.ID) != token {
		t.Error("the second run minted a new token, which the running container does not have")
	}
	if n := registerCalls(f.daemon); n != 1 {
		t.Errorf("kitbashd was asked to register %d times, want 1", n)
	}
}

// A replaced Process is unregistered once its container is gone, not before:
// until then it is still running and still exporting.
func TestReplaceUnregistersTheProcessItReplaced(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")

	first, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	f.build(folder, "ffmpeg")
	second, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run again: %s", prob.Detail)
	}
	if second.ID == first.ID {
		t.Fatal("the replacement is the same Process, so nothing was replaced")
	}
	if got := f.daemon.Unregistered(); len(got) != 1 || got[0] != first.ID {
		t.Errorf("unregistered %v, want the replaced Process %s", got, first.ID)
	}
	if _, held := f.daemon.Registration(first.ID); held {
		t.Error("kitbashd still holds the replaced Process")
	}
	if _, held := f.daemon.Registration(second.ID); !held {
		t.Error("kitbashd does not know the Process that took its place")
	}
	// The container was removed before its registration went, so the Process
	// never lost its token while it was still running.
	if len(f.runner.Removed) != 1 {
		t.Fatalf("the runtime removed %d containers, want 1", len(f.runner.Removed))
	}
}

// A stopped Process cannot export, so its token is revoked with it.
func TestStopUnregistersTheProcess(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")

	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if _, prob := f.processes.Stop(ctx, process.ID); prob != nil {
		t.Fatalf("Stop: %s", prob.Detail)
	}
	if got := f.daemon.Unregistered(); len(got) != 1 || got[0] != process.ID {
		t.Errorf("unregistered %v, want the stopped Process %s", got, process.ID)
	}
	if _, held := f.daemon.Registration(process.ID); held {
		t.Error("kitbashd still holds a Process that is stopped")
	}
	// Stopping it again is not a failure, even though kitbashd no longer
	// knows the Process: 404 is what an already revoked token looks like.
	if _, prob := f.processes.Stop(ctx, process.ID); prob != nil {
		t.Fatalf("Stop again: %s", prob.Detail)
	}
}

// kitbashd writes the environment of the container it starts, so nothing the
// manifest claims of it ever crosses the socket: the start request carries the
// unit's own variables and none of kitbashd's, and the container ends up with
// the daemon's values.
//
// The Package here is the one that names kitbashd's own environment, because
// that is where it matters: a Process that kept the manifest's
// KITBASH_FANOUT_SECRET would accept fan out requests from whoever wrote the
// manifest rather than from kitbashd alone.
func TestTheEnvironmentKitbashdOwnsNeverCrossesTheSocket(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "forger", forgedManifest)
	f.build(folder, "forger")

	process, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	start, ok := f.daemon.Started(process.ID)
	if !ok {
		t.Fatalf("kitbashd was asked to start %v, want one start of %s", f.daemon.Starts(), process.ID)
	}
	for _, key := range telemetry.OwnedEnv {
		if value, set := start.Options.Env[key]; set {
			t.Errorf("%s crossed the socket as %q, and it is kitbashd's to write", key, value)
		}
	}
	if start.Options.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("the request environment is %v, want the manifest's own entries", start.Options.Env)
	}
	// What the container was started with is the daemon's answer, token and
	// secret included, not the manifest's claim.
	env := f.runner.Runs[0].Env
	if env[telemetry.EnvFanoutSecret] != f.daemon.FanoutSecret(process.ID) {
		t.Errorf("the fan out secret is %q, want the one kitbashd minted", env[telemetry.EnvFanoutSecret])
	}
	if env[telemetry.EnvToken] != f.daemon.Token(process.ID) {
		t.Errorf("the token is %q, want the one kitbashd minted", env[telemetry.EnvToken])
	}
}

// A restarted kitbashd learns the running set from the next session.
func TestReconcileRegistersWhatKitbashdDoesNotKnow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var lines bytes.Buffer
	f.processes.SetLogger(log.New(&lines, "", 0))
	folder := f.pack(t, "observer", observerManifest)
	f.build(folder, "observer")

	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	// kitbashd restarted: it holds one Process that is not running here and
	// has forgotten the one that is.
	f.daemon.AddProcess(teltest.Registration{ID: "gone", Package: folder, Name: "gone"})
	if prob := unregister(t, f.daemon, process.ID); prob != nil {
		t.Fatal(prob)
	}

	list, prob := f.processes.List(ctx)
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	registered, stale, prob := f.processes.Reconcile(ctx, list.Processes)
	if prob != nil {
		t.Fatalf("Reconcile: %s", prob.Detail)
	}
	if len(registered) != 1 || registered[0] != process.ID {
		t.Errorf("registered %v, want the running Process %s", registered, process.ID)
	}
	if len(stale) != 1 || stale[0] != "gone" {
		t.Errorf("stale is %v, want the Process that no longer runs here", stale)
	}
	got, ok := f.daemon.Registration(process.ID)
	if !ok {
		t.Fatal("the running Process was not registered again")
	}
	if got.Endpoint != process.Endpoint || got.Endpoint == "" {
		t.Errorf("the endpoint is %q, want the one the Process runs on, %q", got.Endpoint, process.Endpoint)
	}
	if len(got.Subscriptions) != 1 || got.Subscriptions[0] != "telemetry" {
		t.Errorf("subscriptions are %v, want [telemetry] from the manifest", got.Subscriptions)
	}
	// The re-registration minted a secret the running container does not
	// have, so its fan out is refused until it is run again. That gap is the
	// token's gap and is said rather than hidden.
	want := "Process " + process.ID + " was registered again; the running container holds the fan out secret this registration replaced and refuses every delivery, or was started with none"
	if !strings.Contains(lines.String(), want) {
		t.Errorf("the log is %q, want it to contain %q", lines.String(), want)
	}
	// A registration kitbashd holds for a container that is gone is left
	// alone: proc_stop is what removes one, and a failed delivery is all it
	// costs until then.
	if _, held := f.daemon.Registration("gone"); !held {
		t.Error("a stale registration was removed, which is kitbashd's to keep")
	}
}

// A registration whose container never started names an endpoint on this host,
// so kitbashd would fan the member's records out to whatever takes that
// loopback port next. It goes back off the registry.
func TestARegistrationWhoseContainerFailedToStartIsWithdrawn(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "observer", observerManifest)
	f.build(folder, "observer")
	f.daemon.AnswerStart(teltest.Problem(http.StatusInternalServerError, problem.SlugInternal,
		"Internal error", "kitbash could not complete this call; the cause is in the server log", ""))

	_, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob == nil {
		t.Fatal("a run the runtime refused was reported as a success")
	}
	// The runtime's own words go to the daemon log, never to the agent: the
	// command line they describe names the file holding the Process's token.
	if prob.Detail != "kitbash could not complete this call; the cause is in the server log" {
		t.Errorf("detail is %q, want the runtime's output kept out of it", prob.Detail)
	}
	registered := f.daemon.Registrations()
	if len(registered) != 0 {
		t.Errorf("kitbashd still holds %+v, want nothing for a container that never started", registered)
	}
	unregistered := f.daemon.Unregistered()
	if len(unregistered) != 1 {
		t.Fatalf("unregistered %v, want the one Process that failed to start", unregistered)
	}
	if len(f.runner.Runs) != 0 {
		t.Errorf("the session ran %d containers of its own, want none: kitbashd runs them", len(f.runner.Runs))
	}
	starts := f.daemon.Starts()
	if len(starts) != 1 {
		t.Fatalf("kitbashd was asked for %d starts, want one", len(starts))
	}
	if got := starts[0].Options.Labels[podman.LabelID]; got != unregistered[0] {
		t.Errorf("unregistered %s, want the id the start carried, %s", unregistered[0], got)
	}
}

// expose: http without a port has no endpoint to publish or to register, so
// the manifest says one thing and the Process would do another.
func TestHTTPWithoutAPortIsAnInvalidManifest(t *testing.T) {
	const noPort = `name: dashboard
description: A web interface whose manifest forgot to say which port it listens on.
deploy:
  units:
    - type: container
      build: .
      expose: http
`
	f := newFixture(t)
	folder := f.pack(t, "dashboard", noPort)
	f.build(folder, "dashboard")

	_, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob == nil {
		t.Fatal("an http Process with no port was started")
	}
	if prob.Slug() != problem.SlugInvalidManifest {
		t.Errorf("problem is %s, want invalid-manifest", prob.Slug())
	}
	if len(f.runner.Runs) != 0 {
		t.Error("the runtime was asked to start a Process with nothing to publish")
	}
	if n := len(f.daemon.Registrations()); n != 0 {
		t.Errorf("kitbashd holds %d registrations for a Process that was refused", n)
	}
}

// An admin's Process list is the whole machine. Another member's Process is
// neither missing from the registry nor stale because it is not running here.
func TestReconcileLeavesOtherMembersRegistrationsAlone(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// kitbashd names the member under owner, which is what an admin's list
	// carries for a Process that is not the caller's.
	f.daemon.AddProcess(teltest.Registration{
		ID: "theirs", Package: "/home/other/echo", Name: "echo", Owner: "other",
	})

	list, prob := f.processes.List(ctx)
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	registered, stale, prob := f.processes.Reconcile(ctx, list.Processes)
	if prob != nil {
		t.Fatalf("Reconcile: %s", prob.Detail)
	}
	if len(registered) != 0 || len(stale) != 0 {
		t.Errorf("reconciled %v and %v, want another member's Process left alone", registered, stale)
	}
}

// A Process that is stopped holds no registration of its own, so one kitbashd
// still has for it is stale rather than current.
func TestReconcileCountsOnlyRunningProcessesAsKnown(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")
	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if _, prob := f.processes.Stop(ctx, process.ID); prob != nil {
		t.Fatalf("Stop: %s", prob.Detail)
	}
	// kitbashd never heard the stop, which is what a daemon that was down for
	// it looks like.
	f.daemon.AddProcess(teltest.Registration{ID: process.ID, Package: folder, Name: "ffmpeg"})

	list, prob := f.processes.List(ctx)
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	registered, stale, prob := f.processes.Reconcile(ctx, list.Processes)
	if prob != nil {
		t.Fatalf("Reconcile: %s", prob.Detail)
	}
	if len(registered) != 0 {
		t.Errorf("registered %v, want nothing for a Process that is stopped", registered)
	}
	if len(stale) != 1 || stale[0] != process.ID {
		t.Errorf("stale is %v, want the stopped Process %s", stale, process.ID)
	}
}

// unregister drops one Process from the fake daemon the way proc_stop would.
func unregister(t *testing.T, daemon *teltest.Daemon, id string) error {
	t.Helper()
	client := telemetry.NewClient(daemon.Socket)
	if prob := client.UnregisterProcess(context.Background(), id); prob != nil {
		return prob
	}
	return nil
}

// registerCalls is how many times the registry was asked to register.
func registerCalls(daemon *teltest.Daemon) int {
	n := 0
	for _, call := range daemon.Calls() {
		if call.Method == "POST" && call.Path == telemetry.ProcessesPath {
			n++
		}
	}
	return n
}

// Every lifecycle call goes through kitbashd: it runs the container, it stops
// it and it removes the one a replacement takes the place of, because the
// cgroup the limits are enforced in is root's to write, see PLAN.md 2.3.
func TestTheLifecycleGoesThroughKitbashd(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")

	first, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	// A second build is a second digest, so this run replaces the first.
	f.build(folder, "ffmpeg")
	second, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run again: %s", prob.Detail)
	}
	if _, prob := f.processes.Stop(ctx, second.ID); prob != nil {
		t.Fatalf("Stop: %s", prob.Detail)
	}

	var actions []string
	for _, call := range f.daemon.Starts() {
		actions = append(actions, call.Action+" "+call.ID)
	}
	want := []string{
		"start " + first.ID,
		"remove " + first.ID,
		"start " + second.ID,
		"stop " + second.ID,
	}
	if len(actions) != len(want) {
		t.Fatalf("kitbashd was asked for %v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Errorf("call %d is %q, want %q", i, actions[i], want[i])
		}
	}
	// The session ran none of it itself: its own runtime saw the container
	// only because the daemon started it there.
	if len(f.runner.Stopped) != 1 || len(f.runner.Removed) != 1 {
		t.Errorf("the session stopped %v and removed %v of its own", f.runner.Stopped, f.runner.Removed)
	}
}

// A container kitbashd has no registration for is still the member's own, so
// the session stops it itself rather than leaving them with a Process they
// cannot stop.
func TestStopFallsBackToTheSessionWhenKitbashdForgot(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")
	process, prob := f.processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	// kitbashd restarted and lost the registration; the container is running.
	if err := unregister(t, f.daemon, process.ID); err != nil {
		t.Fatalf("unregistering: %v", err)
	}

	if _, prob := f.processes.Stop(ctx, process.ID); prob != nil {
		t.Fatalf("Stop: %s", prob.Detail)
	}
	if len(f.runner.Stopped) != 1 || f.runner.Stopped[0] != process.Container {
		t.Errorf("the session stopped %v, want the container kitbashd no longer knows", f.runner.Stopped)
	}
}

// The other way to have no kitbashd: a session with no registry at all, which
// is a host where the daemon was never configured. It refuses the same way a
// socket that is not there does, and starts nothing.
func TestRunWithoutARegistryIsRefused(t *testing.T) {
	f := newFixtureWithSocket(t, filepath.Join(t.TempDir(), "absent.sock"))
	f.processes = proc.New(f.files, f.runner, nil)
	var lines bytes.Buffer
	f.processes.SetLogger(log.New(&lines, "", 0))
	folder := f.pack(t, "forger", forgedManifest)
	f.build(folder, "forger")

	_, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob == nil {
		t.Fatal("a Process was started with no registry")
	}
	if prob.Fix != telemetry.NotRunningFix {
		t.Errorf("fix is %q, want %q", prob.Fix, telemetry.NotRunningFix)
	}
	if len(f.runner.Runs) != 0 {
		t.Errorf("the runtime was asked to run %d containers, want none", len(f.runner.Runs))
	}
}

// mountingManifest is a unit that asks to see two folders of Files: one of the
// member's own read write, and one of /org read only, see PLAN.md section 2.3.
const mountingManifest = `name: reader
description: A Package whose Process reads and writes folders of Files.
deploy:
  units:
    - type: container
      build: .
      expose: none
      mounts:
        - source: /home/tester/notes
          target: /files/notes
          mode: rw
        - source: /org/handbook
          target: /files/handbook
`

// The mounts a unit declares cross the socket with the registration, and they
// cross it unresolved: kitbashd resolves them as root, because this session
// runs as the member and what a member's process says about a path is a claim,
// see mounts in spec/kitbashd-api.yaml.
func TestRunRegistersTheMountsTheUnitDeclared(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "reader", mountingManifest)
	f.build(folder, "reader")

	if _, prob := f.processes.Run(context.Background(), folder, "", ""); prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	registrations := f.daemon.Registrations()
	if len(registrations) != 1 {
		t.Fatalf("kitbashd holds %d registrations, want 1", len(registrations))
	}
	got := registrations[0].Mounts
	if len(got) != 2 {
		t.Fatalf("the registration carries %+v, want the two mounts the unit declared", got)
	}
	if got[0].Source != "/home/tester/notes" || got[0].Target != "/files/notes" || got[0].Mode != "rw" {
		t.Errorf("the first mount is %+v, want the notes folder read write", got[0])
	}
	// The second declares no mode, and the manifest's default is ro. It is
	// sent as the manifest wrote it: filling it in here would be this session
	// deciding what the daemon decides.
	if got[1].Source != "/org/handbook" || got[1].Mode != "" {
		t.Errorf("the second mount is %+v, want the handbook folder as the manifest wrote it", got[1])
	}
}

// A Package that declares no mounts registers with none, which is every
// Package written before mounts existed.
func TestRunRegistersNoMountsForAUnitThatDeclaresNone(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")

	if _, prob := f.processes.Run(context.Background(), folder, "", ""); prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if got := f.daemon.Registrations()[0].Mounts; len(got) != 0 {
		t.Errorf("the registration carries the mounts %+v, want none", got)
	}
}

// proc_list reports the mounts kitbashd holds and not what the runtime has
// bound: the registration is what kitbash agreed to, and it is what a member
// reads to see which folders their Process can reach.
func TestListReportsTheMountsTheRegistryHolds(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "reader", mountingManifest)
	f.build(folder, "reader")

	process, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	list, prob := f.processes.List(context.Background())
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	var found bool
	for _, p := range list.Processes {
		if p.ID != process.ID {
			continue
		}
		found = true
		if len(p.Mounts) != 2 || p.Mounts[0].Target != "/files/notes" ||
			p.Mounts[1].Target != "/files/handbook" {
			t.Errorf("proc_list reports the mounts %+v, want the two the registration holds", p.Mounts)
		}
	}
	if !found {
		t.Fatalf("proc_list does not hold %s", process.ID)
	}
}

// A Process whose container kitbashd took apart still appears on proc_list,
// failed and with the reason. kitbashd removes the container of a Process whose
// mount turned out to be a folder it did not agree to, so there is nothing in
// the runtime to list: without this the Process would simply stop being listed
// and its owner would have no problem to read.
func TestListReportsAProcessWhoseContainerWasTakenApart(t *testing.T) {
	f := newFixture(t)
	const id = "01930000-0000-7000-8000-0000000000d1"
	f.daemon.AddProcess(teltest.Registration{
		ID:         id,
		Package:    "/home/tester/reader",
		Name:       "reader",
		Digest:     "sha256:" + strings.Repeat("a", 64),
		Expose:     manifest.ExposeNone,
		Problem:    "this Process declares a mount that is no longer legal, so kitbashd did not start it",
		ProblemFix: "Check the folder deploy.units[].mounts names.",
	})
	list, prob := f.processes.List(context.Background())
	if prob != nil {
		t.Fatalf("List: %s", prob.Detail)
	}
	var found bool
	for _, p := range list.Processes {
		if p.ID != id {
			continue
		}
		found = true
		if p.State != proc.StateFailed || p.Problem == "" || p.Fix == "" {
			t.Errorf("proc_list reports %+v, want it failed with the reason and a fix", p)
		}
	}
	if !found {
		t.Fatalf("proc_list dropped the Process whose container was taken apart: %+v", list.Processes)
	}
}

// secretsManifest is a unit that declares the names of two credentials it
// needs and cannot get from its image, see PLAN.md section 2.3.
const secretsManifest = `name: caller
description: A Package whose Process calls a service outside this machine.
deploy:
  units:
    - type: container
      build: .
      expose: none
      environment: { LOG_LEVEL: debug }
      secrets: [ANTHROPIC_API_KEY, OPENAI_API_KEY]
`

// The names a unit declares cross the socket with the registration, and the
// values never cross it at all: kitbashd reads them from root owned files at
// every start and writes them into the environment file itself, which is why
// the request body of proc_run carries names alone, see PLAN.md section 2.3.
func TestRunRegistersTheSecretNamesTheUnitDeclared(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "caller", secretsManifest)
	f.build(folder, "caller")

	if _, prob := f.processes.Run(context.Background(), folder, "", ""); prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	registrations := f.daemon.Registrations()
	if len(registrations) != 1 {
		t.Fatalf("kitbashd holds %d registrations, want 1", len(registrations))
	}
	got := registrations[0].Secrets
	if len(got) != 2 || got[0] != "ANTHROPIC_API_KEY" || got[1] != "OPENAI_API_KEY" {
		t.Errorf("the registration carries %v, want the two names the unit declared", got)
	}
	// The start request carries the manifest's own environment and nothing of
	// a secret: the daemon is the one that resolves a name to a value.
	starts := f.daemon.Starts()
	if len(starts) != 1 {
		t.Fatalf("kitbashd served %d starts, want 1", len(starts))
	}
	if starts[0].Options.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("the start carries the environment %v, want the unit's own", starts[0].Options.Env)
	}
	for _, name := range got {
		if _, sent := starts[0].Options.Env[name]; sent {
			t.Errorf("the start request carries %s, want the secret written by the daemon alone", name)
		}
	}
}

// A Package that declares no secrets registers with none, which is every
// Package written before secrets existed.
func TestRunRegistersNoSecretsForAUnitThatDeclaresNone(t *testing.T) {
	f := newFixture(t)
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")

	if _, prob := f.processes.Run(context.Background(), folder, "", ""); prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if got := f.daemon.Registrations()[0].Secrets; len(got) != 0 {
		t.Errorf("the registration carries the secrets %v, want none", got)
	}
}
