package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/zyx1121/kitbash/internal/store"
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
func probed(p store.Process) bool {
	if !p.Health.Declared() || p.Endpoint == "" {
		return false
	}
	return p.Expose == ExposeHTTP || p.Expose == ExposeMCP
}

// track starts probing one Process, or stops probing it when the registration
// no longer declares one. It is what a registration calls: one row changed, so
// one entry changes.
//
// A re-registration at the same endpoint and path keeps its schedule and its
// last reading: it is the same Process answering the same path, and resetting
// the reading would report a Process as never probed because its token was
// minted again.
func (pr *prober) track(p store.Process, now time.Time) {
	if !probed(p) {
		pr.untrack(p.ID)
		return
	}
	pr.mu.Lock()
	current, tracked := pr.targets[p.ID]
	if tracked && current.endpoint == p.Endpoint && current.path == p.Health.HTTP {
		current.owner = p.Owner
		current.pkg = p.Package
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

// load starts probing the registered Processes that declare a path, which is
// what a daemon that has just started finds in its store. It adds and never
// removes: the set is empty before this runs, so everything in it was
// registered while the daemon was already serving and is current.
func (pr *prober) load(processes []store.Process, now time.Time) int {
	tracked := 0
	for _, p := range processes {
		if !probed(p) {
			continue
		}
		tracked++
		pr.track(p, now)
	}
	return tracked
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

// finished records one reading and reports whether the Process is still
// registered. It answers false for one that was unregistered while its request
// was in flight, which is a Process kitbashd no longer runs.
func (pr *prober) finished(id string, at time.Time, healthy bool) bool {
	pr.mu.Lock()
	target, tracked := pr.targets[id]
	if tracked {
		target.running = false
		target.last = at
		target.healthy = healthy
		target.probed = true
	}
	pr.mu.Unlock()
	if tracked {
		// The next probe of this Process may be nearer than whatever the loop
		// is waiting for, because it was not counted while it was in flight.
		pr.wake()
	}
	return tracked
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
	} else if tracked := s.probes.load(list, s.now()); tracked > 0 {
		logger.Printf("health: probing %d registered Processes", tracked)
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
	if !s.probes.finished(target.id, at, healthy) {
		// The Process was unregistered while this request was in flight, so
		// the reading belongs to a registration that is gone. It is dropped
		// rather than stored: what it would say is that a Process nobody runs
		// any more is down.
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
