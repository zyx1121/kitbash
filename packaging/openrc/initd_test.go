// Package openrc holds no Go code. The test below is here because the service
// script beside it is shell that nothing else in this repository would ever
// parse: a typo in it is found on the host, at boot, or it is found here.
package openrc

import (
	"os"
	"os/exec"
	"regexp"
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

// prereqsPath is the shared script the apk installs as
// /usr/share/kitbash/rootless-prereqs.sh, relative to this package.
const prereqsPath = "../../deploy/rootless-prereqs.sh"

// The rootless prerequisites are / rshared and one /run/user directory per
// member, and boot restore starts every registered Process as its owner, so it
// needs both. They used to come from /etc/local.d/kitbash-rootless.start,
// which made this service after local, and deploy/install.sh run from a
// local.d script then deadlocked the boot (issue #88). start_pre is where they
// belong; a service script that went back to local would boot a host with no
// console to fix it from.
func TestTheServiceScriptRunsTheRootlessPrerequisitesAndNotAfterLocal(t *testing.T) {
	body, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("reading %s: %v", scriptPath, err)
	}
	script := string(body)
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "after local") {
			t.Errorf("%s is after local again, which deadlocks an install.sh run from local.d (issue #88)", scriptPath)
		}
	}
	if !strings.Contains(script, "/usr/share/kitbash/rootless-prereqs.sh") {
		t.Errorf("%s does not run the rootless prerequisites, so boot restore has no /run/user to start a Process in", scriptPath)
	}
	if !strings.Contains(script, "ewarn") {
		t.Errorf("%s skips the rootless prerequisites silently when they are missing, which looks like a daemon that simply lost every Process", scriptPath)
	}
}

// The path the service script runs is the path the apk has to install the
// shared script at. They are in different files and nothing but this test
// reads both, so a package() that stopped shipping it would only be found on
// a host whose Processes did not come back.
func TestTheApkInstallsTheRootlessPrerequisites(t *testing.T) {
	body, err := os.ReadFile("../apk/APKBUILD")
	if err != nil {
		t.Fatalf("reading ../apk/APKBUILD: %v", err)
	}
	const want = `install -Dm755 deploy/rootless-prereqs.sh "$pkgdir"/usr/share/kitbash/rootless-prereqs.sh`
	if !strings.Contains(string(body), want) {
		t.Errorf("packaging/apk/APKBUILD does not carry %q, so %s runs a script that is not on the host", want, scriptPath)
	}
}

// The shared script is shell too, and it is the only copy: the service script
// and the local.d hook deploy/install.sh writes both call this one.
func TestTheRootlessPrerequisitesScriptParses(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh on this host: %v", err)
	}
	out, err := exec.Command(sh, "-n", prereqsPath).CombinedOutput()
	if err != nil {
		t.Fatalf("sh -n %s: %v\n%s", prereqsPath, err, out)
	}
	body, err := os.ReadFile(prereqsPath)
	if err != nil {
		t.Fatalf("reading %s: %v", prereqsPath, err)
	}
	for _, want := range []string{"mount --make-rshared /", "/run/user/$uid"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("%s does not carry %q, which rootless podman needs before any Process starts", prereqsPath, want)
		}
	}
}

// confdPath is the settings file the apk installs as /etc/conf.d/kitbashd.
const confdPath = "kitbashd.confd"

// OpenRC sources /etc/conf.d/kitbashd into the service script's own shell, and
// a shell variable is not in the environment of the daemon that script starts.
// kitbashd reads every setting from its environment, so a name the conf.d file
// documents and the script does not export is a setting an operator writes and
// nothing reads, see PLAN.md section 2.3.
func TestTheServiceScriptExportsEverySettingTheConfdFileDocuments(t *testing.T) {
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("reading %s: %v", scriptPath, err)
	}
	settings, err := os.ReadFile(confdPath)
	if err != nil {
		t.Fatalf("reading %s: %v", confdPath, err)
	}
	named := regexp.MustCompile(`(?m)^#?(KITBASH_[A-Z_]+)=`).FindAllStringSubmatch(string(settings), -1)
	if len(named) == 0 {
		t.Fatalf("%s documents no setting at all", confdPath)
	}
	exported := map[string]bool{}
	for _, line := range strings.Split(string(script), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 || fields[0] != "export" {
			continue
		}
		for _, name := range fields[1:] {
			exported[name] = true
		}
	}
	for _, match := range named {
		if !exported[match[1]] {
			t.Errorf("%s documents %s and %s does not export it, so kitbashd never reads it",
				confdPath, match[1], scriptPath)
		}
	}
}

// The proxy keeps its certificates beside the store and the secrets, and a
// private key is the proof this host is the name it serves, so the directory
// is narrowed here as well as by the daemon.
func TestTheServiceScriptNarrowsTheCertificateDirectory(t *testing.T) {
	body, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("reading %s: %v", scriptPath, err)
	}
	if !strings.Contains(string(body), "--mode 0700 --owner root:root /var/lib/kitbash/certs") {
		t.Errorf("%s does not make /var/lib/kitbash/certs root owned and 0700", scriptPath)
	}
}
