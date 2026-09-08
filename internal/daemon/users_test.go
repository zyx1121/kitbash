package daemon

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"os/user"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
	"github.com/zyx1121/kitbash/internal/uuid"
)

// serveUsers is a daemon whose users family talks to a fake host. The peer is
// the test process, so the caller is whoever runs the tests; admin says
// whether kitbashd treats them as an administrator, and member whether the
// fake host knows them at all.
func serveUsers(t *testing.T, admin bool) (*harness, *sysusers.Fake) {
	t.Helper()
	fake := sysusers.NewFake()
	h := serveWith(t, Options{
		Admin:  func(*user.User) (bool, error) { return admin, nil },
		Users:  fake,
		Runner: fake,
	})
	fake.Add(sysusers.Member{Name: h.user, Admin: admin})
	return h, fake
}

// publicKey is one well formed OpenSSH key line, built rather than pasted so
// the test says what the format is. The comment tells two keys apart.
func publicKey(comment string) string {
	material := make([]byte, 0, 48)
	material = binary.BigEndian.AppendUint32(material, uint32(len("ssh-ed25519")))
	material = append(material, "ssh-ed25519"...)
	material = binary.BigEndian.AppendUint32(material, 32)
	for i := range 32 {
		material = append(material, byte(i)+byte(len(comment)))
	}
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(material) + " " + comment
}

func TestUsersCreate(t *testing.T) {
	h, fake := serveUsers(t, true)

	res, body := h.postJSON(http.MethodPost, usersPath, userRequest{
		Name:   "alice",
		SSHKey: publicKey("alice@laptop"),
		Admin:  true,
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	var got userResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if got.User != "alice" || got.UID == 0 || !got.Admin {
		t.Errorf("response = %+v, want alice with a uid and the admin flag", got)
	}
	if len(fake.Created) != 1 || fake.Created[0].Name != "alice" || !fake.Created[0].Admin {
		t.Fatalf("the host was asked for %+v, want one admin named alice", fake.Created)
	}
	if fake.Created[0].SSHKey != publicKey("alice@laptop") {
		t.Errorf("the host got the key %q, want the one that was sent", fake.Created[0].SSHKey)
	}

	// The same name twice is a conflict, not a second account.
	res, body = h.postJSON(http.MethodPost, usersPath, userRequest{Name: "alice", SSHKey: publicKey("again")})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("second create status = %d, body %s", res.StatusCode, body)
	}
	if slug := h.problemOf(res, body).Slug(); slug != problem.SlugConflict {
		t.Errorf("slug = %q, want conflict", slug)
	}
}

func TestUsersCreateRefusals(t *testing.T) {
	cases := map[string]struct {
		req    userRequest
		status int
		slug   string
	}{
		"a name the pattern refuses": {
			req:    userRequest{Name: "Alice", SSHKey: publicKey("a")},
			status: http.StatusBadRequest,
			slug:   problem.SlugBadRequest,
		},
		"a name that is a path": {
			req:    userRequest{Name: "../root", SSHKey: publicKey("a")},
			status: http.StatusBadRequest,
			slug:   problem.SlugBadRequest,
		},
		"a line that is not a key": {
			req:    userRequest{Name: "alice", SSHKey: `command="rm -rf /" ssh-ed25519 AAAA`},
			status: http.StatusBadRequest,
			slug:   problem.SlugBadRequest,
		},
		"no key at all": {
			req:    userRequest{Name: "alice"},
			status: http.StatusBadRequest,
			slug:   problem.SlugBadRequest,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h, fake := serveUsers(t, true)
			res, body := h.postJSON(http.MethodPost, usersPath, c.req)
			if res.StatusCode != c.status {
				t.Fatalf("status = %d, want %d, body %s", res.StatusCode, c.status, body)
			}
			if slug := h.problemOf(res, body).Slug(); slug != c.slug {
				t.Errorf("slug = %q, want %q", slug, c.slug)
			}
			if len(fake.Created) != 0 {
				t.Errorf("the host was asked for %+v, want nothing", fake.Created)
			}
		})
	}
}

// TestUsersAreAdminOnly is the rule the whole family rests on: creating a
// member is root's work and a member is not root, see PLAN.md section 4.5.
func TestUsersAreAdminOnly(t *testing.T) {
	h, fake := serveUsers(t, false)
	fake.Add(sysusers.Member{Name: "alice"})

	cases := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, usersPath, userRequest{Name: "bob", SSHKey: publicKey("b")}},
		{http.MethodGet, usersPath, nil},
		{http.MethodPost, usersPath + "/alice/keys", keyRequest{SSHKey: publicKey("b")}},
		{http.MethodDelete, usersPath + "/alice", nil},
	}
	for _, c := range cases {
		res, body := h.postJSON(c.method, c.path, c.body)
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s status = %d, want 403, body %s", c.method, c.path, res.StatusCode, body)
		}
		if slug := h.problemOf(res, body).Slug(); slug != problem.SlugNotPermitted {
			t.Errorf("%s %s slug = %q, want not-permitted", c.method, c.path, slug)
		}
	}
	if len(fake.Created) != 0 || len(fake.Removed) != 0 || len(fake.AddedKeys) != 0 {
		t.Errorf("the host was asked to do work for a member: %+v %v %+v",
			fake.Created, fake.Removed, fake.AddedKeys)
	}
}

func TestUsersList(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: "alice", UID: 1005, Admin: true})
	fake.Add(sysusers.Member{Name: "bob", UID: 1006})

	// One registered Process for alice, so the count is not always zero.
	_, hash, err := store.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if err := h.store.RegisterProcess(context.Background(), store.Process{
		ID:           uuid.V7(),
		Owner:        "alice",
		Package:      "/home/alice/echo",
		Name:         "echo",
		Container:    "kitbash-echo-echo",
		Expose:       ExposeNone,
		RegisteredAt: time.Now().UTC(),
	}, hash, 0); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}

	res, body := h.do(http.MethodGet, usersPath, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	var got userList
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	byName := map[string]memberResponse{}
	for _, m := range got.Users {
		byName[m.User] = m
	}
	alice, held := byName["alice"]
	if !held {
		t.Fatalf("users = %+v, want alice among them", got.Users)
	}
	if alice.UID != 1005 || !alice.Admin || alice.Processes != 1 || alice.Home != "/home/alice" {
		t.Errorf("alice = %+v, want uid 1005, admin, one Process and a home", alice)
	}
	if bob := byName["bob"]; bob.Processes != 0 || bob.Admin {
		t.Errorf("bob = %+v, want no Processes and no admin flag", bob)
	}
}

func TestUsersMe(t *testing.T) {
	h, _ := serveUsers(t, true)
	res, body := h.do(http.MethodGet, usersPath+"/me", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	var got meResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if got.User != h.user || !got.Admin || got.UID == 0 {
		t.Errorf("me = %+v, want the peer with a uid and the admin flag", got)
	}
	if len(got.Groups) == 0 {
		t.Errorf("groups = %v, want the groups the host lists", got.Groups)
	}
}

// TestUsersMeIsNotAdminOnly is the one path of this family a member may call.
func TestUsersMeIsNotAdminOnly(t *testing.T) {
	h, _ := serveUsers(t, false)
	res, body := h.do(http.MethodGet, usersPath+"/me", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	var got meResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if got.Admin {
		t.Errorf("me = %+v, want the admin flag false", got)
	}
}

func TestUsersAddKey(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: "alice"})

	res, body := h.postJSON(http.MethodPost, usersPath+"/alice/keys", keyRequest{SSHKey: publicKey("one")})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	var got keyResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if got.User != "alice" || got.Keys != 1 {
		t.Errorf("response = %+v, want alice with one key", got)
	}

	// The same key again is not a second key.
	_, body = h.postJSON(http.MethodPost, usersPath+"/alice/keys", keyRequest{SSHKey: publicKey("one")})
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if got.Keys != 1 {
		t.Errorf("keys = %d after the same key twice, want 1", got.Keys)
	}
	_, body = h.postJSON(http.MethodPost, usersPath+"/alice/keys", keyRequest{SSHKey: publicKey("two")})
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if got.Keys != 2 {
		t.Errorf("keys = %d after a second key, want 2", got.Keys)
	}

	res, body = h.postJSON(http.MethodPost, usersPath+"/carol/keys", keyRequest{SSHKey: publicKey("one")})
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status for a stranger = %d, body %s", res.StatusCode, body)
	}
}

func TestUsersRemove(t *testing.T) {
	h, fake := serveUsers(t, true)
	fake.Add(sysusers.Member{Name: "alice", UID: 1005})

	// A registration and an approval of alice's, both of which go with her.
	_, hash, err := store.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	id := uuid.V7()
	ctx := context.Background()
	if err := h.store.RegisterProcess(ctx, store.Process{
		ID: id, Owner: "alice", Package: "/home/alice/echo", Container: "kitbash-echo",
		Expose: ExposeNone, RegisteredAt: time.Now().UTC(),
	}, hash, 0); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}
	if err := h.store.CreateApproval(ctx, store.Approval{
		ID: uuid.V7(), Requester: "alice", Tool: store.ToolFSWrite,
		Input: json.RawMessage(`{"path":"/org/handbook/x.md"}`), RequestedAt: time.Now().UTC(),
	}, 0); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}

	res, body := h.do(http.MethodDelete, usersPath+"/alice", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	var got removeResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if got.User != "alice" || got.Archived != "/org/.archive/alice" {
		t.Errorf("response = %+v, want alice archived under /org/.archive", got)
	}
	if len(fake.Removed) != 1 || fake.Removed[0] != "alice" {
		t.Errorf("the host removed %v, want alice", fake.Removed)
	}

	if _, found, err := h.store.Process(ctx, id); err != nil || found {
		t.Errorf("the Process of a removed member is still registered (%t, %v)", found, err)
	}
	pending, err := h.store.Approvals(ctx, "alice", "")
	if err != nil {
		t.Fatalf("Approvals: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("approvals = %+v, want none left for a removed member", pending)
	}

	res, body = h.do(http.MethodDelete, usersPath+"/alice", "", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("second remove status = %d, body %s", res.StatusCode, body)
	}
}

// TestUsersRemoveRefusesSelf keeps an admin from locking themselves out of
// their own host with one call.
func TestUsersRemoveRefusesSelf(t *testing.T) {
	h, fake := serveUsers(t, true)
	res, body := h.do(http.MethodDelete, usersPath+"/"+h.user, "", nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body %s", res.StatusCode, body)
	}
	if slug := h.problemOf(res, body).Slug(); slug != problem.SlugConflict {
		t.Errorf("slug = %q, want conflict", slug)
	}
	if len(fake.Removed) != 0 {
		t.Errorf("the host removed %v, want nothing", fake.Removed)
	}
}

// TestUsersRemoveRefusesTheLastAdmin is the other half of the same rule: a
// host with no admin has no way back to one through this API.
func TestUsersRemoveRefusesTheLastAdmin(t *testing.T) {
	// The caller is an admin as far as kitbashd is concerned but is not a
	// member of this host, so alice is the only administrator the host has.
	fake := sysusers.NewFake()
	h := serveWith(t, Options{
		Admin:  func(*user.User) (bool, error) { return true, nil },
		Users:  fake,
		Runner: fake,
	})
	fake.Add(sysusers.Member{Name: "alice", Admin: true})
	fake.Add(sysusers.Member{Name: "bob"})

	res, body := h.do(http.MethodDelete, usersPath+"/alice", "", nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body %s", res.StatusCode, body)
	}
	if len(fake.Removed) != 0 {
		t.Errorf("the host removed %v, want nothing", fake.Removed)
	}

	// A second admin makes the first removable.
	fake.Add(sysusers.Member{Name: "carol", Admin: true})
	res, body = h.do(http.MethodDelete, usersPath+"/alice", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %s", res.StatusCode, body)
	}
	if len(fake.Removed) != 1 || fake.Removed[0] != "alice" {
		t.Errorf("the host removed %v, want alice", fake.Removed)
	}
}

// TestUsersPathsAndMethods checks the shape of the family: a verb a path does
// not carry answers problem details rather than the mux's plain text.
func TestUsersPathsAndMethods(t *testing.T) {
	h, _ := serveUsers(t, true)
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodDelete, usersPath},
		{http.MethodPut, usersPath + "/me"},
		{http.MethodGet, usersPath + "/alice/keys"},
		{http.MethodGet, usersPath + "/alice"},
		{http.MethodPost, usersPath + "/alice/keys/extra"},
	}
	for _, c := range cases {
		res, body := h.do(c.method, c.path, "", nil)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404, body %s", c.method, c.path, res.StatusCode, body)
		}
		if slug := h.problemOf(res, body).Slug(); slug != problem.SlugNotFound {
			t.Errorf("%s %s slug = %q, want not-found", c.method, c.path, slug)
		}
	}
}
