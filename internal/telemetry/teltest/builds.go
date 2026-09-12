package teltest

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/zyx1121/kitbash/internal/podman"
)

// kitbashd records who built which image and copies one between two members'
// stores, so the fake does too: a session records its build, reads what other
// members built, and asks for a copy, see spec/kitbashd-api.yaml builds_record,
// builds_list and images_fetch.

// Build is one build record, as the fake holds it.
type Build struct {
	Path    string `json:"path"`
	Commit  string `json:"commit"`
	Digest  string `json:"digest"`
	Builder string `json:"builder,omitempty"`
	BuiltAt string `json:"builtAt,omitempty"`
	Size    int64  `json:"size,omitempty"`
}

// Fetch is one recorded image copy: which image was asked for, for which
// Package, and from which member.
type Fetch struct {
	Digest string
	Path   string
	From   string
}

// AddBuild seeds the registry, standing in for a build another member made.
// A record with no builder is this fake's own caller, which is what a session
// recording its own build produces.
func (d *Daemon) AddBuild(b Build) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addBuild(b)
}

// addBuild records one build. The caller holds the lock.
func (d *Daemon) addBuild(b Build) {
	if b.Builder == "" {
		b.Builder = d.identity.User
	}
	if b.BuiltAt == "" {
		d.buildSeq++
		b.BuiltAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).
			Add(time.Duration(d.buildSeq) * time.Minute).Format(time.RFC3339)
	}
	for i, held := range d.builds {
		if held.Path == b.Path && held.Commit == b.Commit && held.Digest == b.Digest {
			d.builds[i] = b
			return
		}
	}
	d.builds = append(d.builds, b)
}

// Builds are the build records the fake holds, newest first, which is the
// order it answers in.
func (d *Daemon) Builds() []Build {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sorted()
}

// sorted is the registry newest first. The caller holds the lock.
func (d *Daemon) sorted() []Build {
	out := append([]Build(nil), d.builds...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].BuiltAt > out[j].BuiltAt })
	return out
}

// Fetches are the image copies the fake was asked for, in order.
func (d *Daemon) Fetches() []Fetch {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Fetch(nil), d.fetches...)
}

// AnswerBuilds replaces what the build registry returns, which is how a daemon
// that refuses to record or to list is exercised. The zero Response restores
// the registry's own behaviour.
func (d *Daemon) AnswerBuilds(r Response) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.buildsAnswer = r
}

// AnswerFetch replaces what a fetch returns, which is how a copy that fails is
// exercised. The zero Response restores the fake's own behaviour.
func (d *Daemon) AnswerFetch(r Response) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fetchAnswer = r
}

// recordBuild answers builds_record: it stores the build for the caller, who
// is this fake's one identity.
func (d *Daemon) recordBuild(w http.ResponseWriter, r *http.Request) {
	d.wait()
	body, err := readAll(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	if d.buildsAnswer.Status != 0 {
		answer := d.buildsAnswer
		d.mu.Unlock()
		write(w, answer)
		return
	}
	var b Build
	if err := json.Unmarshal(body, &b); err != nil || b.Path == "" || b.Digest == "" {
		d.mu.Unlock()
		write(w, Problem(http.StatusBadRequest, "bad-request", "Bad request",
			"the body is not a build record", ""))
		return
	}
	b.Builder = d.identity.User
	d.addBuild(b)
	d.mu.Unlock()
	encoded, _ := json.Marshal(b)
	write(w, Response{Status: http.StatusOK, ContentType: "application/json", Body: string(encoded)})
}

// listBuilds answers builds_list, filtered the way the daemon filters.
func (d *Daemon) listBuilds(w http.ResponseWriter, r *http.Request) {
	d.wait()
	query := r.URL.Query()
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path + "?" + r.URL.RawQuery})
	if d.buildsAnswer.Status != 0 {
		answer := d.buildsAnswer
		d.mu.Unlock()
		write(w, answer)
		return
	}
	list := []Build{}
	for _, b := range d.sorted() {
		if b.Path != query.Get("path") {
			continue
		}
		if commit := query.Get("commit"); commit != "" && b.Commit != commit {
			continue
		}
		if digest := query.Get("digest"); digest != "" && b.Digest != digest {
			continue
		}
		list = append(list, b)
	}
	d.mu.Unlock()
	encoded, _ := json.Marshal(map[string]any{"builds": list})
	write(w, Response{Status: http.StatusOK, ContentType: "application/json", Body: string(encoded)})
}

// fetchImage answers images_fetch: it records the call and, when the fake
// mirrors a runtime, puts the image in it the way a load into the caller's own
// store would. The image carries the labels a build of that Package writes, so
// the session reads it back as a build of this Package.
func (d *Daemon) fetchImage(w http.ResponseWriter, r *http.Request) {
	d.wait()
	digest := r.PathValue("digest")
	body, err := readAll(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req struct {
		Path string `json:"path"`
		From string `json:"from"`
	}
	json.Unmarshal(body, &req)

	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	d.fetches = append(d.fetches, Fetch{Digest: digest, Path: req.Path, From: req.From})
	if d.fetchAnswer.Status != 0 {
		answer := d.fetchAnswer
		d.mu.Unlock()
		write(w, answer)
		return
	}
	var record Build
	found := false
	for _, b := range d.sorted() {
		if b.Path == req.Path && b.Digest == digest {
			record = b
			found = true
			break
		}
	}
	runtime := d.runtime
	d.mu.Unlock()

	if !found {
		write(w, Problem(http.StatusNotFound, "not-found", "Not found",
			"kitbashd has no build of "+req.Path+" with the digest "+digest, ""))
		return
	}
	if runtime != nil {
		runtime.AddImage(podman.Image{ID: digest, Labels: map[string]string{
			podman.LabelPath:   record.Path,
			podman.LabelName:   name(record.Path),
			podman.LabelCommit: record.Commit,
			podman.LabelUser:   record.Builder,
		}})
	}
	answer, _ := json.Marshal(map[string]any{
		"digest": digest, "bytes": record.Size, "from": record.Builder,
	})
	write(w, Response{Status: http.StatusOK, ContentType: "application/json", Body: string(answer)})
}

// name is the last segment of a Package path, which is what a Package is
// usually called when nothing else says otherwise.
func name(path string) string {
	cut := strings.LastIndex(path, "/")
	if cut < 0 {
		return path
	}
	return path[cut+1:]
}
