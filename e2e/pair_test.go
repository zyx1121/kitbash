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
	// probeBudget is how long the first health record of the face is waited
	// for, several times the five second interval the fixture declares, and
	// unitDownBudget how long the runtime is given to report a container that
	// was stopped by hand.
	probeBudget    = 45 * time.Second
	unitDownBudget = 30 * time.Second
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

// readEitherUnit is the surface half of M12, issue #163: proc_logs reads the
// unit it is asked for, pkg_inspect says which units the Package declares and
// which of them is the face, and a name that is not a unit is not-found.
func readEitherUnit(t *testing.T, s *state) {
	// The sidecar is an upstream redis and the face is the counter, so what
	// each of them says is nothing like the other.
	cache := pairLogs(t, s, "cache")
	if !strings.Contains(strings.ToLower(cache), "redis") {
		t.Errorf("proc_logs of the cache unit answered:\n%s\nwant the output of redis", truncate(cache))
	}
	web := pairLogs(t, s, "web")
	if !strings.Contains(web, "pair/web listening") {
		t.Errorf("proc_logs of the web unit answered:\n%s\nwant the counter's own output", truncate(web))
	}
	// Without a unit the face is what is read, which is the default a member
	// asking about the Process means.
	face := pairLogs(t, s, "")
	if !strings.Contains(face, "pair/web listening") || strings.Contains(strings.ToLower(face), "redis is starting") {
		t.Errorf("proc_logs without a unit answered:\n%s\nwant the face unit's output", truncate(face))
	}

	// A unit this Process does not have is not-found naming the ones it does.
	res := s.admin.call("proc_logs", map[string]any{"id": s.pairID, "unit": "redis"})
	problem := res.mustProblem(t, "proc_logs", "not-found")
	for _, unit := range []string{"web", "cache"} {
		if !strings.Contains(problem.Fix, unit) {
			t.Errorf("the fix of an unknown unit is %q, want it to name the unit %s", problem.Fix, unit)
		}
	}

	// pkg_inspect answers what the Package is made of, which is what an agent
	// reads before it asks for a unit by name.
	var inspected struct {
		Units []struct {
			Name   string `json:"name"`
			Expose string `json:"expose"`
			Build  string `json:"build"`
			Image  string `json:"image"`
			Digest string `json:"digest"`
		} `json:"units"`
	}
	s.admin.ok("pkg_inspect", map[string]any{"path": s.pairPath}, &inspected)
	if len(inspected.Units) != 2 {
		t.Fatalf("pkg_inspect answered %d units, want the two the manifest declares: %+v", len(inspected.Units), inspected.Units)
	}
	faces := 0
	for _, unit := range inspected.Units {
		if unit.Expose != "" {
			faces++
			if unit.Name != "web" || unit.Expose != "http" {
				t.Errorf("the face is %+v, want the web unit exposed over http", unit)
			}
		}
		if unit.Digest == "" {
			t.Errorf("the unit %s carries no digest after pkg_build built it", unit.Name)
		}
	}
	if faces != 1 {
		t.Errorf("%d units of the fixture declare a face, want exactly one: %+v", faces, inspected.Units)
	}
}

// theProbeCarriesTheUnit reads the other new filter: the health probe requests
// the face and records the unit it read, so tel_query by unit answers the
// records of that unit and of nothing else.
func theProbeCarriesTheUnit(t *testing.T, s *state) {
	var metrics struct {
		Records []struct {
			Name       string `json:"name"`
			Attributes struct {
				Process string `json:"process"`
				Unit    string `json:"unit"`
			} `json:"attributes"`
		} `json:"records"`
	}
	// The probe runs on the interval the fixture declares, so the first
	// record may not be written yet when this step begins.
	deadline := time.Now().Add(probeBudget)
	for {
		s.admin.ok("tel_query", map[string]any{
			"signal":  "metrics",
			"process": s.pairID,
			"unit":    "web",
		}, &metrics)
		if len(metrics.Records) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if len(metrics.Records) == 0 {
		t.Fatalf("tel_query by unit answered no record within %s, want the health probe of the face", probeBudget)
	}
	health := 0
	for _, record := range metrics.Records {
		if record.Attributes.Unit != "web" {
			t.Errorf("a record of the unit %q came back for a query of web: %+v", record.Attributes.Unit, record)
		}
		if record.Name == "kitbash.health" {
			health++
		}
	}
	if health == 0 {
		t.Errorf("the %d records of the web unit carry no kitbash.health: %+v", len(metrics.Records), metrics.Records)
	}
	t.Logf("tel_query by unit answered %d records of the face, %d of them health probes", len(metrics.Records), health)
}

// theCacheUnitGoesDown is the acceptance sentence of M12: a member kills one
// unit's process inside the pod and proc_list says which unit is down, without
// the Process itself being gone.
func theCacheUnitGoesDown(t *testing.T, s *state) {
	pod := "kitbash-" + pairName + "-" + pairName
	if out, err := runAs(t, adminName(), "podman", "stop", "--time", "5", pod+"-cache"); err != nil {
		t.Fatalf("stopping the cache unit: %v\n%s", err, out)
	}

	var listed struct {
		Processes []struct {
			ID    string `json:"id"`
			State string `json:"state"`
			Units []struct {
				Name  string `json:"name"`
				State string `json:"state"`
			} `json:"units"`
		} `json:"processes"`
	}
	// The runtime holds the state, so the next listing is the answer; the
	// budget is for the container that is still on its way down.
	deadline := time.Now().Add(unitDownBudget)
	var process struct {
		State string
		Units map[string]string
	}
	for {
		s.admin.ok("proc_list", map[string]any{"package": s.pairPath}, &listed)
		process.Units = map[string]string{}
		process.State = ""
		for _, p := range listed.Processes {
			if p.ID != s.pairID {
				continue
			}
			process.State = p.State
			for _, unit := range p.Units {
				process.Units[unit.Name] = unit.State
			}
		}
		if process.State != "" && process.State != "running" && process.Units["cache"] != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("proc_list still reports %s with the units %+v after the cache was stopped",
				process.State, process.Units)
		}
		time.Sleep(time.Second)
	}
	if len(process.Units) != 2 {
		t.Fatalf("proc_list answers %d units for the pod, want both: %+v", len(process.Units), process.Units)
	}
	if process.Units["web"] != "running" {
		t.Errorf("the web unit is %q, want it still running while the cache is not", process.Units["web"])
	}
	t.Logf("proc_list reports the Process %s with cache %s and web %s",
		process.State, process.Units["cache"], process.Units["web"])
}

// pairLogs reads the Process's output, of the unit named or of the face when
// the name is empty.
func pairLogs(t *testing.T, s *state, unit string) string {
	t.Helper()
	args := map[string]any{"id": s.pairID}
	if unit != "" {
		args["unit"] = unit
	}
	var out struct {
		Lines []string `json:"lines"`
	}
	s.admin.ok("proc_logs", args, &out)
	return strings.Join(out.Lines, "\n")
}
