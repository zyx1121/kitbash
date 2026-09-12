package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The upgrade half of the job, which is issue #74 without an apk: the previous
// release writes a store and registers a Process, and the current release opens
// that same store and carries the Process on. The workflow owns the daemons;
// these two tests are the halves that run either side of the swap.
//
// KITBASH_E2E_MCP points at the previous release's kitbash-mcp for the first
// and at the current one for the second. KITBASH_E2E_STORE is the store both
// daemons open, and KITBASH_E2E_STATE is the file the first leaves for the
// second.
const (
	storeEnv = "KITBASH_E2E_STORE"
	stateEnv = "KITBASH_E2E_STATE"
)

// registration is what the previous release left behind.
type registration struct {
	ID      string `json:"id"`
	Package string `json:"package"`
	Digest  string `json:"digest"`
}

// upgradePackage is the Package of this half. It is not the echo fixture: that
// manifest carries a permits block, which the release being upgraded from does
// not know and its schema therefore refuses. The container is the same server.
const upgradePackage = "upgrade"

// TestUpgradePreviousRelease registers a Process with the release that is being
// upgraded from. It runs against that release's binaries and its daemon.
func TestUpgradePreviousRelease(t *testing.T) {
	requireHost(t)
	// The release under test is the one the job installed, daemon and session
	// binary alike. Without this the whole half could run twice against the
	// same build and still pass.
	theRelease(t)

	admin := dial(t, adminName())
	pkgPath := "/home/" + adminName() + "/" + upgradePackage

	// The Package, written by the previous release through its own surface.
	sources := map[string]string{
		"kitbash.yaml": filepath.Join("fixtures", upgradePackage, "kitbash.yaml"),
		"Dockerfile":   filepath.Join("fixtures", packageName, "Dockerfile"),
		"server.js":    filepath.Join("fixtures", packageName, "server.js"),
	}
	for _, name := range fixtures {
		body, err := os.ReadFile(sources[name])
		if err != nil {
			t.Fatalf("reading the fixture %s: %v", name, err)
		}
		admin.ok("fs_write", map[string]any{
			"path":    filepath.Join(pkgPath, name),
			"content": string(body),
			"message": "Add " + name + " of the upgrade Package",
		}, nil)
	}

	// The layers are the echo fixture's, so this build is a cache lookup.
	var built struct {
		Digest string `json:"digest"`
	}
	res := admin.callWithin(buildTimeout, "pkg_build", map[string]any{"path": pkgPath})
	res.mustSucceed(t, "pkg_build")
	if err := json.Unmarshal(res.Structured, &built); err != nil {
		t.Fatalf("decoding pkg_build: %v", err)
	}

	var started struct {
		ID     string `json:"id"`
		State  string `json:"state"`
		Digest string `json:"digest"`
	}
	admin.ok("proc_run", map[string]any{"package": pkgPath}, &started)
	if started.State != "running" || started.ID == "" {
		t.Fatalf("proc_run answered %+v, want a running Process", started)
	}
	admin.close()

	body, err := json.Marshal(registration{ID: started.ID, Package: pkgPath, Digest: started.Digest})
	if err != nil {
		t.Fatalf("encoding the registration: %v", err)
	}
	if err := os.WriteFile(statePath(t), body, 0o644); err != nil {
		t.Fatalf("writing %s: %v", statePath(t), err)
	}
	t.Logf("the previous release registered %s at %s", started.ID, built.Digest)
}

// TestUpgradeCurrentRelease opens the store the previous release wrote with the
// current daemon and requires the Process to have survived the swap.
func TestUpgradeCurrentRelease(t *testing.T) {
	requireHost(t)
	want := readState(t)

	// 1. This release answers on the store the previous release wrote.
	answer := theRelease(t)
	if store := os.Getenv(storeEnv); store != "" && answer.Store != store {
		t.Fatalf("the daemon answers for the store %s, want %s", answer.Store, store)
	}

	// 2. The registration the previous release wrote is still a Process.
	admin := dial(t, adminName())
	var list struct {
		Processes []struct {
			ID      string `json:"id"`
			Package string `json:"package"`
			Digest  string `json:"digest"`
			State   string `json:"state"`
		} `json:"processes"`
	}
	admin.ok("proc_list", map[string]any{}, &list)
	var found bool
	for _, process := range list.Processes {
		if process.ID != want.ID {
			continue
		}
		found = true
		if process.Package != want.Package || process.Digest != want.Digest {
			t.Fatalf("the registration reads back as %+v, want %+v", process, want)
		}
		if process.State != "running" {
			t.Fatalf("the Process is %s after the upgrade, want running", process.State)
		}
	}
	if !found {
		t.Fatalf("proc_list does not hold %s: %+v", want.ID, list.Processes)
	}

	// 3. A run still works: the Process is stopped and started again through
	//    the current binaries, on the store the previous release created.
	var stopped struct {
		State string `json:"state"`
	}
	admin.ok("proc_stop", map[string]any{"id": want.ID}, &stopped)
	if stopped.State != "stopped" {
		t.Fatalf("proc_stop answered %+v, want the stopped state", stopped)
	}
	var restarted struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	admin.ok("proc_run", map[string]any{"package": want.Package}, &restarted)
	if restarted.State != "running" {
		t.Fatalf("proc_run answered %+v after the upgrade, want a running Process", restarted)
	}
	admin.close()
}

// health is what the health path answers, as far as these tests read it.
type health struct {
	Version string `json:"version"`
	Store   string `json:"store"`
}

// theRelease requires the daemon on the socket and the kitbash-mcp a session
// runs to be the release this step installed, which KITBASH_E2E_VERSION names.
// The upgrade half is two halves of the same job on one host: if either binary
// were the other release, both halves would pass and prove nothing.
func theRelease(t *testing.T) health {
	t.Helper()
	want := os.Getenv(versionEnv)
	if want == "" {
		t.Fatalf("%s does not name the release this step installed", versionEnv)
	}
	var answer health
	if err := json.Unmarshal(daemonHealth(t), &answer); err != nil {
		t.Fatalf("decoding the health of the daemon: %v", err)
	}
	if answer.Version != want {
		t.Fatalf("the daemon on %s is %s, want %s", socketPath, answer.Version, want)
	}
	out, err := runAs(t, adminName(), mcpBinary(), "--version")
	if err != nil {
		t.Fatalf("asking %s its version: %v\n%s", mcpBinary(), err, out)
	}
	if got := lastLine(out); got != want {
		t.Fatalf("%s is %s, want %s", mcpBinary(), got, want)
	}
	t.Logf("the daemon and %s are both %s, on %s", mcpBinary(), want, answer.Store)
	return answer
}

// lastLine is the answer a command printed, after whatever the job's own sudo
// and cgroup placement said before it.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func statePath(t *testing.T) string {
	t.Helper()
	path := os.Getenv(stateEnv)
	if path == "" {
		t.Fatalf("%s names no file to carry the registration between the two halves", stateEnv)
	}
	return path
}

func readState(t *testing.T) registration {
	t.Helper()
	body, err := os.ReadFile(statePath(t))
	if err != nil {
		t.Fatalf("reading %s: %v", statePath(t), err)
	}
	var want registration
	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatalf("decoding %s: %v", statePath(t), err)
	}
	return want
}

// daemonHealth reads the health path over the unix socket. The socket is
// root's and every member's, and this test runs as neither, so curl is sent
// through sudo the way the service script's healthcheck runs it.
func daemonHealth(t *testing.T) []byte {
	t.Helper()
	out, err := exec.Command("sudo", "-n", "curl", "--silent", "--fail", "--max-time", "10",
		"--unix-socket", socketPath, "http://localhost/kitbash/v1/health").Output()
	if err != nil {
		t.Fatalf("the daemon did not answer on %s: %v", socketPath, err)
	}
	return out
}
