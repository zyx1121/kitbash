package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The composition half of the job, PLAN.md 5.6: a Package of two units runs as
// one pod, the counter answers at its address and increments across calls
// because the count lives in the other unit of its own Package, and proc_stop
// leaves no pod and no container behind.
//
// This is the first fixture with an HTTP listener, so it is also the first
// address this job requests. The port is on the host's loopback, which the job
// can reach; what it proves is that the two containers share one network
// namespace, because the counter reaches its cache on localhost and nothing
// told it where that is.

// pairName is the Package of two units and pairBudget how long its address is
// given to start answering. A pod is two containers and the second of them is
// pulled, so the first request may arrive before the entrypoint has listened.
const (
	pairName   = "pair"
	pairBudget = 60 * time.Second
)

// runThePair writes the Package, builds every unit of it and runs the pod.
func runThePair(t *testing.T, s *state) {
	writeFixture(t, s.admin, pairName, s.pairPath)

	var built struct {
		Digest string `json:"digest"`
	}
	res := s.admin.callWithin(buildTimeout, "pkg_build", map[string]any{"path": s.pairPath})
	res.mustSucceed(t, "pkg_build")
	if err := json.Unmarshal(res.Structured, &built); err != nil {
		t.Fatalf("decoding pkg_build: %v", err)
	}
	if built.Digest == "" {
		t.Fatal("pkg_build of a Package of two units answered no digest")
	}

	var out struct {
		ID       string `json:"id"`
		State    string `json:"state"`
		Expose   string `json:"expose"`
		Endpoint string `json:"endpoint"`
		Digest   string `json:"digest"`
		Units    []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"units"`
	}
	run := s.admin.callWithin(buildTimeout, "proc_run", map[string]any{"package": s.pairPath})
	run.mustSucceed(t, "proc_run")
	if err := json.Unmarshal(run.Structured, &out); err != nil {
		t.Fatalf("decoding proc_run: %v", err)
	}
	if out.State != "running" {
		t.Fatalf("proc_run answered %+v, want a running Process", out)
	}
	if out.Expose != "http" || out.Endpoint == "" {
		t.Fatalf("proc_run answered expose %q and endpoint %q, want http and an address", out.Expose, out.Endpoint)
	}
	if out.Digest != built.Digest {
		t.Fatalf("proc_run runs %s, want the digest pkg_build answered, %s", out.Digest, built.Digest)
	}
	if len(out.Units) != 2 {
		t.Fatalf("proc_run answered %d units, want the two the manifest declares: %+v", len(out.Units), out.Units)
	}
	for _, unit := range out.Units {
		if unit.State != "running" {
			t.Errorf("the unit %s is %s, want running", unit.Name, unit.State)
		}
	}
	s.pairID = out.ID
	s.pairEndpoint = out.Endpoint
}

// thePodHoldsBothUnits asks the runtime itself, as the member who owns it: one
// pod named after the Process and one container per unit inside it.
func thePodHoldsBothUnits(t *testing.T, s *state) {
	pods, err := runAs(t, adminName(), "podman", "pod", "ps", "--format", "{{.Name}} {{.NumberOfContainers}}")
	if err != nil {
		t.Fatalf("podman pod ps: %v\n%s", err, pods)
	}
	pod := "kitbash-" + pairName + "-" + pairName
	line := ""
	for _, l := range strings.Split(pods, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), pod+" ") {
			line = strings.TrimSpace(l)
		}
	}
	if line == "" {
		t.Fatalf("podman pod ps does not list %s:\n%s", pod, pods)
	}
	// Two units and the infra container podman makes to hold the namespaces.
	if !strings.HasSuffix(line, " 3") {
		t.Errorf("the pod is %q, want three containers: two units and the infra container", line)
	}

	containers, err := runAs(t, adminName(), "podman", "ps", "--filter", "pod="+pod, "--format", "{{.Names}}")
	if err != nil {
		t.Fatalf("podman ps: %v\n%s", err, containers)
	}
	for _, unit := range []string{"web", "cache"} {
		if !strings.Contains(containers, pod+"-"+unit) {
			t.Errorf("the runtime does not hold %s-%s:\n%s", pod, unit, containers)
		}
	}
}

// theCounterIncrements is the sentence M12 is for: the address answers, and it
// answers a number that goes up, which it can only do by reaching the other
// unit of its own Package on localhost.
func theCounterIncrements(t *testing.T, s *state) {
	first := pairCount(t, s.pairEndpoint, pairBudget)
	second := pairCount(t, s.pairEndpoint, 10*time.Second)
	if second != first+1 {
		t.Fatalf("the counter answered %d then %d, want it to increment across calls", first, second)
	}
}

// pairCount requests the counter once and answers the number it returned,
// waiting out the budget for a Process whose entrypoint has not listened yet.
func pairCount(t *testing.T, endpoint string, budget time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(budget)
	var last string
	for {
		count, body, err := requestCount(endpoint)
		if err == nil {
			return count
		}
		last = body
		if time.Now().After(deadline) {
			t.Fatalf("the address %s did not answer within %s: %v\n%s", endpoint, budget, err, last)
		}
		time.Sleep(time.Second)
	}
}

func requestCount(endpoint string) (int, string, error) {
	res, err := (&http.Client{Timeout: 5 * time.Second}).Get(endpoint + "/")
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return 0, "", err
	}
	var answer struct {
		Count int    `json:"count"`
		Unit  string `json:"unit"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return 0, string(body), err
	}
	if res.StatusCode != http.StatusOK || answer.Count == 0 {
		return 0, string(body), errNotCounting
	}
	// Every unit of a pod is given the name of the unit it is, which the face
	// answers with so the job reads it from inside the container.
	if answer.Unit != "web" {
		return 0, string(body), errNotCounting
	}
	return answer.Count, string(body), nil
}

// errNotCounting is an address that answered something other than a count.
var errNotCounting = errPair("the address did not answer a count")

type errPair string

func (e errPair) Error() string { return string(e) }

// stopThePair stops the Process, which stops and removes the whole pod: a pod
// left behind is a name the next run cannot take and a port nothing answers
// on, see PLAN.md section 5.6.
func stopThePair(t *testing.T, s *state) {
	var out struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	s.admin.ok("proc_stop", map[string]any{"id": s.pairID}, &out)
	if out.State != "stopped" {
		t.Fatalf("proc_stop answered %+v, want a stopped Process", out)
	}

	pod := "kitbash-" + pairName + "-" + pairName
	// podman pod ps lists every pod and takes no --all: a pod that is stopped
	// is still a pod, which is the thing this asks about.
	pods, err := runAs(t, adminName(), "podman", "pod", "ps", "--format", "{{.Name}}")
	if err != nil {
		t.Fatalf("podman pod ps: %v\n%s", err, pods)
	}
	if strings.Contains(pods, pod) {
		t.Errorf("the pod %s is still on the host after proc_stop:\n%s", pod, pods)
	}
	containers, err := runAs(t, adminName(), "podman", "ps", "--all", "--format", "{{.Names}}")
	if err != nil {
		t.Fatalf("podman ps: %v\n%s", err, containers)
	}
	if strings.Contains(containers, pod) {
		t.Errorf("a container of %s is still on the host after proc_stop:\n%s", pod, containers)
	}
}
