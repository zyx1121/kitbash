package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// The health probe of a Process. A Package declares deploy.units[0].health.http
// and kitbashd requests that path on the Process's endpoint every interval,
// writing one metric record per probe. It restarts nothing, stops nothing and
// changes no registration: what a probe produces is Telemetry, and what is done
// about it is the owner's or a kit's, see PLAN.md section 2.4.

// HealthMetric is the name of the metric one probe writes, and
// AttrHealthStatus the wire name of the attribute carrying what it saw: the
// HTTP status as a number, or the class of the failure.
const (
	HealthMetric     = "kitbash.health"
	AttrHealthStatus = "kitbash.health.status"
)

// The values of the metric. A probe answers healthy or it does not; there is
// no third reading, and a Process nobody could reach is unhealthy rather than
// unknown.
const (
	healthyValue   = 1
	unhealthyValue = 0
)

// DefaultHealthInterval is how often a Process whose manifest names no
// interval is probed, and MinHealthInterval the floor every interval is held
// to. A manifest asking to be probed every 100 ms would spend the daemon's
// time rather than its own, so an interval below the floor is raised to it
// rather than refused: the manifest is valid, and what it asks for is not.
const (
	DefaultHealthInterval = 30 * time.Second
	MinHealthInterval     = 5 * time.Second
)

// HealthTimeout bounds one probe, request and response together. A Process
// that has not answered in five seconds is not answering.
const HealthTimeout = 5 * time.Second

// HealthIdleWait is how long the loop waits when nothing is due, which is what
// a host whose Processes declare no probe spends its time on. A registration
// wakes the loop, so this is a ceiling and not a delay.
const HealthIdleWait = time.Minute

// HealthWriteTimeout bounds the write of one probe's record.
const HealthWriteTimeout = 5 * time.Second

// The classes a failed probe records in place of an HTTP status. A probe that
// timed out and one that found nothing listening are different problems for
// whoever reads the records, so they are not one word.
const (
	healthTimedOut    = "timeout"
	healthUnreachable = "unreachable"
)

// probe is one Process kitbashd probes: what to request, how often, and what
// the last request saw.
type probe struct {
	id       string
	owner    string
	pkg      string
	endpoint string
	path     string
	interval time.Duration

	// next is when this Process is due and running whether a request for it
	// is in flight. A probe that outlives its interval is waited for rather
	// than sent again, so a Process that answers slowly is not probed twice
	// at once.
	next    time.Time
	running bool

	// last, healthy and probed are the most recent reading, which
	// processes_list carries and proc_list publishes. They live here and
	// nowhere else: the store keeps records, not the latest value of one, and
	// a daemon that has just started has no reading to report.
	last    time.Time
	healthy bool
	probed  bool
}

// prober holds what is probed and the client that probes it. The Processes it
// holds are the registered ones that declare a path, tracked as registrations
// arrive and dropped as they go.
type prober struct {
	client      *http.Client
	minInterval time.Duration

	mu      sync.Mutex
	targets map[string]*probe

	// changed wakes the loop when the set of Processes changed, so a Process
	// registered now is probed now rather than when the current wait runs
	// out. It is buffered and never blocks: one wake up is as good as three.
	changed chan struct{}
}

// newProber builds the prober with the probe timeout and a client that cannot
// be talked off the host.
//
// kitbashd runs as root and a probe is a request it makes on behalf of nobody,
// so the two rules the fan out follows apply here as well. A redirect is not
// followed, so a Process cannot answer 307 and have the daemon request
// something else. And every connection is refused unless it lands on a
// loopback address, which re-checks at connect time what registration checked
// at write time.
func newProber(minInterval time.Duration) *prober {
	if minInterval <= 0 {
		minInterval = MinHealthInterval
	}
	return &prober{
		client: &http.Client{
			Timeout: HealthTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DialContext:           dialLoopback,
				MaxIdleConnsPerHost:   1,
				IdleConnTimeout:       IdleSubscriberTimeout,
				ResponseHeaderTimeout: HealthTimeout,
			},
		},
		minInterval: minInterval,
		targets:     map[string]*probe{},
		changed:     make(chan struct{}, 1),
	}
}

// probed reports whether one registration is something kitbashd probes: a
// Process that declares a path, publishes an endpoint on this host, and is
// exposed over HTTP or as an MCP server. A Process exposed as none publishes
// nothing to request.
//
// A Process a run kit owns is never probed. There is no container of it on
// this host, so there is nothing to check its endpoint against, and kitbashd
// does not supervise it at all: the kit that started it is the one that knows
// whether it is up, see PLAN.md section 3.
func probed(p store.Process) bool {
	if p.Runner != "" {
		return false
	}
	if !p.Health.Declared() || p.Endpoint == "" {
		return false
	}
	return p.Expose == ExposeHTTP || p.Expose == ExposeMCP
}

// track starts probing one Process, or stops probing it when the registration
// no longer declares one. It is what a registration calls: one row changed, so
// one entry changes.
//
// A Process this prober already holds keeps its schedule and its in flight
// marker, whatever the registration changed. That is the guarantee: one
// Process is requested at most once per interval and has at most one request
// in flight, however often it is registered again. A re-registration that
// moved the endpoint or the path is therefore probed at the next due time
// rather than at once, and the reading of a request already in flight for the
// declaration it replaced is dropped, see finished.
//
// It keeps the last reading as well: resetting it would report a Process as
// never probed because its token was minted again.
func (pr *prober) track(p store.Process, now time.Time) {
	if !probed(p) {
		pr.untrack(p.ID)
		return
	}
	pr.mu.Lock()
	current, tracked := pr.targets[p.ID]
	if tracked {
		current.owner = p.Owner
		current.pkg = p.Package
		current.endpoint = p.Endpoint
		current.path = p.Health.HTTP
		current.interval = pr.every(p.Health.Interval)
		pr.mu.Unlock()
		return
	}
	// A Process kitbashd has not probed yet is due at once: the first reading
	// of a Process that has just been registered is the one its owner is
	// waiting for.
	pr.targets[p.ID] = &probe{
		id:       p.ID,
		owner:    p.Owner,
		pkg:      p.Package,
		endpoint: p.Endpoint,
		path:     p.Health.HTTP,
		interval: pr.every(p.Health.Interval),
		next:     now,
	}
	pr.mu.Unlock()
	pr.wake()
}

// untrack stops probing one Process, which is what unregistering it does. A
// request already in flight for it is answered to nobody, see probeOnce.
func (pr *prober) untrack(id string) {
	pr.mu.Lock()
	_, tracked := pr.targets[id]
	delete(pr.targets, id)
	pr.mu.Unlock()
	if tracked {
		pr.wake()
	}
}

// verifyProbe reports whether the endpoint a registration names is a port the
// Process's own container publishes, which is what kitbashd probes and the
// only thing it probes.
//
// The daemon runs as root and a probe is a GET it makes on a member's word. If
// the endpoint were the member's to choose, a registration would turn kitbashd
// into a loopback port scanner: the status of any port on the host would come
// back through kitbash.health.status, and any neighbour's Process could be
// requested on a schedule. So the port is not taken from the registration but
// read off the container, as the owner, before anything is probed. What the
// container publishes is what the kernel let that member bind, so a port
// another member holds is never in this list.
//
// A container the runtime does not have yet is not a refusal: proc_run
// registers the Process before the container is created, so the registration
// is accepted and probed by nothing until the start that creates the container
// checks again. A runtime that will not answer is a refusal: kitbashd probes
// what it has checked, and nothing it has not.
func (s *Server) verifyProbe(ctx context.Context, m sysusers.Member, p store.Process, instance string) (bool, *problem.Problem) {
	if p.Runner != "" {
		// A run kit owns this Process, so there is no container of it here
		// and nothing to check an endpoint against. A declared probe is
		// refused rather than ignored: the Package asked for something
		// kitbashd cannot do for a Process it does not supervise.
		if p.Health.Declared() {
			return false, problem.NotPermitted(instance,
				"a Process a run kit owns is not probed by kitbashd",
				fmt.Sprintf("Remove deploy.units[0].health from this Package, or have %s probe the Process it runs.", p.Runner))
		}
		return false, nil
	}
	if !probed(p) {
		return false, nil
	}
	port, ok := endpointPort(p.Endpoint)
	if !ok {
		return false, problem.BadRequest(instance,
			fmt.Sprintf("%q does not name a port to probe", p.Endpoint),
			healthFix)
	}
	if p.Container == "" {
		return false, problem.NotPermitted(instance,
			"a health probe is checked against the Process's own container, and this registration names none",
			"Register the Process with the container name the runtime holds it under, then declare health.http.")
	}
	config, err := s.runner.ContainerConfig(ctx, m, p.Container)
	if errors.Is(err, sysusers.ErrNoContainer) {
		return false, nil
	}
	if err != nil {
		return false, problem.Internal(instance,
			fmt.Sprintf("reading the configuration of %s: %v", p.Container, err), "")
	}
	for _, published := range config.Publish {
		if published.HostPort == port {
			return true, nil
		}
	}
	return false, problem.NotPermitted(instance,
		fmt.Sprintf("the endpoint of this registration names port %d, which %s does not publish",
			port, p.Container),
		fmt.Sprintf("health.http probes the Process's own published port; the endpoint names port %d, which this container does not publish.", port))
}

// trackProbe verifies one Process's declaration and points the prober at it,
// or leaves it unprobed. It answers the problem the caller reports; a caller
// that cannot refuse anything any more logs it, see startProcess.
func (s *Server) trackProbe(ctx context.Context, p store.Process, instance string) *problem.Problem {
	if !probed(p) {
		s.probes.untrack(p.ID)
		return nil
	}
	m, found, err := s.users.Lookup(ctx, p.Owner)
	if err != nil || !found {
		s.probes.untrack(p.ID)
		return problem.Internal(instance,
			fmt.Sprintf("%s owns the Process %s and could not be looked up: %v", p.Owner, p.ID, err), "")
	}
	return s.trackProbeAs(ctx, m, p, instance)
}

// trackProbeAs is trackProbe for a caller that has already looked the owner
// up, which every start has.
func (s *Server) trackProbeAs(ctx context.Context, m sysusers.Member, p store.Process, instance string) *problem.Problem {
	verified, prob := s.verifyProbe(ctx, m, p, instance)
	if !verified {
		// A Process whose declaration cannot be checked is not probed, and one
		// that was probed under a declaration that no longer checks out stops
		// being probed.
		s.probes.untrack(p.ID)
		return prob
	}
	s.probes.track(p, s.now())
	return nil
}

// endpointPort reads the port of a registered endpoint. Registration has
// already held it to http://127.0.0.1:PORT, see validateEndpoint.
func endpointPort(endpoint string) (int, bool) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return 0, false
	}
	_, port, err := splitHostPort(u.Host)
	if err != nil {
		return 0, false
	}
	number, err := strconv.Atoi(port)
	if err != nil || number <= 0 {
		return 0, false
	}
	return number, true
}

// loadProbes starts probing the registered Processes whose declaration checks
// out, which is what a daemon that has just started finds in its store. It
// adds and never removes: the set is empty before this runs, so everything in
// it was registered while the daemon was already serving and is current.
//
// It runs after restore, so the containers it checks the endpoints against are
// the ones that came back. A Process whose declaration does not check out is
// logged and not probed: nothing about it is a member's to be told again here,
// and the next start of that Process refuses it to their face.
func (s *Server) loadProbes(ctx context.Context, processes []store.Process) {
	for _, p := range processes {
		if !probed(p) {
			continue
		}
		if prob := s.trackProbe(ctx, p, healthPath); prob != nil {
			logger.Printf("health: not probing %s of %s: %s", p.ID, p.Owner, prob.Detail)
		}
	}
}

// every reads the interval a manifest spells, held to the floor. A spelling
// this daemon cannot read is the default rather than a refusal: the
// registration was accepted, and a Process probed every thirty seconds is
// closer to what the Package asked for than one probed never.
func (pr *prober) every(spelling string) time.Duration {
	if spelling == "" {
		return DefaultHealthInterval
	}
	interval, err := time.ParseDuration(spelling)
	if err != nil || interval <= 0 {
		return DefaultHealthInterval
	}
	if interval < pr.minInterval {
		return pr.minInterval
	}
	return interval
}

// due takes the Processes that are due now, marks each one in flight and
// schedules its next probe. It also answers how long the loop may wait before
// it looks again, which is the nearest due time or HealthIdleWait.
func (pr *prober) due(now time.Time) ([]probe, time.Duration) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	var due []probe
	wait := HealthIdleWait
	for _, target := range pr.targets {
		if target.running {
			continue
		}
		if !target.next.After(now) {
			target.running = true
			target.next = now.Add(target.interval)
			due = append(due, *target)
			continue
		}
		if left := target.next.Sub(now); left < wait {
			wait = left
		}
	}
	return due, wait
}

// finished records one reading and reports whether it counts. It answers false
// for a Process that was unregistered while its request was in flight, which
// is a Process kitbashd no longer runs, and for one whose declaration was
// replaced while the request was in flight, which is a reading about an
// endpoint the Process no longer names. Either way the Process stops being in
// flight, so the next interval probes it again.
func (pr *prober) finished(sent probe, at time.Time, healthy bool) bool {
	pr.mu.Lock()
	current, tracked := pr.targets[sent.id]
	sameDeclaration := tracked &&
		current.endpoint == sent.endpoint && current.path == sent.path
	if tracked {
		current.running = false
	}
	if sameDeclaration {
		current.last = at
		current.healthy = healthy
		current.probed = true
	}
	pr.mu.Unlock()
	if tracked {
		// The next probe of this Process may be nearer than whatever the loop
		// is waiting for, because it was not counted while it was in flight.
		pr.wake()
	}
	return sameDeclaration
}

// reading is the most recent probe of one Process, for processes_list.
func (pr *prober) reading(id string) (time.Time, bool, bool) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	target, tracked := pr.targets[id]
	if !tracked || !target.probed {
		return time.Time{}, false, false
	}
	return target.last, target.healthy, true
}

// wake tells the loop to look again. A wake up that is already pending is
// enough, so this never blocks and never grows a queue.
func (pr *prober) wake() {
	select {
	case pr.changed <- struct{}{}:
	default:
	}
}

// count is how many Processes are being probed, which health reports.
func (pr *prober) count() int {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return len(pr.targets)
}

// HealthLoop probes every registered Process that declares a path until ctx is
// done. kitbashd starts it after restore: a Process that is coming back would
// otherwise be probed while its container is still starting, and the first
// record of the day would say unhealthy about a host that is merely booting.
//
// It is one goroutine. Each probe runs on its own, so a Process that answers
// slowly holds up nothing, and the loop sleeps until the nearest one is due or
// a registration wakes it.
func (s *Server) HealthLoop(ctx context.Context) {
	list, err := s.store.Processes(ctx, "")
	if err != nil {
		logger.Printf("health: could not read the registered Processes: %v", err)
	} else {
		s.loadProbes(ctx, list)
		if tracked := s.probes.count(); tracked > 0 {
			logger.Printf("health: probing %d registered Processes", tracked)
		}
	}
	for {
		due, wait := s.probes.due(s.now())
		for _, target := range due {
			go s.probeOnce(ctx, target)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.stopped:
			timer.Stop()
			return
		case <-s.probes.changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// probeOnce requests one path and records what it saw.
func (s *Server) probeOnce(ctx context.Context, target probe) {
	healthy, status := s.request(ctx, target)
	at := s.now()
	if !s.probes.finished(target, at, healthy) {
		// The Process was unregistered, or its declaration was replaced,
		// while this request was in flight. The reading belongs to something
		// that is no longer registered, so it is dropped rather than stored:
		// what it would say is that a Process nobody runs any more, or an
		// endpoint nobody named any more, is down.
		return
	}
	s.recordHealth(ctx, target, healthy, status, at)
}

// request runs one probe and reports whether the Process is healthy and what
// the attempt saw. Any 2xx is healthy; everything else, a status this daemon
// did not ask about included, is not.
func (s *Server) request(ctx context.Context, target probe) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, HealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.endpoint+target.path, nil)
	if err != nil {
		return false, healthUnreachable
	}
	res, err := s.probes.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return false, healthTimedOut
		}
		var timeout interface{ Timeout() bool }
		if errors.As(err, &timeout) && timeout.Timeout() {
			return false, healthTimedOut
		}
		return false, healthUnreachable
	}
	defer res.Body.Close()
	// Enough of the body is drained for the connection to be reused. A health
	// endpoint answers a word; whatever more it sends is not read.
	io.Copy(io.Discard, io.LimitReader(res.Body, DiscardLimit))
	healthy := res.StatusCode >= http.StatusOK && res.StatusCode < http.StatusMultipleChoices
	return healthy, strconv.Itoa(res.StatusCode)
}

// recordHealth stores one probe as a metric and hands it to the fan out, which
// is the path every stored record takes: a subscriber sees a probe the same
// way it sees anything else, and tel_query answers it to the Process's owner.
//
// The record carries the four attributes PLAN.md section 2.4 requires. The
// user is the member who owns the Process, because a probe is about their
// Process; the producer is kitbashd, because a probe is not something the
// Process said about itself.
func (s *Server) recordHealth(ctx context.Context, target probe, healthy bool, status string, at time.Time) {
	value := float64(unhealthyValue)
	if healthy {
		value = healthyValue
	}
	export := store.Export{Metrics: []store.Metric{{
		TimeNS: at.UnixNano(),
		Name:   HealthMetric,
		Value:  value,
		Attributes: store.Attributes{
			User:     target.owner,
			Package:  target.pkg,
			Process:  target.id,
			Path:     target.path,
			Producer: InternalProducer,
			Other:    map[string]any{AttrHealthStatus: status},
		},
	}}}
	write, cancel := context.WithTimeout(ctx, HealthWriteTimeout)
	defer cancel()
	if err := s.store.Insert(write, export); err != nil {
		// A daemon that is stopping cancelled this write itself, which is not
		// a failure to report: the probe is over and so is the store.
		if ctx.Err() == nil {
			logger.Printf("health: could not record the probe of %s: %v", target.id, err)
		}
		return
	}
	s.fanout.dispatch(export, InternalProducer)
}

// withReadings fills in the most recent reading of every Process in one
// listing.
// The declaration comes from the store and the reading from the prober, so a
// Process registered before this daemon started answers with a declaration and
// no reading until it has been probed once.
func (s *Server) withReadings(list []store.Process) []store.Process {
	for i := range list {
		if !list[i].Health.Declared() {
			continue
		}
		last, healthy, probed := s.probes.reading(list[i].ID)
		if !probed {
			continue
		}
		list[i].Health.Last = last.UTC().Format(time.RFC3339Nano)
		list[i].Health.Healthy = &healthy
	}
	return list
}

// healthFix is what a Package author does about a probe this daemon refuses.
var healthFix = fmt.Sprintf(
	"Declare deploy.units[0].health.http as the path to request, such as /healthz, and health.interval as a duration such as 30s; an interval under %s is raised to it.",
	MinHealthInterval)
