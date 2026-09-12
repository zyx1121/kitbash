package bridge_test

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/bridge"
	"github.com/zyx1121/kitbash/internal/fs"
	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/podman"
	"github.com/zyx1121/kitbash/internal/proc"
	"github.com/zyx1121/kitbash/internal/telemetry"
	"github.com/zyx1121/kitbash/internal/telemetry/teltest"
)

// subscriberManifest is a Process that talks to Telemetry only, so a session
// that syncs it is exercising the registry and nothing else.
const subscriberManifest = `name: observer
description: Receives every stored Telemetry record and writes judgments back.
provides:
  subscriptions: [telemetry]
deploy:
  units:
    - type: container
      build: .
      expose: http
      port: 9090
`

// A session tells kitbashd about the Processes that are running here and that
// it does not know, which is how a restarted daemon learns the running set,
// see client_behaviour.processes in spec/kitbashd-api.yaml.
func TestSyncReRegistersProcessesKitbashdForgot(t *testing.T) {
	ctx := context.Background()
	daemon, err := teltest.Start()
	if err != nil {
		t.Fatalf("teltest.Start: %v", err)
	}
	t.Cleanup(daemon.Close)

	root := t.TempDir()
	files, err := fs.New("tester", []string{root})
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	folder := filepath.Join(root, "observer")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(folder, manifest.FileName), subscriberManifest)

	runner := podman.NewFake()
	daemon.MirrorRuns(runner)
	client := telemetry.NewClient(daemon.Socket)
	processes := proc.New(files, runner, client)
	if _, _, err := runner.Build(ctx, folder, folder+"/Containerfile", "localhost/kitbash/observer:test",
		map[string]string{podman.LabelPath: folder, podman.LabelName: "observer", podman.LabelUser: "tester"}); err != nil {
		t.Fatalf("building: %v", err)
	}
	process, prob := processes.Run(ctx, folder, "", "")
	if prob != nil {
		t.Fatalf("proc.Run: %s", prob.Detail)
	}

	// kitbashd restarted: the Process is still running, its registration is
	// not, and one registration is left over from a container that is gone.
	if prob := client.UnregisterProcess(ctx, process.ID); prob != nil {
		t.Fatalf("unregistering: %s", prob.Detail)
	}
	daemon.AddProcess(teltest.Registration{ID: "gone", Package: folder, Name: "gone"})

	var lines bytes.Buffer
	tools := bridge.New(files, processes, runner)
	tools.SetLogger(log.New(&lines, "", 0))
	tools.Attach(mcp.NewServer(&mcp.Implementation{Name: "kitbash", Version: "test"}, nil))
	tools.Sync(ctx)

	got, ok := daemon.Registration(process.ID)
	if !ok {
		t.Fatal("the running Process was not registered again at session start")
	}
	if got.Package != folder || got.Endpoint != process.Endpoint || got.Endpoint == "" {
		t.Errorf("the registration is %+v, want the Package and endpoint the Process runs with", got)
	}
	if len(got.Subscriptions) != 1 || got.Subscriptions[0] != "telemetry" {
		t.Errorf("subscriptions are %v, want [telemetry] from the manifest", got.Subscriptions)
	}
	// Re-registration mints a token the container does not have, so the gap
	// is said plainly rather than hidden.
	want := "re-registered " + process.ID + "; its exports resume when it is run again"
	if !strings.Contains(lines.String(), want) {
		t.Errorf("the log is %q, want it to contain %q", lines.String(), want)
	}
	// A registration for a container that no longer exists here is kitbashd's
	// to keep: a failed delivery is all it costs.
	if _, held := daemon.Registration("gone"); !held {
		t.Error("a session removed a registration that is not its own to remove")
	}
	if !strings.Contains(lines.String(), "gone") {
		t.Errorf("the log is %q, want the stale registration named in it", lines.String())
	}
	if got := daemon.Unregistered(); len(got) != 1 || got[0] != process.ID {
		t.Errorf("unregistered %v, want only the one this test made", got)
	}
}
