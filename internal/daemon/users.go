package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// The two sub paths under /kitbash/v1/users that are not a member name.
const (
	mePath   = "me"
	keysPath = "keys"
)

// userRequest is the users_create input of spec/kitbashd-api.yaml.
type userRequest struct {
	Name   string `json:"name"`
	SSHKey string `json:"sshKey"`
	Admin  bool   `json:"admin,omitempty"`
}

// keyRequest is the users_add_key input. The name is in the path.
type keyRequest struct {
	SSHKey string `json:"sshKey"`
}

// userResponse is what users_create answers.
type userResponse struct {
	User  string `json:"user"`
	UID   int    `json:"uid"`
	Admin bool   `json:"admin"`
}

// meResponse is what users_me answers about the peer.
type meResponse struct {
	User   string   `json:"user"`
	UID    int      `json:"uid"`
	Admin  bool     `json:"admin"`
	Groups []string `json:"groups"`
}

// memberResponse is one row of users_list: the member and how many Processes
// they hold registered.
type memberResponse struct {
	User      string `json:"user"`
	UID       int    `json:"uid"`
	Admin     bool   `json:"admin"`
	Keys      int    `json:"keys"`
	Home      string `json:"home"`
	Processes int    `json:"processes"`
	// State is removing for a member whose removal job is still running and
	// empty for every other member. An account being taken away is still an
	// account on this host, so it is listed, and what it is listed as is what
	// is happening to it, see removal.go.
	State string `json:"state,omitempty"`
}

// userList is what users_list answers: every member, and every removal this
// host remembers. The second is how a removal that is over is read, because
// the member it was about is not a member any more: one that finished is
// removed and one that did not is failed with the step that failed.
type userList struct {
	Users    []memberResponse `json:"users"`
	Removals []store.Removal  `json:"removals"`
}

// keyResponse is what users_add_key answers.
type keyResponse struct {
	User string `json:"user"`
	Keys int    `json:"keys"`
}

// removeResponse is what users_remove answers: the member and the state their
// removal is in, which is removing, because the work is a job kitbashd runs
// for itself and the answer is 202, see removal.go. Where the home went is
// read through users_list once the job has moved it, because the archive is a
// step of the job and not of this call.
type removeResponse struct {
	User  string `json:"user"`
	State string `json:"state"`
}

// usersFamily answers POST and GET on /kitbash/v1/users.
func (s *Server) usersFamily(w http.ResponseWriter, r *http.Request) {
	caller, prob := s.caller(r)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.createUser(w, r, caller)
	case http.MethodGet:
		s.listUsers(w, r, caller)
	default:
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s %s is not part of the kitbashd API", r.Method, r.URL.Path),
			"Call POST to create a member, GET to list them, or DELETE on the member name to remove one."))
	}
}

// user answers the paths under /kitbash/v1/users: me, a member's keys, and a
// member to remove.
func (s *Server) user(w http.ResponseWriter, r *http.Request) {
	caller, prob := s.caller(r)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, usersPath+"/")
	name, tail, _ := strings.Cut(rest, "/")

	switch {
	case name == mePath && tail == "":
		if r.Method != http.MethodGet {
			writeProblem(w, wrongMethod(r, "Call GET to read who you are."))
			return
		}
		s.me(w, r, caller)
	case tail == keysPath:
		if r.Method != http.MethodPost {
			writeProblem(w, wrongMethod(r, "Call POST to add a key to a member."))
			return
		}
		s.addKey(w, r, caller, name)
	case tail == "":
		if r.Method != http.MethodDelete {
			writeProblem(w, wrongMethod(r, "Call DELETE on the member name to remove a member."))
			return
		}
		s.removeUser(w, r, caller, name)
	default:
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s is not part of the kitbashd API", r.URL.Path),
			"Call the member name, or the member name with /keys."))
	}
}

// me answers who the peer is. The uid and the groups come from the host; the
// admin flag is the one the rest of this API decides by, so it is the same
// answer every other path here uses.
func (s *Server) me(w http.ResponseWriter, r *http.Request, caller Caller) {
	out := meResponse{
		User:   caller.User,
		UID:    int(caller.Peer.UID),
		Admin:  caller.Admin,
		Groups: []string{},
	}
	m, found, err := s.users.Lookup(r.Context(), caller.User)
	if err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	// The peer is the identity whether or not the host can list their groups,
	// so a member the group database does not describe still gets an answer.
	if found {
		out.UID = m.UID
		out.Groups = m.Groups
		writeJSON(w, r.URL.Path, out)
		return
	}
	// Unless the account was removed. A session open across a removal keeps
	// its uid for as long as it runs, and this host has answered the removal
	// already: the member is gone, so the honest answer about them is that
	// there is none, and not the identity of an account nothing on this host
	// has any more, see removal.go.
	if gone, prob := s.removed(r, caller.User); prob != nil {
		writeProblem(w, prob)
		return
	} else if gone {
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s is not a member of this host", caller.User),
			"Ask an administrator to create a member for this uid."))
		return
	}
	writeJSON(w, r.URL.Path, out)
}

// createUser makes one member. Creating a member is root's work, which is why
// it is served here and not by the session, see PLAN.md section 4.5.
func (s *Server) createUser(w http.ResponseWriter, r *http.Request, caller Caller) {
	if prob := s.requireAdmin(r, caller, "create a member"); prob != nil {
		writeProblem(w, prob)
		return
	}
	var req userRequest
	if prob := decodeBody(w, r, &req); prob != nil {
		writeProblem(w, prob)
		return
	}
	if !sysusers.ValidName(req.Name) {
		writeProblem(w, problem.BadRequest(r.URL.Path,
			fmt.Sprintf("%q is not a member name", req.Name),
			"Send a name of 2 to 32 characters, starting with a letter and made of lower case letters, digits and hyphens."))
		return
	}

	m, err := s.users.Create(r.Context(), sysusers.Spec{
		Name:   req.Name,
		SSHKey: req.SSHKey,
		Admin:  req.Admin,
	})
	if err != nil {
		writeProblem(w, s.userProblem(r, err, req.Name))
		return
	}
	// The member's cgroup subtree is created with the account, so their first
	// Process is placed without waiting for the next daemon start, see
	// internal/cgroups.
	s.memberCgroup(r.Context(), m)
	// A name removed once and created again is a member of its own, so what
	// this host remembers about the removal of the old one is not about them,
	// see removal.go.
	if err := s.store.DeleteRemoval(r.Context(), m.Name); err != nil {
		logger.Printf("users: the removal record of %s could not be forgotten: %v", m.Name, err)
	}
	logger.Printf("%s created the member %s (uid %d, admin %t)", caller.User, m.Name, m.UID, m.Admin)
	writeJSON(w, r.URL.Path, userResponse{User: m.Name, UID: m.UID, Admin: m.Admin})
}

// listUsers answers every member with their key and Process counts.
func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, caller Caller) {
	if prob := s.requireAdmin(r, caller, "list the members"); prob != nil {
		writeProblem(w, prob)
		return
	}
	members, err := s.users.List(r.Context())
	if err != nil {
		writeProblem(w, s.userProblem(r, err, ""))
		return
	}
	counts, err := s.store.ProcessCounts(r.Context())
	if err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	removals, err := s.store.Removals(r.Context())
	if err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	state := map[string]string{}
	for _, removal := range removals {
		state[removal.Name] = removal.State
	}
	out := userList{
		Users:    make([]memberResponse, 0, len(members)),
		Removals: removals,
	}
	if out.Removals == nil {
		out.Removals = []store.Removal{}
	}
	for _, m := range members {
		row := memberResponse{
			User:      m.Name,
			UID:       m.UID,
			Admin:     m.Admin,
			Keys:      m.Keys,
			Home:      m.Home,
			Processes: counts[m.Name],
		}
		if state[m.Name] == store.Removing {
			row.State = store.Removing
		}
		out.Users = append(out.Users, row)
	}
	writeJSON(w, r.URL.Path, out)
}

// addKey adds one public key to a member. A key already on the member is not
// added twice, so the call is safe to repeat.
func (s *Server) addKey(w http.ResponseWriter, r *http.Request, caller Caller, name string) {
	if prob := s.requireAdmin(r, caller, "add a key to a member"); prob != nil {
		writeProblem(w, prob)
		return
	}
	if !sysusers.ValidName(name) {
		writeProblem(w, problem.BadRequest(r.URL.Path,
			fmt.Sprintf("%q is not a member name", name),
			"Call the path with the member name, such as /kitbash/v1/users/alice/keys."))
		return
	}
	var req keyRequest
	if prob := decodeBody(w, r, &req); prob != nil {
		writeProblem(w, prob)
		return
	}
	m, err := s.users.AddKey(r.Context(), name, req.SSHKey)
	if err != nil {
		writeProblem(w, s.userProblem(r, err, name))
		return
	}
	writeJSON(w, r.URL.Path, keyResponse{User: m.Name, Keys: m.Keys})
}

// removeUser takes a member away. Two refusals come before anything is done:
// an admin may not remove themselves, and the last admin may not go, because a
// host with no admin has no way back to one through this API.
//
// What the call does beyond refusing is start a job: the member is marked
// removing, the answer is 202, and their containers, registrations, approvals,
// builds, secrets, home and account are taken away by kitbashd on its own
// time. A removal is not the length of a request, and a member holding
// seventeen Processes used to outrun the caller's deadline and leave half an
// account behind, see removal.go and issue #152.
func (s *Server) removeUser(w http.ResponseWriter, r *http.Request, caller Caller, name string) {
	if prob := s.requireAdmin(r, caller, "remove a member"); prob != nil {
		writeProblem(w, prob)
		return
	}
	if !sysusers.ValidName(name) {
		writeProblem(w, problem.BadRequest(r.URL.Path,
			fmt.Sprintf("%q is not a member name", name),
			"Call DELETE on the member name, such as /kitbash/v1/users/alice."))
		return
	}
	if name == caller.User {
		writeProblem(w, problem.ConflictFix(r.URL.Path,
			fmt.Sprintf("%s may not remove themselves", caller.User),
			"Ask another administrator to remove this member."))
		return
	}
	// A member the job is already working through is answered what they are:
	// the call describes the state the caller asked for, so a second one is
	// the same answer and not a second job.
	if s.isRemoving(name) {
		writeJSONStatus(w, r.URL.Path, http.StatusAccepted,
			removeResponse{User: name, State: store.Removing})
		return
	}

	target, found, err := s.users.Lookup(r.Context(), name)
	if err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	// root is refused by name before anything else looks at it. The
	// membership test below would refuse it too, but the account that runs
	// this daemon is worth its own sentence.
	if found && target.UID == 0 {
		writeProblem(w, problem.NotPermitted(r.URL.Path,
			fmt.Sprintf("%s is the host's own account and is not a member", name),
			"Members are the accounts users_list answers with; the host's own accounts are not managed here."))
		return
	}
	// An account that is not a member of kitbash-users is not found as far as
	// this family is concerned. Without this rule the path is a way to delete
	// sshd, the build user or any other account on the host.
	if !found || !target.IsMember() {
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s is not a member of this host", name),
			"List the members to see which names exist."))
		return
	}
	if target.Admin {
		last, prob := s.lastAdmin(r, name)
		if prob != nil {
			writeProblem(w, prob)
			return
		}
		if last {
			writeProblem(w, problem.ConflictFix(r.URL.Path,
				fmt.Sprintf("%s is the only administrator on this host", name),
				"Create another administrator first, then remove this one."))
			return
		}
	}

	// The mark is written before the answer, so a daemon that stops between
	// the two takes the job up again at its next start rather than leaving a
	// member an admin believes is going.
	if _, err := s.store.BeginRemoval(r.Context(), name, s.now()); err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	logger.Printf("%s is removing the member %s", caller.User, name)
	s.startRemoval(name, caller.User)
	writeJSONStatus(w, r.URL.Path, http.StatusAccepted, removeResponse{User: name, State: store.Removing})
}

// removed reports whether this host has removed one member, which is what
// tells an account that is simply not described by the group database from one
// that is gone.
func (s *Server) removed(r *http.Request, name string) (bool, *problem.Problem) {
	removal, held, err := s.store.Removal(r.Context(), name)
	if err != nil {
		return false, problem.Internal(r.URL.Path, err.Error(), "")
	}
	return held && removal.State == store.Removed, nil
}

// lastAdmin reports whether this member is the only administrator left.
func (s *Server) lastAdmin(r *http.Request, name string) (bool, *problem.Problem) {
	members, err := s.users.List(r.Context())
	if err != nil {
		return false, s.userProblem(r, err, name)
	}
	for _, m := range members {
		if m.Admin && m.Name != name {
			return false, nil
		}
	}
	return true, nil
}

// requireAdmin refuses a member who is not in kitbash-admin.
func (s *Server) requireAdmin(r *http.Request, caller Caller, doing string) *problem.Problem {
	if caller.Admin {
		return nil
	}
	return problem.NotPermitted(r.URL.Path,
		fmt.Sprintf("%s is not a member of %s, so may not %s", caller.User, AdminGroup, doing),
		"Ask an administrator to run this call.")
}

// userProblem turns a failure of the users family into problem details. Only
// the four named failures reach the caller as themselves; everything else is
// the host refusing, and its message names host paths and shadow utilities an
// agent can do nothing with, so it goes to the server log instead.
func (s *Server) userProblem(r *http.Request, err error, name string) *problem.Problem {
	switch {
	case errors.Is(err, sysusers.ErrExists):
		return problem.ConflictFix(r.URL.Path,
			fmt.Sprintf("%s is already a member of this host", name),
			"Add a key to the existing member instead, or choose another name.")
	case errors.Is(err, sysusers.ErrNotFound):
		return problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s is not a member of this host", name),
			"List the members to see which names exist.")
	case errors.Is(err, sysusers.ErrName):
		return problem.BadRequest(r.URL.Path,
			fmt.Sprintf("%q is not a member name", name),
			"Send a name of 2 to 32 characters, starting with a letter and made of lower case letters, digits and hyphens.")
	case errors.Is(err, sysusers.ErrKey):
		return problem.BadRequest(r.URL.Path,
			"the sshKey is not one OpenSSH public key line",
			"Send one line of an id_ed25519.pub or id_rsa.pub file, type and base64 body with an optional comment.")
	}
	return problem.Internal(r.URL.Path, err.Error(), "")
}

// wrongMethod is the answer to a verb a path does not carry, in the shape
// every other error of this API has.
func wrongMethod(r *http.Request, fix string) *problem.Problem {
	return problem.NotFoundFix(r.URL.Path,
		fmt.Sprintf("%s %s is not part of the kitbashd API", r.Method, r.URL.Path), fix)
}
