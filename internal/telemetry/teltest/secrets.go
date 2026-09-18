package teltest

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The secrets family of spec/kitbashd-api.yaml, as much of it as a session can
// tell apart: the caller is the fake's one identity, so there is no second
// member here to be kept out of. What a test asks of this fake is that the
// three tools reached the socket with the right method, path and body, and
// that a value never came back in a listing.

// secretName is the shape kitbashd holds a name to. It is spelled here rather
// than imported so this fake stays a fake of the wire.
var secretName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// maxSecretBytes is the longest value the real daemon carries.
const maxSecretBytes = 8192

// HeldSecret is one value the fake holds, which is what a test reads to see
// that a set crossed the socket with the value the agent sent.
type HeldSecret struct {
	Name    string
	Value   string
	Updated time.Time
}

// Secrets is what the fake holds for its caller, sorted by name.
func (d *Daemon) Secrets() []HeldSecret {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]HeldSecret, 0, len(d.held))
	for name, value := range d.held {
		out = append(out, HeldSecret{Name: name, Value: value.value, Updated: value.updated})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// AnswerSecrets replaces what the secrets family returns, which is how a
// daemon that refuses a call is exercised. The zero Response restores the
// fake's own behaviour.
func (d *Daemon) AnswerSecrets(r Response) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.secretsAnswer = r
}

// heldSecret is one value with the moment it was written.
type heldSecret struct {
	value   string
	updated time.Time
}

// setSecret answers secrets_set: the name in the path, the value in the body.
func (d *Daemon) setSecret(w http.ResponseWriter, r *http.Request) {
	d.wait()
	name := r.PathValue("name")
	body, err := readAll(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	if answer, ok := d.secretsOverride(); ok {
		d.mu.Unlock()
		write(w, answer)
		return
	}
	var in struct {
		Value string `json:"value"`
	}
	// The same four refusals the real daemon makes: a value is one line of an
	// environment file, so a NUL or a line break is not one.
	if err := json.Unmarshal(body, &in); err != nil || !secretName.MatchString(name) ||
		in.Value == "" || len(in.Value) > maxSecretBytes ||
		strings.ContainsAny(in.Value, "\x00\n\r") {
		d.mu.Unlock()
		write(w, Problem(http.StatusBadRequest, "bad-request", "Bad request",
			"the body is not a secret", "Send a name and a value of 1 to 8192 bytes."))
		return
	}
	if d.held == nil {
		d.held = map[string]heldSecret{}
	}
	updated := time.Now().UTC()
	d.held[name] = heldSecret{value: in.Value, updated: updated}
	d.mu.Unlock()
	answer, _ := json.Marshal(map[string]any{"name": name, "updated": updated.Format(time.RFC3339)})
	write(w, Response{Status: http.StatusOK, ContentType: "application/json", Body: string(answer)})
}

// listSecrets answers secrets_list: names and timestamps, never a value.
func (d *Daemon) listSecrets(w http.ResponseWriter, r *http.Request) {
	d.wait()
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path})
	if answer, ok := d.secretsOverride(); ok {
		d.mu.Unlock()
		write(w, answer)
		return
	}
	names := make([]string, 0, len(d.held))
	for name := range d.held {
		names = append(names, name)
	}
	sort.Strings(names)
	list := make([]map[string]any, 0, len(names))
	for _, name := range names {
		list = append(list, map[string]any{
			"name": name, "updated": d.held[name].updated.Format(time.RFC3339),
		})
	}
	d.mu.Unlock()
	answer, _ := json.Marshal(map[string]any{"secrets": list})
	write(w, Response{Status: http.StatusOK, ContentType: "application/json", Body: string(answer)})
}

// removeSecret answers secrets_remove, which is idempotent: a name the caller
// does not hold answers removed false.
func (d *Daemon) removeSecret(w http.ResponseWriter, r *http.Request) {
	d.wait()
	name := r.PathValue("name")
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path})
	if answer, ok := d.secretsOverride(); ok {
		d.mu.Unlock()
		write(w, answer)
		return
	}
	_, held := d.held[name]
	delete(d.held, name)
	d.mu.Unlock()
	answer, _ := json.Marshal(map[string]any{"name": name, "removed": held})
	write(w, Response{Status: http.StatusOK, ContentType: "application/json", Body: string(answer)})
}

// secretsOverride is the answer AnswerSecrets installed, if any. The caller
// holds the lock.
func (d *Daemon) secretsOverride() (Response, bool) {
	if d.secretsAnswer.Status == 0 {
		return Response{}, false
	}
	return d.secretsAnswer, true
}
