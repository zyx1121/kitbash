// Package deploy holds no Go code. The tests below are here because
// install.sh is shell that nothing else in this repository would ever run: the
// script puts a host together as root, so what can be checked without a host
// is checked here, and the rest is checked by running it on one.
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scriptPath is the installer, which the apk also ships as
// /usr/share/kitbash/install.sh.
const scriptPath = "install.sh"

// sh -n parses the script without running any of it.
func TestTheInstallerParses(t *testing.T) {
	out, err := exec.Command(shell(t), "-n", scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("sh -n %s: %v\n%s", scriptPath, err, out)
	}
}

// shell is the POSIX shell this host has, or the test is skipped.
func shell(t *testing.T) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh on this host: %v", err)
	}
	return sh
}

// script is install.sh as text.
func script(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("reading %s: %v", scriptPath, err)
	}
	return string(body)
}

// cut takes the part of the installer between two markers, so a test runs one
// step of it rather than the whole thing, which needs a host and root.
func cut(t *testing.T, from, to string) string {
	t.Helper()
	body := script(t)
	start := strings.Index(body, from)
	if start < 0 {
		t.Fatalf("%s no longer carries %q", scriptPath, from)
	}
	end := strings.Index(body[start:], to)
	if end < 0 {
		t.Fatalf("%s no longer carries %q after %q", scriptPath, to, from)
	}
	return body[start : start+end]
}

// run executes a fragment of the installer with sh, in a directory of the
// test's own.
func run(t *testing.T, fragment string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fragment.sh")
	if err := os.WriteFile(path, []byte(fragment), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(shell(t), path).CombinedOutput()
	if err != nil {
		t.Fatalf("running the fragment: %v\n%s", err, out)
	}
	return string(out)
}

// The domain an operator gives the host is written to /etc/conf.d/kitbashd,
// which is what makes it survive a reboot and an upgrade. Running install.sh
// twice has to leave the same file as running it once: an installer that
// appended would leave a file with two answers in it, and the last line wins
// by luck rather than by rule.
func TestTheSettingsFileIsWrittenOnce(t *testing.T) {
	editor := cut(t, "set_confd() {", "if [ -n \"${KITBASH_DOMAIN+set}\"")
	dir := t.TempDir()
	confd := filepath.Join(dir, "kitbashd")
	// The file the apk ships, which documents the setting commented out.
	if err := os.WriteFile(confd, []byte("# a comment\n#KITBASH_DOMAIN=\"kitbash.example.org\"\n#KITBASH_TLS=\"acme\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := run(t, "set -eu\nconfd="+confd+"\n"+editor+`
set_confd KITBASH_DOMAIN kitbash.example
set_confd KITBASH_TLS gateway
set_confd KITBASH_DOMAIN kitbash.example
printf 'domain=%s tls=%s\n' "$(confd_value KITBASH_DOMAIN)" "$(confd_value KITBASH_TLS)"
`)
	if want := "domain=kitbash.example tls=gateway\n"; out != want {
		t.Errorf("the settings read back as %q, want %q", out, want)
	}
	body, err := os.ReadFile(confd)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(body), "KITBASH_DOMAIN="); got != 1 {
		t.Errorf("%s carries KITBASH_DOMAIN %d times, want once however often the installer runs:\n%s",
			confd, got, body)
	}
	if !strings.Contains(string(body), "# a comment") {
		t.Errorf("the installer dropped a line it does not speak for:\n%s", body)
	}
}

// The ruleset is what decides who may reach the proxy, so it is rendered here
// rather than read: the ports are opened only on a host that has a domain, and
// 443 only where this host holds the certificates.
func TestTheRulesetOpensTheProxyOnlyWithADomain(t *testing.T) {
	// The whole generated ruleset, with the path it is written to replaced by
	// one the test owns.
	fragment := cut(t, "cat > /etc/nftables.nft.kitbash-new", "if nft -c -f")
	cases := []struct {
		domain  string
		tls     string
		gateway string
		http    bool
		https   bool
		from    string
	}{
		{"", "acme", "", false, false, ""},
		{"kitbash.example", "acme", "", true, true, ""},
		{"kitbash.example", "gateway", "", true, false, ""},
		// A gateway that was named is the only source 80 is open to: the proxy
		// is reachable through it and from nowhere else.
		{"kitbash.example", "gateway", "203.0.113.10", true, false, "ip saddr 203.0.113.10"},
		{"kitbash.example", "gateway", "2001:db8::1", true, false, "ip6 saddr 2001:db8::1"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		ruleset := filepath.Join(dir, "nftables.nft")
		body := strings.ReplaceAll(fragment, "/etc/nftables.nft.kitbash-new", ruleset)
		run(t, "set -eu\ndomain='"+c.domain+"'\ntls='"+c.tls+"'\ngateway='"+c.gateway+"'\n"+body)
		rendered, err := os.ReadFile(ruleset)
		if err != nil {
			t.Fatal(err)
		}
		got := string(rendered)
		if strings.Contains(got, "tcp dport 80 ") != c.http {
			t.Errorf("domain %q, tls %s: 80 open = %v, want %v:\n%s",
				c.domain, c.tls, !c.http, c.http, got)
		}
		if strings.Contains(got, "tcp dport 443 ") != c.https {
			t.Errorf("domain %q, tls %s: 443 open = %v, want %v:\n%s",
				c.domain, c.tls, !c.https, c.https, got)
		}
		if c.from != "" && !strings.Contains(got, "tcp dport 80 "+c.from+" accept") {
			t.Errorf("domain %q, gateway %q: 80 is not narrowed to the gateway:\n%s",
				c.domain, c.gateway, got)
		}
		if c.from == "" && strings.Contains(got, "saddr") {
			t.Errorf("domain %q: 80 names a source address and no gateway was given:\n%s", c.domain, got)
		}
		// Whatever else changed, the receiver stays off the wire and SSH
		// stays first: the way back into the host never depends on a rule
		// further down.
		if !strings.Contains(got, "tcp dport 4318 drop") {
			t.Errorf("domain %q: the Process receiver is no longer scoped:\n%s", c.domain, got)
		}
		if ssh := strings.Index(got, "tcp dport 22 accept"); ssh < 0 ||
			ssh > strings.Index(got, "tcp dport 4318 drop") {
			t.Errorf("domain %q: SSH is no longer the first rule:\n%s", c.domain, got)
		}
	}
}

// nft is on a kitbash host and may be on this one. Where it is, and where this
// test can use it, the generated ruleset is parsed by the thing that will load
// it. nft -c reads the kernel's own tables to resolve what it checks, which
// needs root, so this runs where install.sh itself runs and is skipped
// elsewhere; what the rules say is checked above without it.
func TestTheGeneratedRulesetParses(t *testing.T) {
	nft, err := exec.LookPath("nft")
	if err != nil {
		t.Skipf("no nft on this host: %v", err)
	}
	if os.Geteuid() != 0 {
		t.Skip("nft -c needs root to read the kernel's tables")
	}
	fragment := cut(t, "cat > /etc/nftables.nft.kitbash-new", "if nft -c -f")
	for _, domain := range []string{"", "kitbash.example"} {
		dir := t.TempDir()
		ruleset := filepath.Join(dir, "nftables.nft")
		body := strings.ReplaceAll(fragment, "/etc/nftables.nft.kitbash-new", ruleset)
		// The include at the end names a directory this test does not have,
		// so it is pointed at an empty one of its own.
		include := filepath.Join(dir, "nftables.d")
		if err := os.MkdirAll(include, 0o755); err != nil {
			t.Fatal(err)
		}
		body = strings.ReplaceAll(body, "/etc/nftables.d/", include+"/")
		run(t, "set -eu\ndomain='"+domain+"'\ntls=acme\ngateway=''\n"+body)
		out, err := exec.Command(nft, "-c", "-f", ruleset).CombinedOutput()
		if err != nil {
			t.Errorf("domain %q: nft -c: %v\n%s", domain, err, out)
		}
	}
}
