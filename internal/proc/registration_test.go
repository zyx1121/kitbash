package proc_test

import (
	"bytes"
	"context"
	"errors"
	"log"
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
      env:
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

// The endpoint override exists for tests, the same rule as KITBASH_SOCKET.
func TestTheProcessEndpointCanBeOverriddenOutsideSSH(t *testing.T) {
	t.Setenv(telemetry.ProcessEndpointEnv, "http://127.0.0.1:4318")
	t.Setenv("SSH_CONNECTION", "")
	f := newFixture(t)
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")

	if _, prob := f.processes.Run(context.Background(), folder, "", ""); prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if got := f.runner.Runs[0].Env[telemetry.EnvEndpoint]; got != "http://127.0.0.1:4318" {
		t.Errorf("endpoint is %q, want the override", got)
	}
	// The two endpoints are one listener, so the override moves both.
	if got := f.runner.Runs[0].Env[telemetry.EnvMCPEndpoint]; got != "http://127.0.0.1:4318/mcp" {
		t.Errorf("the MCP endpoint is %q, want the override with the MCP path", got)
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

// The container runtime does not depend on kitbashd, so a host without it
// still runs Packages. The Process is simply untraced, and the session says so
// once.
func TestRunWithoutKitbashdStartsTheProcessUntraced(t *testing.T) {
	f := newFixtureWithSocket(t, filepath.Join(t.TempDir(), "absent.sock"))
	var lines bytes.Buffer
	f.processes.SetLogger(log.New(&lines, "", 0))
	folder := f.pack(t, "ffmpeg", mcpManifest)
	f.build(folder, "ffmpeg")

	process, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob != nil {
		t.Fatalf("Run: %s", prob.Detail)
	}
	if process.State != proc.StateRunning {
		t.Errorf("state is %s, want running without kitbashd", process.State)
	}
	env := f.runner.Runs[0].Env
	for _, key := range []string{
		telemetry.EnvEndpoint, telemetry.EnvToken,
		telemetry.EnvProcess, telemetry.EnvPackage, telemetry.EnvUser,
		telemetry.EnvFanoutSecret,
	} {
		if _, set := env[key]; set {
			t.Errorf("%s is in the environment, but no registration minted it", key)
		}
	}
	if env["LOG_LEVEL"] != "debug" {
		t.Errorf("env is %v, want the manifest's own entries", env)
	}
	want := "kitbashd is not running; Process " + process.ID + " starts untraced"
	if !strings.Contains(lines.String(), want) {
		t.Errorf("the log is %q, want it to contain %q", lines.String(), want)
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
	want := "Process " + process.ID + " was registered again; the running container holds the previous fan out secret"
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
	f.runner.RunErr = errors.New("the runtime refused the command")

	_, prob := f.processes.Run(context.Background(), folder, "", "")
	if prob == nil {
		t.Fatal("a run the runtime refused was reported as a success")
	}
	// The runtime's own words go to the server log, never to the agent: the
	// command line they describe carries the Process's Telemetry token.
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
	if got := f.runner.Runs[0].Labels[podman.LabelID]; got != unregistered[0] {
		t.Errorf("unregistered %s, want the id the run carried, %s", unregistered[0], got)
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
