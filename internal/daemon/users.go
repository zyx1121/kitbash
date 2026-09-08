package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/zyx1121/kitbash/internal/problem"
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
}

// userList is what users_list answers.
type userList struct {
	Users []memberResponse `json:"users"`
}

// keyResponse is what users_add_key answers.
type keyResponse struct {
	User string `json:"user"`
	Keys int    `json:"keys"`
}

// removeResponse is what users_remove answers: where the home went, so an
// admin who deleted the wrong member knows where to find it.
type removeResponse struct {
	User     string `json:"user"`
	Archived string `json:"archived"`
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
	out := userList{Users: make([]memberResponse, 0, len(members))}
	for _, m := range members {
		out.Users = append(out.Users, memberResponse{
			User:      m.Name,
			UID:       m.UID,
			Admin:     m.Admin,
			Keys:      m.Keys,
			Home:      m.Home,
			Processes: counts[m.Name],
		})
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

	// The host goes first: the containers stop, the sessions end, the account
	// is deleted and the home is archived. Only then does the store forget the
	// member, because a registration deleted for an account that is still
	// there is a Process whose Telemetry token was revoked for nothing.
	archived, err := s.users.Remove(r.Context(), name)
	if err != nil {
		writeProblem(w, s.userProblem(r, err, name))
		return
	}
	// Every registration is a live Telemetry token, and a token outliving the
	// member it names is a producer nobody owns, see spec/kitbashd-api.yaml.
	ids, err := s.store.DeleteProcessesByOwner(r.Context(), name)
	if err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	for _, id := range ids {
		s.fanout.untrack(id)
		s.endMCPSessions(id)
	}
	if _, err := s.store.DeleteApprovals(r.Context(), name); err != nil {
		writeProblem(w, problem.Internal(r.URL.Path, err.Error(), ""))
		return
	}
	logger.Printf("%s removed the member %s, home archived at %s", caller.User, name, archived)
	writeJSON(w, r.URL.Path, removeResponse{User: name, Archived: archived})
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
