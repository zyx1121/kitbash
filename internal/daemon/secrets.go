package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/secrets"
	"github.com/zyx1121/kitbash/internal/store"
)

// The secrets family is served here and nowhere else, see the secrets family
// in spec/kitbashd-api.yaml. The values are root owned files and the daemon is
// the only reader: a member's session runs as the member, so a session that
// could read them would be a session that could read them for any Process it
// starts, and the whole point is that the value goes into the environment file
// kitbashd writes and into nothing a member's process ever holds.
//
// The owner of every call here is the socket's peer credentials and is never a
// field of a request. There is no name to ask on somebody else's behalf with,
// which is what makes "an admin reads nobody else's secrets" a property of the
// API rather than a check that could be forgotten, see PLAN.md section 2.3.

// secretRequest is the body of secrets_set. The name is in the path.
type secretRequest struct {
	Value string `json:"value"`
}

// secretResponse is what secrets_set answers: the name and when it was
// written. A value is never in an answer.
type secretResponse struct {
	Name    string `json:"name"`
	Updated string `json:"updated"`
}

// secretEntry is one row of secrets_list, and secretList the whole answer.
type secretEntry struct {
	Name    string `json:"name"`
	Updated string `json:"updated"`
}

type secretList struct {
	Secrets []secretEntry `json:"secrets"`
}

// removedSecret is what secrets_remove answers. Removing a name the caller
// does not hold is not a failure, so the answer says whether there was one.
type removedSecret struct {
	Name    string `json:"name"`
	Removed bool   `json:"removed"`
}

// secretsFamily answers GET on /kitbash/v1/secrets, which is the whole of
// secrets_list: the caller's own names and nobody else's.
func (s *Server) secretsFamily(w http.ResponseWriter, r *http.Request) {
	caller, prob := s.caller(r)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	if r.Method != http.MethodGet {
		writeProblem(w, wrongMethod(r,
			"Call GET to list your secrets, PUT on the name to set one, or DELETE on the name to remove one."))
		return
	}
	if prob := s.notRemoving(r, caller.User, "read their secrets"); prob != nil {
		writeProblem(w, prob)
		return
	}
	held, err := s.secrets.List(caller.User)
	if err != nil {
		writeProblem(w, s.secretProblem(r, err, ""))
		return
	}
	out := secretList{Secrets: make([]secretEntry, 0, len(held))}
	for _, entry := range held {
		out.Secrets = append(out.Secrets, secretEntry{Name: entry.Name, Updated: written(entry.Updated)})
	}
	writeJSON(w, r.URL.Path, out)
}

// secret answers PUT and DELETE on /kitbash/v1/secrets/{name}.
func (s *Server) secret(w http.ResponseWriter, r *http.Request) {
	caller, prob := s.caller(r)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, secretsPath+"/")
	if name == "" || strings.Contains(name, "/") {
		writeProblem(w, problem.NotFoundFix(r.URL.Path,
			fmt.Sprintf("%s is not part of the kitbashd API", r.URL.Path),
			"Call the secret's name, such as /kitbash/v1/secrets/ANTHROPIC_API_KEY."))
		return
	}
	// The removal deletes this whole directory at its secrets step, so a
	// value written while it runs is a value that outlives the account it
	// belongs to and is handed to the next member created with that name,
	// see removal.go and PLAN.md section 2.3.
	if prob := s.notRemoving(r, caller.User, "set or remove a secret"); prob != nil {
		writeProblem(w, prob)
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.setSecret(w, r, caller, name)
	case http.MethodDelete:
		s.removeSecret(w, r, caller, name)
	default:
		writeProblem(w, wrongMethod(r, "Call PUT on the name to set a secret, or DELETE on the name to remove one."))
	}
}

// setSecret writes one value of the caller's. Nothing is restarted: which
// Processes take a new value is the owner's decision and proc_stop followed by
// proc_run is how it is made, see PLAN.md section 2.3.
func (s *Server) setSecret(w http.ResponseWriter, r *http.Request, caller Caller, name string) {
	var req secretRequest
	if prob := decodeBody(w, r, &req); prob != nil {
		writeProblem(w, prob)
		return
	}
	updated, err := s.secrets.Set(caller.User, name, req.Value)
	if err != nil {
		writeProblem(w, s.secretProblem(r, err, name))
		return
	}
	// The log line names the member and the name and never the value, which is
	// the rule every line about a secret follows.
	logger.Printf("secrets: %s set %s", caller.User, name)
	writeJSON(w, r.URL.Path, secretResponse{Name: name, Updated: written(updated)})
}

// removeSecret drops one value of the caller's. It is idempotent: a name they
// do not hold answers removed false, because the call describes the state it
// wants, which is the invariant in PLAN.md section 2.6.
func (s *Server) removeSecret(w http.ResponseWriter, r *http.Request, caller Caller, name string) {
	removed, err := s.secrets.Remove(caller.User, name)
	if err != nil {
		writeProblem(w, s.secretProblem(r, err, name))
		return
	}
	if removed {
		logger.Printf("secrets: %s removed %s", caller.User, name)
	}
	writeJSON(w, r.URL.Path, removedSecret{Name: name, Removed: removed})
}

// resolveSecrets answers the environment one Process's declared names resolve
// to at this moment. It is called by every start, the first and every restore,
// so a value set after the Process was registered is the one the next start
// reads: the names travel with the registration and the values never do, which
// is what makes rotation one call, see PLAN.md section 2.3.
//
// A declared name the owner has not set is not-found, naming the secret and
// the call that fixes it. Nothing is created before this runs, so a Process
// whose credential is missing is refused rather than started without it.
func (s *Server) resolveSecrets(instance string, p store.Process) (map[string]string, *problem.Problem) {
	return s.resolveSecretNames(instance, p.Owner, p.Secrets)
}

// resolveSecretNames is resolveSecrets for a list the caller holds, which is
// what one unit of a pod has: the names are per unit, and the registration
// carries one list for each, see PLAN.md section 5.6.
func (s *Server) resolveSecretNames(instance, owner string, names []string) (map[string]string, *problem.Problem) {
	if len(names) == 0 {
		return nil, nil
	}
	resolved := make(map[string]string, len(names))
	for _, name := range names {
		// A name the registration should never have carried is refused here
		// rather than opened: this is what builds a path, so it is the one
		// that must not be talked into somebody else's.
		if !manifest.ValidSecretName(name) || strings.HasPrefix(name, manifest.OwnedEnvPrefix) {
			return nil, problem.BadRequest(instance,
				fmt.Sprintf("%q is not a secret name this Process may declare", name),
				"Declare deploy.units[0].secrets as environment variable names, none of them a KITBASH_ name.")
		}
		value, held, err := s.secrets.Get(owner, name)
		if err != nil {
			return nil, problem.Internal(instance, err.Error(), "")
		}
		if !held {
			return nil, problem.NotFoundFix(instance,
				fmt.Sprintf("this Process declares the secret %s and %s has not set it", name, owner),
				fmt.Sprintf("Call secrets_set %s, then run the Package again.", name))
		}
		resolved[name] = value
	}
	return resolved, nil
}

// unresolvedSecret is what restore records about a Process whose declared secret
// is not there, in the two sentences proc_list carries. It is the same shape a
// mount that stopped being legal takes: the Process is not running, and the
// owner reads why and what to do, see mountProblem.
func unresolvedSecret(prob *problem.Problem) restoreProblem {
	fix := prob.Fix
	if fix == "" {
		fix = "Set the secret the unit declares with secrets_set, then run the Package again."
	}
	return restoreProblem{
		Detail: "this Process declares a secret kitbashd could not resolve, so it did not start it: " + prob.Detail,
		Fix:    fix,
	}
}

// secretProblem turns a failure of the secrets store into problem details.
// Only the three named refusals reach the caller as themselves; everything
// else is the host, whose message names paths a member can do nothing with, so
// it goes to the daemon log as an internal cause. No refusal here quotes a
// value: the shape is what was wrong with it.
func (s *Server) secretProblem(r *http.Request, err error, name string) *problem.Problem {
	switch {
	case errors.Is(err, secrets.ErrName):
		return problem.BadRequest(r.URL.Path,
			fmt.Sprintf("%q is not a secret name", name),
			"Name a secret with capital letters, digits and underscores, starting with a letter, at most 64 characters.")
	case errors.Is(err, secrets.ErrValue):
		return problem.BadRequest(r.URL.Path,
			fmt.Sprintf("the value of %s is not one an environment file carries: %s", name, valueShape(err)),
			fmt.Sprintf("Send a value of 1 to %d bytes, with no NUL and no line break; encode a multi line credential first.",
				secrets.MaxValueBytes))
	case errors.Is(err, secrets.ErrMember):
		// The member is the peer, so this is a host whose account names this
		// daemon cannot spell rather than anything the caller sent.
		return problem.Internal(r.URL.Path, err.Error(), "")
	}
	return problem.Internal(r.URL.Path, err.Error(), "")
}

// valueShape is the part of a value refusal that describes the value without
// quoting it: internal/secrets writes "the value is not one ...: it is empty",
// and what a caller needs is the half after the colon.
func valueShape(err error) string {
	message := err.Error()
	if _, after, found := strings.Cut(message, ": "); found {
		return after
	}
	return message
}

// written renders when a secret was last set, in the RFC 3339 shape every
// other timestamp of this API carries.
func written(t time.Time) string { return t.UTC().Format(timeLayout) }
