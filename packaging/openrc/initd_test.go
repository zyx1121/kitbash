// Package openrc holds no Go code. The test below is here because the service
// script beside it is shell that nothing else in this repository would ever
// parse: a typo in it is found on the host, at boot, or it is found here.
package openrc

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// scriptPath is the OpenRC service script kitbash installs as
// /etc/init.d/kitbashd.
const scriptPath = "kitbashd.initd"

// sh -n parses the script without running any of it, which is the whole check
// an operator would otherwise make by restarting the daemon.
func TestTheServiceScriptParses(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh on this host: %v", err)
	}
	out, err := exec.Command(sh, "-n", scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("sh -n %s: %v\n%s", scriptPath, err, out)
	}
}

// The health check and the respawn settings are what brings kitbashd back
// when it stops answering, see issue #67. A script that lost either would
// still parse, so each is named here.
func TestTheServiceScriptChecksHealthAndRespawns(t *testing.T) {
	body, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("reading %s: %v", scriptPath, err)
	}
	script := string(body)
	for _, want := range []string{
		"healthcheck() {",
		"healthcheck_timer=60",
		"--unix-socket /run/kitbash/kitbashd.sock",
		"/kitbash/v1/health",
		"supervisor=\"supervise-daemon\"",
		"respawn_delay=5",
		"respawn_max=0",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("%s does not carry %q, so a daemon that stops answering is not restarted", scriptPath, want)
		}
	}
}
