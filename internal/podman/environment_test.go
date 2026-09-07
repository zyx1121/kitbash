package podman

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// One of a Process's variables is its Telemetry token, so a command line that
// carries it is readable in /proc/<pid>/cmdline and lands in the server log
// the first time the runtime refuses a run. Neither may happen.
func TestRunKeepsEnvironmentValuesOutOfItsErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	secrets := map[string]string{
		"KITBASH_TELEMETRY_TOKEN": "s3cret-token-value",
		"LOG_LEVEL":               "debug",
		// A value with a line break has no spelling in an env file, so it
		// stays on the command line and has to be redacted there.
		"BANNER": "first line\nsecond line",
	}

	// The image does not exist, so this fails whether or not the host has a
	// container runtime at all. What is under test is the error, not the run.
	_, err := (&CLI{}).Run(ctx, RunOptions{
		Name:  "kitbash-test-does-not-exist",
		Image: "localhost/kitbash-test/does-not-exist:0",
		Env:   secrets,
	})
	if err == nil {
		t.Fatal("a container was started from an image that does not exist")
	}
	for key, value := range secrets {
		if strings.Contains(err.Error(), value) {
			t.Errorf("the error carries the value of %s: %s", key, err)
		}
	}
	if !strings.Contains(err.Error(), "KITBASH_TELEMETRY_TOKEN") &&
		!strings.Contains(err.Error(), "--env-file") {
		t.Errorf("the error names neither the environment file nor the keys: %s", err)
	}
}

func TestRedactHidesEveryEnvironmentValue(t *testing.T) {
	got := redact([]string{
		"run", "--name", "kitbash-echo-echo",
		"--env", "KITBASH_TELEMETRY_TOKEN=s3cret",
		"--env", "PASSED_THROUGH",
		"--label", "kitbash.user=tester",
		"sha256:abc",
	})
	want := "run --name kitbash-echo-echo " +
		"--env KITBASH_TELEMETRY_TOKEN=<redacted> --env PASSED_THROUGH " +
		"--label kitbash.user=tester sha256:abc"
	if got != want {
		t.Errorf("the redacted command line is\n%s\nwant\n%s", got, want)
	}
}

// The environment file exists for one run and only its owner may read it.
func TestWriteEnvFileIsPrivateAndTemporary(t *testing.T) {
	path, inline, cleanup, err := writeEnvFile(map[string]string{
		"KITBASH_TELEMETRY_TOKEN": "s3cret",
		"LOG_LEVEL":               "debug",
		"BANNER":                  "first\nsecond",
	})
	if err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}
	if cleanup == nil {
		t.Fatal("the environment file has no cleanup, so it outlives the run")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the environment file is %s, want 0600", info.Mode().Perm())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(body) != "KITBASH_TELEMETRY_TOKEN=s3cret\nLOG_LEVEL=debug\n" {
		t.Errorf("the environment file holds %q", string(body))
	}
	if len(inline) != 1 || inline[0] != "BANNER" {
		t.Errorf("inline keys are %v, want the one whose value has a line break", inline)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the environment file outlived the run")
	}
}

func TestWriteEnvFileWritesNothingForAnEmptyEnvironment(t *testing.T) {
	path, inline, cleanup, err := writeEnvFile(nil)
	if err != nil || path != "" || inline != nil || cleanup != nil {
		t.Errorf("an empty environment produced %q, %v, %v, %v", path, inline, cleanup != nil, err)
	}
}
