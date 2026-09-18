package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/secrets"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// The value every test here sets. It is one string so an assertion that a
// value did not reach a listing, a log line or a registration can look for it
// by name.
const secretValue = "sk-the-value-that-must-not-travel"

// setSecret calls secrets_set for the caller, which is the peer and the only
// member this API takes calls about.
func (h *harness) setSecret(name, value string) (*http.Response, []byte) {
	h.t.Helper()
	return h.postJSON(http.MethodPut, secretsPath+"/"+name, secretRequest{Value: value})
}

// seedSecret writes one value straight into the tree, standing in for a member
// other than the caller: the API takes no member name, so this is the only way
// to give one to somebody else.
func (h *harness) seedSecret(member, name, value string) {
	h.t.Helper()
	if _, err := secrets.New(h.secretsDir).Set(member, name, value); err != nil {
		h.t.Fatalf("seeding the secret of %s: %v", member, err)
	}
}

// TestSecretsSetListRemove is the first half of the M9 acceptance sentence: a
// member sets a value, the listing shows the name and no value, and a removal
// takes it away.
func TestSecretsSetListRemove(t *testing.T) {
	h := serve(t, false)

	res, body := h.setSecret("ANTHROPIC_API_KEY", secretValue)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("set = %d %s, want 200", res.StatusCode, body)
	}
	var set secretResponse
	if err := json.Unmarshal(body, &set); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if set.Name != "ANTHROPIC_API_KEY" {
		t.Errorf("set answered %+v, want the name", set)
	}
	if _, err := time.Parse(timeLayout, set.Updated); err != nil {
		t.Errorf("updated is %q, want an RFC 3339 timestamp: %v", set.Updated, err)
	}
	if strings.Contains(string(body), secretValue) {
		t.Errorf("the answer of secrets_set is %s, want no value in it", body)
	}

	res, body = h.do(http.MethodGet, secretsPath, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list = %d %s, want 200", res.StatusCode, body)
	}
	if strings.Contains(string(body), secretValue) {
		t.Fatalf("the listing is %s, want the name and no value", body)
	}
	var list secretList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(list.Secrets) != 1 || list.Secrets[0].Name != "ANTHROPIC_API_KEY" || list.Secrets[0].Updated == "" {
		t.Fatalf("the listing is %+v, want the one name with a timestamp", list.Secrets)
	}

	res, body = h.do(http.MethodDelete, secretsPath+"/ANTHROPIC_API_KEY", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("remove = %d %s, want 200", res.StatusCode, body)
	}
	var removed removedSecret
	if err := json.Unmarshal(body, &removed); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if !removed.Removed {
		t.Errorf("remove answered %+v, want it removed", removed)
	}
	// Removing it again answers false rather than failing, which is what makes
	// the call safe to repeat, see PLAN.md section 2.6.
	res, body = h.do(http.MethodDelete, secretsPath+"/ANTHROPIC_API_KEY", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("second remove = %d %s, want 200", res.StatusCode, body)
	}
	if err := json.Unmarshal(body, &removed); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if removed.Removed {
		t.Errorf("the second remove answered %+v, want removed false", removed)
	}
}

// Every refusal secrets_set can earn, each its own guard, and none of them
// quoting the value.
func TestSecretsSetRefusals(t *testing.T) {
	long := strings.Repeat("x", secrets.MaxValueBytes+1)
	cases := map[string]struct {
		name  string
		value string
	}{
		"a lower case name":         {"anthropic_api_key", secretValue},
		"a name with a dash":        {"ANTHROPIC-API-KEY", secretValue},
		"a name over 64 characters": {"A" + strings.Repeat("B", 64), secretValue},
		"an empty value":            {"KEY", ""},
		"a value over the size":     {"KEY", long},
		"a value with a NUL":        {"KEY", "sk-\x00-key"},
		"a value with a line break": {"KEY", "sk-first\nsk-second"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := serve(t, false)
			res, body := h.setSecret(c.name, c.value)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body %s", res.StatusCode, body)
			}
			if slug := h.problemOf(res, body).Slug(); slug != problem.SlugBadRequest {
				t.Errorf("slug = %q, want bad-request", slug)
			}
			if c.value != "" && strings.Contains(string(body), c.value) {
				t.Errorf("the refusal is %s, want one that does not quote the value", body)
			}
			// Nothing was written, so the listing is still empty.
			_, body = h.do(http.MethodGet, secretsPath, "", nil)
			var list secretList
			if err := json.Unmarshal(body, &list); err != nil {
				t.Fatalf("body %q: %v", body, err)
			}
			if len(list.Secrets) != 0 {
				t.Errorf("the listing is %+v after a refusal, want nothing", list.Secrets)
			}
		})
	}
}

// The API takes no member name, so there is no way to ask about another
// member: a path that names one is not a path this daemon serves, and a
// listing answers the peer's own names whatever else is in the tree.
func TestOneMemberReadsNoneOfAnothersSecrets(t *testing.T) {
	h := serve(t, false)
	h.seedSecret("bob", "ANTHROPIC_API_KEY", "bob-"+secretValue)

	_, body := h.do(http.MethodGet, secretsPath, "", nil)
	if strings.Contains(string(body), "bob-") {
		t.Fatalf("the caller's listing is %s, want none of another member's", body)
	}
	var list secretList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(list.Secrets) != 0 {
		t.Errorf("the caller's listing is %+v, want their own names only", list.Secrets)
	}

	// A path that names the member whose secret it is, is not a path here.
	for _, path := range []string{
		secretsPath + "/bob/ANTHROPIC_API_KEY",
		secretsPath + "/bob/",
	} {
		res, body := h.do(http.MethodGet, path, "", nil)
		if res.StatusCode == http.StatusOK {
			t.Errorf("%s answered 200 %s, want a refusal", path, body)
		}
	}

	// The caller's own set is theirs: setting the same name writes their file
	// and leaves the other member's alone.
	if res, body := h.setSecret("ANTHROPIC_API_KEY", secretValue); res.StatusCode != http.StatusOK {
		t.Fatalf("set = %d %s, want 200", res.StatusCode, body)
	}
	value, found, err := secrets.New(h.secretsDir).Get("bob", "ANTHROPIC_API_KEY")
	if err != nil || !found || value != "bob-"+secretValue {
		t.Errorf("bob holds %q (found %t, err %v), want the value they had", value, found, err)
	}
}

// An admin is a member here like any other: kitbash-admin is what reads every
// member's Telemetry and creates accounts, and it reads nobody's secrets, see
// PLAN.md section 2.3.
func TestAnAdminReadsNoOtherMembersSecrets(t *testing.T) {
	h := serve(t, true)
	h.seedSecret("bob", "ANTHROPIC_API_KEY", "bob-"+secretValue)

	res, body := h.do(http.MethodGet, secretsPath, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list = %d %s, want 200", res.StatusCode, body)
	}
	if strings.Contains(string(body), "bob-") {
		t.Fatalf("the admin's listing is %s, want none of another member's", body)
	}
	var list secretList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(list.Secrets) != 0 {
		t.Errorf("the admin's listing is %+v, want their own names only", list.Secrets)
	}
	res, _ = h.do(http.MethodDelete, secretsPath+"/bob/ANTHROPIC_API_KEY", "", nil)
	if res.StatusCode == http.StatusOK {
		t.Error("an admin removed another member's secret by naming them in the path")
	}
	if _, found, _ := secrets.New(h.secretsDir).Get("bob", "ANTHROPIC_API_KEY"); !found {
		t.Error("another member's secret is gone after an admin called DELETE")
	}
}

// A registration carries names and never values, and a name no unit may
// declare is refused rather than stored as a Process that fails every start.
func TestRegistrationCarriesTheSecretNames(t *testing.T) {
	h := serve(t, false)

	req := registration("")
	req.Secrets = []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"}
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register = %d %s, want 200", res.StatusCode, body)
	}
	p, found, err := h.store.Process(context.Background(), req.ID)
	if err != nil || !found {
		t.Fatalf("the Process was not stored (%t, %v)", found, err)
	}
	if len(p.Secrets) != 2 || p.Secrets[0] != "ANTHROPIC_API_KEY" || p.Secrets[1] != "OPENAI_API_KEY" {
		t.Errorf("the registration holds %v, want the two declared names", p.Secrets)
	}

	res, body := h.do(http.MethodGet, processesPath, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list = %d %s, want 200", res.StatusCode, body)
	}
	var list processList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(list.Processes) != 1 || len(list.Processes[0].Secrets) != 2 {
		t.Errorf("processes_list answered %+v, want the declared names", list.Processes)
	}
}

// The names of a registration are held to the same rule the manifest is, so a
// session that sent something else is refused: kitbash-mcp runs as the member,
// so what it claims about a unit is a claim.
func TestRegistrationSecretRefusals(t *testing.T) {
	cases := map[string][]string{
		"a lower case name":          {"anthropic_api_key"},
		"a name kitbashd speaks for": {EnvTelemetryToken},
		"another KITBASH_ name":      {"KITBASH_ANYTHING"},
		"the same name twice":        {"KEY", "KEY"},
		"more than sixteen names":    secretNames(17),
	}
	for name, declared := range cases {
		t.Run(name, func(t *testing.T) {
			h := serve(t, false)
			req := registration("")
			req.Secrets = declared
			_, res, body := h.register(req)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body %s", res.StatusCode, body)
			}
			if slug := h.problemOf(res, body).Slug(); slug != problem.SlugBadRequest {
				t.Errorf("slug = %q, want bad-request", slug)
			}
			if _, found, err := h.store.Process(context.Background(), req.ID); err != nil || found {
				t.Errorf("the refused registration was stored (%t, %v)", found, err)
			}
		})
	}
	// Sixteen legal names is a registration kitbashd takes.
	h := serve(t, false)
	req := registration("")
	req.Secrets = secretNames(16)
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("sixteen names = %d %s, want 200", res.StatusCode, body)
	}
}

// TestStartWritesTheSecretIntoTheEnvironmentFile is the middle of the M9
// acceptance sentence: the Process that declares a name is given the member's
// current value, in the same root owned file the Telemetry token is written
// into, and nowhere else.
func TestStartWritesTheSecretIntoTheEnvironmentFile(t *testing.T) {
	h, fake := supervised(t)
	h.seedSecret(h.user, "ANTHROPIC_API_KEY", secretValue)
	id := h.superviseWithSecrets(h.user, "kitbash-echo-echo", "ANTHROPIC_API_KEY")

	res, body := h.start(id, startRequest{Image: testDigest, Env: map[string]string{"LOG_LEVEL": "debug"}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("start = %d %s, want 200", res.StatusCode, body)
	}
	run := fake.Runs()[0]
	env := parseEnvFile(run.Env)
	if env["ANTHROPIC_API_KEY"] != secretValue {
		t.Fatalf("the environment carries %q for the declared secret, want the value the member set",
			env["ANTHROPIC_API_KEY"])
	}
	if env["LOG_LEVEL"] != "debug" {
		t.Errorf("the environment is %v, want the unit's own entries beside the secret", env)
	}
	// The value is in the file and on no command line: podman is given the
	// file, see PLAN.md section 2.3.
	for _, arg := range run.Args {
		if strings.Contains(arg, secretValue) {
			t.Fatalf("the command line carries the value: %v", run.Args)
		}
	}
	// A rotation is one call: the next start reads the value that is there
	// now, and nothing was restarted in between.
	if _, err := secrets.New(h.secretsDir).Set(h.user, "ANTHROPIC_API_KEY", "sk-rotated"); err != nil {
		t.Fatalf("rotating: %v", err)
	}
	if res, body := h.start(id, startRequest{Image: testDigest}); res.StatusCode != http.StatusOK {
		t.Fatalf("second start = %d %s, want 200", res.StatusCode, body)
	}
	runs := fake.Runs()
	if env := parseEnvFile(runs[len(runs)-1].Env); env["ANTHROPIC_API_KEY"] != "sk-rotated" {
		t.Errorf("the second start carries %q, want the rotated value", env["ANTHROPIC_API_KEY"])
	}
}

// The last clause of the M9 acceptance sentence: a Process whose owner has not
// set a declared name is refused, naming the secret and the call that fixes
// it, and nothing is created.
func TestStartRefusesAProcessWhoseSecretIsNotSet(t *testing.T) {
	h, fake := supervised(t)
	id := h.superviseWithSecrets(h.user, "kitbash-echo-echo", "ANTHROPIC_API_KEY")

	res, body := h.start(id, startRequest{Image: testDigest})
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("start = %d %s, want 404", res.StatusCode, body)
	}
	prob := h.problemOf(res, body)
	if prob.Slug() != problem.SlugNotFound {
		t.Errorf("slug = %q, want not-found", prob.Slug())
	}
	if !strings.Contains(prob.Detail, "ANTHROPIC_API_KEY") {
		t.Errorf("detail is %q, want the secret named", prob.Detail)
	}
	if !strings.Contains(prob.Fix, "secrets_set ANTHROPIC_API_KEY") {
		t.Errorf("fix is %q, want the call that sets it", prob.Fix)
	}
	if len(fake.Runs()) != 0 {
		t.Errorf("the runtime was asked for %+v, want nothing created", fake.Runs())
	}
	if _, err := os.Stat(filepath.Join(h.envDir, id)); !os.IsNotExist(err) {
		t.Errorf("an environment file was written for a start that was refused: %v", err)
	}

	// Setting it is all it takes: the names travel with the registration and
	// the values do not, so nothing is registered again.
	h.seedSecret(h.user, "ANTHROPIC_API_KEY", secretValue)
	if res, body := h.start(id, startRequest{Image: testDigest}); res.StatusCode != http.StatusOK {
		t.Fatalf("start after the secret was set = %d %s, want 200", res.StatusCode, body)
	}
}

// A restore of a Process whose secret is gone does not start it, and its owner
// reads why through proc_list: the same shape a mount that stopped being legal
// takes, see mounts.go.
func TestRestoreReportsAProcessWhoseSecretIsGone(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: "alice", UID: 1005})
	id := h.registeredWithSecrets("alice", "kitbash-echo-echo", "ANTHROPIC_API_KEY")

	counts := h.server.Restore(context.Background())
	if counts.Failed != 1 || counts.Started != 0 {
		t.Fatalf("counts = %+v, want the one Process failed and nothing started", counts)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("the runtime was asked for %+v, want nothing started", fake.Calls())
	}

	res, body := h.do(http.MethodGet, processesPath, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list = %d %s, want 200", res.StatusCode, body)
	}
	var list processList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	var reported *listedProcess
	for i := range list.Processes {
		if list.Processes[i].ID == id {
			reported = &list.Processes[i]
		}
	}
	if reported == nil {
		t.Fatalf("processes_list has no %s: %+v", id, list.Processes)
	}
	if !strings.Contains(reported.Problem, "ANTHROPIC_API_KEY") {
		t.Errorf("the problem is %q, want the secret named", reported.Problem)
	}
	if !strings.Contains(reported.Fix, "secrets_set") {
		t.Errorf("the fix is %q, want the call that sets it", reported.Fix)
	}
}

// The environment of a start carries the secret and never a KITBASH_ name a
// registration should not have held: the ownedEnv removal runs after the
// secrets are merged, so a registration written before the rule existed cannot
// forge the Process's own credentials.
func TestSecretsNeverOverruleWhatKitbashdSpeaksFor(t *testing.T) {
	h, fake := supervised(t)
	p := store.Process{
		ID: "forged", Owner: h.user, Container: "kitbash-echo-echo",
		FanoutSecret: "the-secret",
		Secrets:      []string{EnvFanoutSecret},
	}
	env := h.server.environment(p, nil, "the-token", map[string]string{EnvFanoutSecret: "forged"})
	if env[EnvFanoutSecret] != "the-secret" {
		t.Errorf("the fan out secret is %q, want the registration's", env[EnvFanoutSecret])
	}
	if env[EnvTelemetryToken] != "the-token" {
		t.Errorf("the token is %q, want the one kitbashd minted", env[EnvTelemetryToken])
	}
	_ = fake
}

// Removing a member takes their secrets with the account: the values are
// theirs, and a directory left behind would hand them to the next account
// created with that name, see PLAN.md section 2.3.
func TestUsersRemoveTakesTheMembersSecrets(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: "alice", UID: 1005})
	h.seedSecret("alice", "ANTHROPIC_API_KEY", "alice-"+secretValue)
	h.seedSecret("bob", "ANTHROPIC_API_KEY", "bob-"+secretValue)

	res, body := h.do(http.MethodDelete, usersPath+"/alice", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("remove = %d %s, want 200", res.StatusCode, body)
	}
	if _, err := os.Stat(filepath.Join(h.secretsDir, "alice")); !os.IsNotExist(err) {
		t.Errorf("the secrets of a removed member are still there: %v", err)
	}
	// Only theirs. Another member's set is not collateral.
	if _, found, err := secrets.New(h.secretsDir).Get("bob", "ANTHROPIC_API_KEY"); err != nil || !found {
		t.Errorf("another member's secret went with the removal (%t, %v)", found, err)
	}
}

// superviseWithSecrets registers one Process of the caller that declares the
// names given, the way proc_run would.
func (h *harness) superviseWithSecrets(owner, container string, declared ...string) string {
	h.t.Helper()
	id := h.supervise(owner, container)
	p, found, err := h.store.Process(context.Background(), id)
	if err != nil || !found {
		h.t.Fatalf("reading back the registration (%t, %v)", found, err)
	}
	p.Secrets = declared
	_, hash, err := store.NewToken()
	if err != nil {
		h.t.Fatalf("NewToken: %v", err)
	}
	if err := h.store.RegisterProcess(context.Background(), p, hash, 0); err != nil {
		h.t.Fatalf("RegisterProcess: %v", err)
	}
	return id
}

// registeredWithSecrets is registered with the names that Process declares,
// which is what restore resolves before it starts anything.
func (h *harness) registeredWithSecrets(owner, container string, declared ...string) string {
	h.t.Helper()
	id := h.registered(owner, container)
	p, found, err := h.store.Process(context.Background(), id)
	if err != nil || !found {
		h.t.Fatalf("reading back the registration (%t, %v)", found, err)
	}
	p.Secrets = declared
	_, hash, err := store.NewToken()
	if err != nil {
		h.t.Fatalf("NewToken: %v", err)
	}
	if err := h.store.RegisterProcess(context.Background(), p, hash, 0); err != nil {
		h.t.Fatalf("RegisterProcess: %v", err)
	}
	return id
}

// secretNames is a list of n legal names, for the counting guards.
func secretNames(n int) []string {
	names := make([]string, 0, n)
	for i := range n {
		names = append(names, "KEY_"+string(rune('A'+i)))
	}
	return names
}
