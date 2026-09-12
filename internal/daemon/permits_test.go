package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/problem"
)

// declaredPermits is what a kit that reaches back over /mcp asks for: the two
// tools it reads Files with, and the prefixes it may name.
func declaredPermits() manifest.Permits {
	return manifest.Permits{
		Tools: []string{"fs_read", "fs_list", "*_*"},
		Paths: []string{"/org", "/home/*"},
	}
}

// childEnvironment reads the environment one session's child recorded.
func childEnvironment(t *testing.T, call sessionCall) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(call.Dump)
	if err != nil {
		t.Fatalf("the child recorded no environment: %v", err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if key, value, found := strings.Cut(line, "="); found {
			got[key] = value
		}
	}
	return got
}

// TestMCPSessionCarriesThePermitsOfTheProcess is the hand off: the block the
// Process registered reaches its session child as KITBASH_PERMITS, which is
// the whole of what narrows that session, see mcp_for_processes in
// spec/kitbashd-api.yaml.
func TestMCPSessionCarriesThePermitsOfTheProcess(t *testing.T) {
	m := serveMCPDaemon(t, 0)

	req := registration("")
	req.Expose = ExposeMCP
	req.Permits = declaredPermits()
	token, res, body := m.register(req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("register: status %d, body %s", res.StatusCode, body)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "process", Version: "test"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   m.base + MCPPath,
		HTTPClient: m.client(token),
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	calls := m.sessions.recorded()
	if len(calls) != 1 {
		t.Fatalf("children started = %d, want one", len(calls))
	}
	got := childEnvironment(t, calls[0])[manifest.EnvPermits]
	if got != string(declaredPermits().JSON()) {
		t.Fatalf("%s = %s, want the registered block", manifest.EnvPermits, got)
	}
	// The child reads it back as the block the Package declared, which is
	// what its surface is narrowed to.
	permits, err := manifest.ParsePermits([]byte(got))
	if err != nil {
		t.Fatalf("the child could not read the block: %v", err)
	}
	if !permits.Match("fs_read", nil) || !permits.Allows("/home/tester/flows") || permits.Match("nounderscore", nil) {
		t.Errorf("the child was given %+v", permits)
	}
}

// TestMCPSessionOfAProcessWithoutPermitsCarriesTheEmptyBlock is the default a
// Process gets when its Package declared nothing: an empty block rather than
// no variable, so the child narrows to nothing instead of guessing.
func TestMCPSessionOfAProcessWithoutPermitsCarriesTheEmptyBlock(t *testing.T) {
	m := serveMCPDaemon(t, 0)
	session := m.connect(t, nil)
	defer session.Close()

	calls := m.sessions.recorded()
	if len(calls) != 1 {
		t.Fatalf("children started = %d, want one", len(calls))
	}
	got := childEnvironment(t, calls[0])[manifest.EnvPermits]
	if got != `{"tools":[],"paths":[]}` {
		t.Errorf("%s = %q, want the empty block", manifest.EnvPermits, got)
	}
}

// TestProcessListCarriesThePermits: what a Process may call is not a secret
// from its owner, so the registry lists it beside the rest of the record.
func TestProcessListCarriesThePermits(t *testing.T) {
	h := serve(t, false)

	req := registration("")
	req.Expose = ExposeMCP
	req.Permits = declaredPermits()
	if _, res, body := h.register(req); res.StatusCode != http.StatusOK {
		t.Fatalf("register: status %d, body %s", res.StatusCode, body)
	}

	res, body := h.do(http.MethodGet, processesPath, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, body %s", res.StatusCode, body)
	}
	var list processList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(list.Processes) != 1 {
		t.Fatalf("list = %+v, want one Process", list.Processes)
	}
	permits := list.Processes[0].Permits
	if strings.Join(permits.Tools, ",") != "fs_read,fs_list,*_*" ||
		strings.Join(permits.Paths, ",") != "/org,/home/*" {
		t.Errorf("the listed permits are %+v, want the registered block", permits)
	}
}

// TestRegistrationRefusesPermitsItCannotHonour: a glob kitbashd cannot read is
// answered now, with the manifest named, rather than stored as a rule that
// silently permits nothing.
func TestRegistrationRefusesPermitsItCannotHonour(t *testing.T) {
	h := serve(t, false)

	for _, permits := range []manifest.Permits{
		{Tools: []string{"fs read"}},
		{Paths: []string{"home/tester"}},
		{Paths: []string{"/home/te*"}},
	} {
		req := registration("")
		req.Expose = ExposeMCP
		req.Permits = permits
		_, res, body := h.register(req)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("register %+v: status %d, body %s", permits, res.StatusCode, body)
		}
		p := h.problemOf(res, body)
		if p.Slug() != problem.SlugBadRequest {
			t.Errorf("problem is %s, want bad-request", p.Slug())
		}
		if !strings.Contains(p.Fix, "provides.permits") {
			t.Errorf("fix is %q, which does not name the manifest block", p.Fix)
		}
	}
}
