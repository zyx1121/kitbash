package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The M9 half of the job, PLAN.md 5.3: a member calls secrets_set with a name
// and a value, secrets_list shows the name and no value, a Package whose unit
// declares that name runs and its tool reads the variable, proc_run of a
// Package declaring the same name under a second member who has not set it is
// refused naming the secret, and the span secrets_set recorded in Telemetry
// carries no value.
//
// The Package is the reader fixture, already built by the mounts half: what a
// secret changes is the manifest and the environment the container is created
// with, so the image is the same one and this costs the job no build.

// The name the member declares and the variable the fixture reads back. The
// value is made fresh at each run so a stale one cannot pass this.
const (
	secretName  = "E2E_SECRET"
	secretBlock = "[" + secretName + "]"
)

// theMemberSetsASecret is the first clause: the value crosses the surface once,
// the listing answers the name and the timestamp, and no value comes back.
func theMemberSetsASecret(t *testing.T, s *state) {
	s.secret = fmt.Sprintf("sk-e2e-%d", time.Now().UnixNano())

	var set struct {
		Name    string `json:"name"`
		Updated string `json:"updated"`
	}
	s.member.ok("secrets_set", map[string]any{"name": secretName, "value": s.secret}, &set)
	if set.Name != secretName || set.Updated == "" {
		t.Fatalf("secrets_set answered %+v, want the name and a timestamp", set)
	}

	listed := s.member.call("secrets_list", map[string]any{})
	listed.mustSucceed(t, "secrets_list")
	if strings.Contains(listed.text(), s.secret) {
		t.Fatalf("secrets_list answered %s, want the name and no value", truncate(listed.text()))
	}
	var list struct {
		Secrets []struct {
			Name    string `json:"name"`
			Updated string `json:"updated"`
		} `json:"secrets"`
	}
	s.member.ok("secrets_list", map[string]any{}, &list)
	if len(list.Secrets) != 1 || list.Secrets[0].Name != secretName || list.Secrets[0].Updated == "" {
		t.Fatalf("secrets_list answered %+v, want the one name with a timestamp", list.Secrets)
	}
}

// aProcessReadsTheSecret is the middle clause: the unit declares the name, the
// Process is started by kitbashd with the member's current value, and the
// Package's own tool reads it out of its environment. Nothing of the member's
// own process ever held the value: kitbashd wrote the environment file.
func aProcessReadsTheSecret(t *testing.T, s *state) {
	// The mounts half left the manifest naming an illegal folder, so the mount
	// is written back with the secret declared beside it.
	rewriteReader(t, s, s.notesPath, notesMount, "ro", secretBlock)

	// The reader is still running from the mounts half, and secrets_set
	// restarts nothing: a Process takes a new environment when its owner runs
	// it again, which is proc_stop followed by proc_run, see PLAN.md 2.3. A
	// proc_run of a Process that is already running at the same digest
	// converges and changes nothing, which is what this stop is for.
	var listed struct {
		Processes []struct {
			ID      string `json:"id"`
			Package string `json:"package"`
		} `json:"processes"`
	}
	s.member.ok("proc_list", map[string]any{}, &listed)
	var running string
	for _, p := range listed.Processes {
		if p.Package == s.readerPath {
			running = p.ID
		}
	}
	if running == "" {
		t.Fatalf("proc_list holds no Process of %s: %+v", s.readerPath, listed.Processes)
	}
	s.member.ok("proc_stop", map[string]any{"id": running}, nil)

	var out struct {
		State string   `json:"state"`
		Tools []string `json:"tools"`
	}
	s.member.ok("proc_run", map[string]any{"package": s.readerPath}, &out)
	if out.State != "running" {
		t.Fatalf("proc_run answered %+v, want a running Process", out)
	}
	if !hasTool(out.Tools, readerEnv) {
		t.Fatalf("proc_run published %v, want %s among them", out.Tools, readerEnv)
	}

	var env struct {
		Value string `json:"value"`
	}
	s.member.ok(readerEnv, map[string]any{"name": secretName}, &env)
	if env.Value != s.secret {
		t.Fatalf("%s answered %q, want the value the member set", readerEnv, env.Value)
	}
	// What the member declared is names, so nothing but the name is readable
	// through the Package: pkg_inspect shows the manifest as it is written.
	var inspected struct {
		Manifest map[string]any `json:"manifest"`
	}
	s.member.ok("pkg_inspect", map[string]any{"path": s.readerPath}, &inspected)
	rendered := fmt.Sprint(inspected.Manifest)
	if !strings.Contains(rendered, secretName) {
		t.Errorf("pkg_inspect does not show the declared secret name: %s", truncate(rendered))
	}
	if strings.Contains(rendered, s.secret) {
		t.Fatalf("pkg_inspect answered the value: %s", truncate(rendered))
	}
}

// aSecondMemberIsRefused is the last clause: the same name declared by a
// Package of the admin, who has never set it, is refused at proc_run with the
// secret named and the call that fixes it. The admin's Package is the echo
// fixture, already built, so what this reaches is the secret and not a build,
// and the run is refused before a container is made, so the echo Process the
// job has been using is untouched.
func aSecondMemberIsRefused(t *testing.T, s *state) {
	body, err := os.ReadFile(filepath.Join("fixtures", packageName, "kitbash.yaml"))
	if err != nil {
		t.Fatalf("reading the echo manifest: %v", err)
	}
	declared := strings.Replace(string(body), "      expose: mcp\n",
		"      expose: mcp\n      secrets: "+secretBlock+"\n", 1)
	if declared == string(body) {
		t.Fatal("the echo manifest gained no secrets block")
	}
	writeFile(t, s.admin, filepath.Join(s.pkgPath, "kitbash.yaml"), declared)

	res := s.admin.call("proc_run", map[string]any{"package": s.pkgPath, "name": "needs-a-secret"})
	prob := res.mustProblem(t, "proc_run", "not-found")
	if !strings.Contains(prob.Detail, secretName) {
		t.Fatalf("the refusal is %q, want the secret named", prob.Detail)
	}
	if !strings.Contains(prob.Fix, "secrets_set "+secretName) {
		t.Fatalf("the fix is %q, want the call that sets it", prob.Fix)
	}
	if strings.Contains(prob.Detail+prob.Fix, s.secret) {
		t.Fatalf("the refusal quotes a value: %+v", prob)
	}

	// A secret is the member's own: the admin holding the same name with
	// another value changes nothing about the member's Process, and neither
	// member reads the other's, see PLAN.md section 2.3.
	s.admin.ok("secrets_set", map[string]any{"name": secretName, "value": "sk-the-admins-own"}, nil)
	var list struct {
		Secrets []struct {
			Name string `json:"name"`
		} `json:"secrets"`
	}
	s.admin.ok("secrets_list", map[string]any{}, &list)
	if len(list.Secrets) != 1 || list.Secrets[0].Name != secretName {
		t.Fatalf("the admin's listing is %+v, want their own one name", list.Secrets)
	}
	var env struct {
		Value string `json:"value"`
	}
	s.member.ok(readerEnv, map[string]any{"name": secretName}, &env)
	if env.Value != s.secret {
		t.Fatalf("the member's Process reads %q, want their own value", env.Value)
	}
}

// theSpanCarriesNoValue is the sentence the whole design rests on: the value
// crossed the wire once, and Telemetry recorded the call and not the
// credential, see PLAN.md section 2.3.
func theSpanCarriesNoValue(t *testing.T, s *state) {
	// Telemetry is flushed when a session exits, so the session that set the
	// secret is closed before its records are read.
	s.member.close()
	s.member = dial(t, memberName())

	res := s.member.call("tel_query", map[string]any{
		"signal": "traces",
		"tool":   "secrets_set",
	})
	res.mustSucceed(t, "tel_query")
	if strings.Contains(res.text(), s.secret) {
		t.Fatalf("a record of secrets_set carries the value: %s", truncate(res.text()))
	}
	var traces struct {
		Records []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Attributes struct {
				User string `json:"user"`
				Tool string `json:"tool"`
			} `json:"attributes"`
		} `json:"records"`
	}
	s.member.ok("tel_query", map[string]any{"signal": "traces", "tool": "secrets_set"}, &traces)
	if len(traces.Records) == 0 {
		t.Fatal("tel_query answered no secrets_set span")
	}
	for _, record := range traces.Records {
		if record.Name != "secrets_set" || record.Attributes.Tool != "secrets_set" {
			t.Fatalf("a span answered for secrets_set is %+v", record)
		}
		if record.Attributes.User != memberName() {
			t.Fatalf("a span of secrets_set is attributed to %s", record.Attributes.User)
		}
	}
}

// theSecretIsRemoved closes the loop: secrets_remove takes the name away and
// repeating it answers removed false rather than failing.
func theSecretIsRemoved(t *testing.T, s *state) {
	var removed struct {
		Name    string `json:"name"`
		Removed bool   `json:"removed"`
	}
	s.member.ok("secrets_remove", map[string]any{"name": secretName}, &removed)
	if !removed.Removed {
		t.Fatalf("secrets_remove answered %+v, want it removed", removed)
	}
	s.member.ok("secrets_remove", map[string]any{"name": secretName}, &removed)
	if removed.Removed {
		t.Fatalf("the second secrets_remove answered %+v, want removed false", removed)
	}
	var list struct {
		Secrets []struct {
			Name string `json:"name"`
		} `json:"secrets"`
	}
	s.member.ok("secrets_list", map[string]any{}, &list)
	if len(list.Secrets) != 0 {
		t.Fatalf("secrets_list answered %+v after the removal, want nothing", list.Secrets)
	}
	// The Process that is running keeps the value it was started with, and its
	// next start is the one that is refused, see PLAN.md section 2.3.
	res := s.member.call("proc_run", map[string]any{"package": s.readerPath, "name": "again"})
	prob := res.mustProblem(t, "proc_run", "not-found")
	if !strings.Contains(prob.Detail, secretName) {
		t.Fatalf("the refusal is %q, want the secret named", prob.Detail)
	}
}
