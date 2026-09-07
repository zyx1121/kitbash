package telemetry_test

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
	"github.com/zyx1121/kitbash/internal/telemetry/teltest"
)

func TestRegisterProcessSendsTheRegistrationAndReturnsTheToken(t *testing.T) {
	daemon := newDaemon(t)
	client := telemetry.NewClient(daemon.Socket)
	reg := telemetry.Registration{
		ID:            "0192f000-0000-7000-8000-000000000000",
		Package:       "/home/tester/observer",
		Name:          "observer",
		Expose:        "http",
		Endpoint:      "http://127.0.0.1:40275",
		Subscriptions: []string{"telemetry"},
	}

	token, prob := client.RegisterProcess(context.Background(), reg)
	if prob != nil {
		t.Fatalf("RegisterProcess: %s", prob.Detail)
	}
	if token == "" || token != daemon.Token(reg.ID) {
		t.Errorf("token is %q, want the one the daemon minted", token)
	}

	calls := daemon.Calls()
	if len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Path != telemetry.ProcessesPath {
		t.Fatalf("calls are %+v, want one POST to %s", calls, telemetry.ProcessesPath)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(calls[0].Body), &sent); err != nil {
		t.Fatalf("the request body is not JSON: %v", err)
	}
	want := map[string]any{
		"id":            reg.ID,
		"package":       reg.Package,
		"name":          reg.Name,
		"expose":        reg.Expose,
		"endpoint":      reg.Endpoint,
		"subscriptions": []any{"telemetry"},
	}
	for k, v := range want {
		if got := sent[k]; !equalJSON(got, v) {
			t.Errorf("%s is %v, want %v", k, got, v)
		}
	}
}

// A refusal reaches the caller as the problem kitbashd sent, so a Process id
// that belongs to another member reads as a conflict rather than as an
// internal failure.
func TestARefusedRegistrationKeepsTheDaemonsProblem(t *testing.T) {
	daemon := newDaemon(t)
	daemon.AnswerProcesses(teltest.Problem(http.StatusConflict, "conflict", "Conflict",
		"a Process with this id belongs to another member", "Run it under a name of your own."))
	client := telemetry.NewClient(daemon.Socket)

	_, prob := client.RegisterProcess(context.Background(), telemetry.Registration{ID: "taken"})
	if prob == nil {
		t.Fatal("a refused registration was reported as a success")
	}
	if prob.Slug() != problem.SlugConflict {
		t.Errorf("problem is %s, want conflict", prob.Slug())
	}
}

// A Process kitbashd does not know is already unregistered, which is what a
// stop after a daemon restart looks like.
func TestUnregisterProcessTreatsNotFoundAsDone(t *testing.T) {
	daemon := newDaemon(t)
	client := telemetry.NewClient(daemon.Socket)
	ctx := context.Background()
	daemon.AddProcess(teltest.Registration{ID: "known", Package: "/home/tester/echo", Name: "echo"})

	if prob := client.UnregisterProcess(ctx, "known"); prob != nil {
		t.Fatalf("UnregisterProcess: %s", prob.Detail)
	}
	if _, held := daemon.Registration("known"); held {
		t.Error("the daemon still holds a Process that was unregistered")
	}
	if prob := client.UnregisterProcess(ctx, "known"); prob != nil {
		t.Fatalf("unregistering twice: %s", prob.Detail)
	}
}

func TestListProcessesReadsWhatTheDaemonHolds(t *testing.T) {
	daemon := newDaemon(t)
	client := telemetry.NewClient(daemon.Socket)
	daemon.AddProcess(teltest.Registration{
		ID: "one", Package: "/home/tester/echo", Name: "echo", Expose: "mcp",
	})
	daemon.AddProcess(teltest.Registration{
		ID: "two", Package: "/home/tester/observer", Name: "observer", Expose: "http",
		Endpoint: "http://127.0.0.1:40275", Subscriptions: []string{"telemetry"},
	})
	daemon.AddProcess(teltest.Registration{
		ID: "three", Package: "/home/other/echo", Name: "echo", Owner: "other", Admin: true,
	})

	list, prob := client.ListProcesses(context.Background())
	if prob != nil {
		t.Fatalf("ListProcesses: %s", prob.Detail)
	}
	if len(list) != 3 {
		t.Fatalf("the daemon holds %d Processes, want 3", len(list))
	}
	if list[0].ID != "one" || list[0].Package != "/home/tester/echo" || list[0].Expose != "mcp" {
		t.Errorf("the first Process is %+v, want the echo Process", list[0])
	}
	if list[1].Endpoint != "http://127.0.0.1:40275" ||
		len(list[1].Subscriptions) != 1 || list[1].Subscriptions[0] != "telemetry" {
		t.Errorf("the second Process is %+v, want its endpoint and subscription", list[1])
	}
	// The member a Process belongs to is under owner, which is what tells an
	// admin's list apart from a member's own.
	if list[2].Owner != "other" || !list[2].Admin {
		t.Errorf("the third Process is %+v, want it owned by other and marked admin", list[2])
	}
}

// kitbashd names the member under owner. The older spelling is still read, so
// a daemon that has not caught up does not silently become anonymous, which
// would make every Process look like the caller's.
func TestAListedProcessAcceptsBothSpellingsOfItsOwner(t *testing.T) {
	cases := map[string]string{
		"owner": `{"id":"one","package":"/home/other/echo","name":"echo","owner":"other"}`,
		"user":  `{"id":"one","package":"/home/other/echo","name":"echo","user":"other"}`,
		"both":  `{"id":"one","package":"/home/other/echo","name":"echo","owner":"other","user":"ignored"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			var got telemetry.Registered
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if got.Owner != "other" {
				t.Errorf("owner is %q, want other", got.Owner)
			}
			if got.ID != "one" || got.Package != "/home/other/echo" {
				t.Errorf("the Process is %+v, want its id and Package kept", got)
			}
		})
	}
}

// A host without kitbashd cannot mint a token. The caller is told so with the
// fix only an operator can act on, and starts the Process untraced.
func TestRegisteringWithoutADaemonIsAProblemWithAFix(t *testing.T) {
	client := telemetry.NewClient(filepath.Join(t.TempDir(), "absent.sock"))

	_, prob := client.RegisterProcess(context.Background(), telemetry.Registration{ID: "orphan"})
	if prob == nil {
		t.Fatal("registering against a socket that is not there succeeded")
	}
	if prob.Slug() != problem.SlugInternal {
		t.Errorf("problem is %s, want internal", prob.Slug())
	}
	if !strings.Contains(prob.Fix, "kitbashd is not running") {
		t.Errorf("fix is %q, want the one only an operator can act on", prob.Fix)
	}
}

// The endpoint Processes export to is the host address containers reach, and
// the override for it follows the same rule as KITBASH_SOCKET.
func TestTheProcessEndpointIsOverridableOnlyOutsideAnSSHSession(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv(telemetry.ProcessEndpointEnv, "http://127.0.0.1:4318")
	if got := telemetry.EndpointForProcesses(); got != "http://127.0.0.1:4318" {
		t.Errorf("outside SSH the endpoint is %q, want the override", got)
	}
	t.Setenv("SSH_CONNECTION", "10.0.0.1 52000 10.0.0.2 22")
	if got := telemetry.EndpointForProcesses(); got != telemetry.ProcessEndpoint {
		t.Errorf("inside SSH the endpoint is %q, want %s", got, telemetry.ProcessEndpoint)
	}
}

// equalJSON compares two decoded JSON values.
func equalJSON(got, want any) bool {
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	return string(a) == string(b)
}
