package teltest

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Member is one member of the fake's user list, as users_list returns it.
type Member struct {
	User      string `json:"user"`
	UID       int    `json:"uid"`
	Admin     bool   `json:"admin"`
	Keys      int    `json:"keys"`
	Processes int    `json:"processes"`
}

// Identity is what users_me answers. It is the caller of every request, since
// one fake daemon serves one test session, and it is injected rather than read
// from peer credentials: a test decides whether its caller is an admin.
type Identity struct {
	User   string   `json:"user"`
	UID    int      `json:"uid"`
	Admin  bool     `json:"admin"`
	Groups []string `json:"groups"`
}

// SetIdentity replaces who the fake says the caller is.
func (d *Daemon) SetIdentity(identity Identity) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.identity = identity
}

// SetAdmin makes the caller an admin or a member, which is the one thing most
// tests need from the identity.
func (d *Daemon) SetAdmin(admin bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.identity.Admin = admin
	if admin {
		d.identity.Groups = []string{"kitbash-users", "kitbash-admin"}
	} else {
		d.identity.Groups = []string{"kitbash-users"}
	}
}

// AnswerUsers replaces what the users family returns, which is how a daemon
// that refuses a call is exercised. The zero Response restores the fake's own
// behaviour.
func (d *Daemon) AnswerUsers(r Response) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.users = r
}

// AddMember seeds the user list, standing in for a member created earlier.
func (d *Daemon) AddMember(member Member) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.members = append(d.members, member)
}

// Members is the user list the fake holds.
func (d *Daemon) Members() []Member {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Member(nil), d.members...)
}

// me answers users_me from the injected identity.
func (d *Daemon) me(w http.ResponseWriter, r *http.Request) {
	d.wait()
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path})
	if override, ok := d.usersOverride(); ok {
		d.mu.Unlock()
		write(w, override)
		return
	}
	body, _ := json.Marshal(d.identity)
	d.mu.Unlock()
	write(w, Response{Status: http.StatusOK, ContentType: "application/json", Body: string(body)})
}

// createUser answers users_create: admin only, a name already held is a
// conflict.
func (d *Daemon) createUser(w http.ResponseWriter, r *http.Request) {
	d.wait()
	body, err := readAll(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	if answer, ok := d.usersRefusal(); ok {
		d.mu.Unlock()
		write(w, answer)
		return
	}
	var in struct {
		Name   string `json:"name"`
		SSHKey string `json:"sshKey"`
		Admin  bool   `json:"admin"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.Name == "" || in.SSHKey == "" {
		d.mu.Unlock()
		write(w, Problem(http.StatusBadRequest, "bad-request", "Bad request",
			"the body is not a member", "Send a name and one SSH public key line."))
		return
	}
	if _, held := d.member(in.Name); held {
		d.mu.Unlock()
		write(w, Problem(http.StatusConflict, "conflict", "Conflict",
			fmt.Sprintf("%s is already a member", in.Name), ""))
		return
	}
	member := Member{User: in.Name, UID: 1000 + len(d.members) + 1, Admin: in.Admin, Keys: 1}
	d.members = append(d.members, member)
	d.mu.Unlock()
	answer, _ := json.Marshal(map[string]any{"user": member.User, "uid": member.UID, "admin": member.Admin})
	write(w, Response{Status: http.StatusOK, ContentType: "application/json", Body: string(answer)})
}

// listUsers answers users_list, admin only.
func (d *Daemon) listUsers(w http.ResponseWriter, r *http.Request) {
	d.wait()
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path})
	if answer, ok := d.usersRefusal(); ok {
		d.mu.Unlock()
		write(w, answer)
		return
	}
	list := append([]Member(nil), d.members...)
	d.mu.Unlock()
	answer, _ := json.Marshal(map[string]any{"users": list})
	write(w, Response{Status: http.StatusOK, ContentType: "application/json", Body: string(answer)})
}

// addKey answers users_add_key, admin only.
func (d *Daemon) addKey(w http.ResponseWriter, r *http.Request) {
	d.wait()
	name := r.PathValue("name")
	body, err := readAll(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	if answer, ok := d.usersRefusal(); ok {
		d.mu.Unlock()
		write(w, answer)
		return
	}
	index, held := d.member(name)
	if !held {
		d.mu.Unlock()
		write(w, Problem(http.StatusNotFound, "not-found", "Not found",
			fmt.Sprintf("%s is not a member", name), ""))
		return
	}
	d.members[index].Keys++
	keys := d.members[index].Keys
	d.mu.Unlock()
	answer, _ := json.Marshal(map[string]any{"user": name, "keys": keys})
	write(w, Response{Status: http.StatusOK, ContentType: "application/json", Body: string(answer)})
}

// removeUser answers users_remove, admin only. The daemon answers 202 and
// removes the member on its own time, so the fake answers the same shape: the
// member is gone from the list as far as a test is concerned, because the fake
// has no job to run, see internal/daemon/removal.go.
func (d *Daemon) removeUser(w http.ResponseWriter, r *http.Request) {
	d.wait()
	name := r.PathValue("name")
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path})
	if answer, ok := d.usersRefusal(); ok {
		d.mu.Unlock()
		write(w, answer)
		return
	}
	index, held := d.member(name)
	if !held {
		d.mu.Unlock()
		write(w, Problem(http.StatusNotFound, "not-found", "Not found",
			fmt.Sprintf("%s is not a member", name), ""))
		return
	}
	d.members = append(d.members[:index], d.members[index+1:]...)
	d.mu.Unlock()
	answer, _ := json.Marshal(map[string]any{"user": name, "state": "removing"})
	write(w, Response{Status: http.StatusAccepted, ContentType: "application/json", Body: string(answer)})
}

// member finds one member by name. The caller holds the lock.
func (d *Daemon) member(name string) (int, bool) {
	for i, held := range d.members {
		if held.User == name {
			return i, true
		}
	}
	return 0, false
}

// usersOverride is the answer AnswerUsers installed, if any. The caller holds
// the lock.
func (d *Daemon) usersOverride() (Response, bool) {
	if d.users.Status == 0 {
		return Response{}, false
	}
	return d.users, true
}

// usersRefusal is the answer an admin only endpoint gives before it does
// anything: an installed override, or the refusal a member gets. The caller
// holds the lock.
func (d *Daemon) usersRefusal() (Response, bool) {
	if answer, ok := d.usersOverride(); ok {
		return answer, true
	}
	if !d.identity.Admin {
		return Problem(http.StatusForbidden, "not-permitted", "Not permitted",
			"this call is for admins", "Ask an admin to run it."), true
	}
	return Response{}, false
}
