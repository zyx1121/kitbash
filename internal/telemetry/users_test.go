package telemetry_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
	"github.com/zyx1121/kitbash/internal/telemetry/teltest"
)

func TestUsersCallsReachTheDaemonAndComeBackVerbatim(t *testing.T) {
	daemon := newDaemon(t)
	daemon.SetAdmin(true)
	daemon.AddMember(teltest.Member{User: "loki", UID: 1001, Admin: true, Keys: 1})
	client := telemetry.NewClient(daemon.Socket)
	ctx := context.Background()

	created, prob := client.CreateUser(ctx, json.RawMessage(
		`{"name":"member","sshKey":"ssh-ed25519 AAAA member@laptop","admin":false}`))
	if prob != nil {
		t.Fatalf("CreateUser: %s", prob.Detail)
	}
	var member map[string]any
	if err := json.Unmarshal(created, &member); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if member["user"] != "member" {
		t.Errorf("users_create answered %v, want the member it created", member)
	}

	listed, prob := client.ListUsers(ctx)
	if prob != nil {
		t.Fatalf("ListUsers: %s", prob.Detail)
	}
	var list struct {
		Users []teltest.Member `json:"users"`
	}
	if err := json.Unmarshal(listed, &list); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if len(list.Users) != 2 {
		t.Errorf("users_list answered %d members, want the two the daemon holds", len(list.Users))
	}

	keys, prob := client.AddKey(ctx, "member", "ssh-ed25519 BBBB member@desktop")
	if prob != nil {
		t.Fatalf("AddKey: %s", prob.Detail)
	}
	var added map[string]any
	json.Unmarshal(keys, &added)
	if added["keys"] != float64(2) {
		t.Errorf("users_add_key answered %v, want two keys", added)
	}

	removed, prob := client.RemoveUser(ctx, "member")
	if prob != nil {
		t.Fatalf("RemoveUser: %s", prob.Detail)
	}
	var removal map[string]any
	json.Unmarshal(removed, &removal)
	// The daemon answers 202 and removes the member on its own time, so what
	// comes back is the state and not the archive path, see
	// internal/daemon/removal.go.
	if removal["user"] != "member" || removal["state"] != "removing" {
		t.Errorf("users_remove answered %v, want the member removing", removal)
	}

	// The bodies above went over the socket rather than being answered here.
	paths := map[string]bool{}
	methods := map[string]string{}
	for _, call := range daemon.Calls() {
		paths[call.Path] = true
		methods[call.Path+" "+call.Method] = call.Body
	}
	for _, want := range []string{
		telemetry.UsersPath,
		telemetry.UsersPath + "/member/keys",
		telemetry.UsersPath + "/member",
	} {
		if !paths[want] {
			t.Errorf("no call reached %s; the calls were %+v", want, daemon.Calls())
		}
	}
}

func TestMeAnswersWhoTheCallerIs(t *testing.T) {
	daemon := newDaemon(t)
	daemon.SetIdentity(teltest.Identity{User: "loki", UID: 1001, Admin: true,
		Groups: []string{"kitbash-users", "kitbash-admin"}})
	client := telemetry.NewClient(daemon.Socket)

	body, prob := client.Me(context.Background())
	if prob != nil {
		t.Fatalf("Me: %s", prob.Detail)
	}
	var identity telemetry.Identity
	if err := json.Unmarshal(body, &identity); err != nil {
		t.Fatalf("the answer is not an identity: %v", err)
	}
	if identity.User != "loki" || !identity.Admin || identity.UID != 1001 {
		t.Errorf("users_me answered %+v, want the caller kitbashd knows", identity)
	}
}

// A refusal by kitbashd reaches the agent as the problem the daemon sent, slug
// and status and fix and all: the surface adds nothing to it.
func TestUsersPassTheDaemonsProblemThrough(t *testing.T) {
	daemon := newDaemon(t)
	client := telemetry.NewClient(daemon.Socket)

	_, prob := client.ListUsers(context.Background())
	if prob == nil {
		t.Fatal("a member listing every user was allowed")
	}
	if prob.Slug() != problem.SlugNotPermitted {
		t.Errorf("problem is %s, want not-permitted", prob.Slug())
	}
	if prob.Status != http.StatusForbidden {
		t.Errorf("status is %d, want 403", prob.Status)
	}
	if prob.Fix != "Ask an admin to run it." {
		t.Errorf("fix is %q, want the daemon's own", prob.Fix)
	}
}

// The identity is asked for once and kept, because it cannot change inside one
// SSH session.
func TestAdminAsksTheDaemonOncePerSession(t *testing.T) {
	daemon := newDaemon(t)
	daemon.SetAdmin(true)
	client := telemetry.NewClient(daemon.Socket)
	ctx := context.Background()

	for range 3 {
		admin, known := client.Admin(ctx)
		if !admin || !known {
			t.Fatal("the caller is an admin and was not reported as one")
		}
	}
	if calls := meCalls(daemon); calls != 1 {
		t.Errorf("users_me was called %d times, want once", calls)
	}
}

// A daemon that cannot be reached makes the caller neither an admin nor a
// member: there is nothing to queue with, so the caller falls through to the
// kernel instead of being guessed about.
func TestAdminIsUnknownWithoutADaemon(t *testing.T) {
	client := telemetry.NewClient(t.TempDir() + "/absent.sock")

	admin, known := client.Admin(context.Background())
	if admin || known {
		t.Errorf("Admin answered (%v, %v), want it unknown", admin, known)
	}
}

// A failure is not an answer, so it is not kept: a daemon that was starting
// when the first call landed is asked again by the second.
func TestAFailedIdentityIsAskedAgain(t *testing.T) {
	daemon := newDaemon(t)
	daemon.AnswerUsers(teltest.Problem(http.StatusInternalServerError, "internal",
		"Internal error", "the store is not open yet", ""))
	client := telemetry.NewClient(daemon.Socket)
	ctx := context.Background()

	if _, known := client.Admin(ctx); known {
		t.Fatal("a refused users_me was taken for an answer")
	}
	daemon.AnswerUsers(teltest.Response{})
	daemon.SetAdmin(true)

	admin, known := client.Admin(ctx)
	if !admin || !known {
		t.Errorf("Admin answered (%v, %v), want the admin the daemon now names", admin, known)
	}
	if calls := meCalls(daemon); calls != 2 {
		t.Errorf("users_me was called %d times, want the failure and the answer", calls)
	}
}

// meCalls counts how often the identity was asked for.
func meCalls(daemon *teltest.Daemon) int {
	calls := 0
	for _, call := range daemon.Calls() {
		if call.Path == telemetry.UsersMePath {
			calls++
		}
	}
	return calls
}
