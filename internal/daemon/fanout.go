package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zyx1121/kitbash/internal/otlp"
	"github.com/zyx1121/kitbash/internal/store"
)

// QueueDepth is how many requests wait for one subscriber. Beyond it the
// oldest is dropped, because a subscriber that has fallen a thousand requests
// behind wants the newest records, and the store still holds them all, see
// spec/kitbashd-api.yaml.
const QueueDepth = 1024

// QueueBytes is how much a subscriber's queue may hold. A request may carry up
// to otlp.MaxBodyBytes, so a depth of 1024 alone would let one subscriber that
// stopped answering pin gigabytes of the daemon's memory. Whichever bound is
// reached first drops the oldest request.
const QueueBytes = 64 << 20

// DeliveryTimeout bounds one POST to a subscriber. There is no retry: the push
// is a wake up and the store is the source of truth.
const DeliveryTimeout = 5 * time.Second

// DiscardLimit is how much of a subscriber's answer is drained so the
// connection can be reused. A Process answering an OTLP export sends an empty
// object; anything larger is not worth reading.
const DiscardLimit = 4 << 10

// delivery is one encoded export request waiting for one subscriber.
type delivery struct {
	// path is the standard OTLP path of the signal, appended to the endpoint.
	path string
	// body is the OTLP/HTTP JSON export request.
	body []byte
}

// subscriber is one registered Process that asked for the fan out: its queue,
// its worker, and what it has missed.
type subscriber struct {
	id       string
	owner    string
	admin    bool
	endpoint string
	queue    chan delivery
	dropped  atomic.Int64
	// held is how many bytes the queue is carrying. It is the second bound on
	// the queue and it is kept by whoever puts a request in or takes one out.
	held atomic.Int64
	// failing is read and written only by the worker.
	failing bool
	// stop is closed when this subscriber is unregistered, and done when its
	// worker has left. The queue itself is never closed: a producer that took
	// a snapshot of the subscribers may still be enqueueing, and a send on a
	// closed channel would take the daemon down with it.
	stop chan struct{}
	done chan struct{}
}

// eligible reports whether this subscriber receives a record about user. An
// admin's subscriber sees the whole machine, a member's only their own, the
// same rule tel_query follows, see PLAN.md section 2.4.
func (s *subscriber) eligible(user string) bool {
	return s.admin || s.owner == user
}

// enqueue never blocks. A queue that is full, by count or by bytes, loses its
// oldest requests to make room, because the producer's 200 must not wait for a
// subscriber that is behind.
//
// A request larger than the whole budget would empty the queue and still not
// fit, so it is dropped rather than allowed to evict everything for nothing.
func (s *subscriber) enqueue(d delivery) {
	size := int64(len(d.body))
	for {
		if s.held.Load()+size <= QueueBytes {
			select {
			case s.queue <- d:
				s.held.Add(size)
				return
			default:
			}
		}
		select {
		case old := <-s.queue:
			s.held.Add(-int64(len(old.body)))
			s.dropped.Add(1)
		default:
			// Nothing left to evict. If the budget has room now, the worker
			// emptied the queue between the two checks and this request goes
			// in on the next turn; if it still does not fit, it never will.
			if s.held.Load()+size <= QueueBytes {
				continue
			}
			s.dropped.Add(1)
			return
		}
	}
}

// release gives the bytes of one request back to the budget. The worker calls
// it for every request it takes off the queue.
func (s *subscriber) release(d delivery) {
	s.held.Add(-int64(len(d.body)))
}

// fanout holds the subscribers and delivers stored records to them.
type fanout struct {
	client *http.Client
	mu     sync.Mutex
	subs   map[string]*subscriber
	closed bool
}

// newFanout builds the fan out with the delivery timeout of the spec and a
// client that cannot be talked off the host.
//
// kitbashd runs as root and delivers other members' Telemetry, so a subscriber
// is a place records go, never a place that decides where they go next. Two
// rules enforce that. A redirect is not followed, so a member's Process cannot
// answer 307 and have the daemon replay the records somewhere else. And every
// connection is refused unless it lands on a loopback address, which re-checks
// at connect time what registration checked at write time: a store row that
// was tampered with, or an endpoint that resolves off the host, never reaches
// the network.
func newFanout() *fanout {
	return &fanout{
		client: &http.Client{
			Timeout: DeliveryTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DialContext:           dialLoopback,
				MaxIdleConnsPerHost:   2,
				IdleConnTimeout:       IdleSubscriberTimeout,
				TLSHandshakeTimeout:   DeliveryTimeout,
				ResponseHeaderTimeout: DeliveryTimeout,
			},
		},
		subs: map[string]*subscriber{},
	}
}

// IdleSubscriberTimeout is how long a connection to a subscriber is kept for
// the next delivery.
const IdleSubscriberTimeout = 30 * time.Second

// dialLoopback connects only to the loopback address, and only over TCP. A
// host name is resolved first and refused unless every address it answers with
// is loopback, so a name that resolves off the host is refused rather than
// dialled once and noticed later.
func dialLoopback(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("daemon: the fan out does not deliver over %s", network)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("daemon: %q is not an address the fan out delivers to", address)
	}
	resolved, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("daemon: %q is not an address the fan out delivers to", address)
	}
	for _, addr := range resolved {
		if !addr.Unmap().IsLoopback() {
			return nil, fmt.Errorf("daemon: the fan out delivers to the loopback address only, and %q is %s",
				host, addr.Unmap())
		}
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("daemon: %q resolves to no address", host)
	}
	dialer := net.Dialer{Timeout: DeliveryTimeout}
	return dialer.DialContext(ctx, network, net.JoinHostPort(resolved[0].String(), port))
}

// count is how many subscribers the fan out is delivering to, which health
// reports.
func (f *fanout) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

// track adds or replaces the subscriber of one Process, and is what a
// registration calls: one row changed, so one entry changes. A Process that
// declares no subscription, or no endpoint, is removed instead.
func (f *fanout) track(p store.Process) {
	if !p.Subscribes() || p.Endpoint == "" {
		f.untrack(p.ID)
		return
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	old, existed := f.subs[p.ID]
	if existed && old.endpoint == p.Endpoint && old.owner == p.Owner && old.admin == p.Admin {
		f.mu.Unlock()
		return
	}
	sub := newSubscriber(p)
	f.subs[p.ID] = sub
	f.mu.Unlock()

	if existed {
		close(old.stop)
	}
	go f.work(sub)
}

// untrack removes one subscriber, which is what unregistering a Process does.
func (f *fanout) untrack(id string) {
	f.mu.Lock()
	sub, existed := f.subs[id]
	if existed {
		delete(f.subs, id)
	}
	f.mu.Unlock()
	if existed {
		close(sub.stop)
	}
}

// newSubscriber is one Process's queue and the worker that drains it.
func newSubscriber(p store.Process) *subscriber {
	return &subscriber{
		id:       p.ID,
		owner:    p.Owner,
		admin:    p.Admin,
		endpoint: p.Endpoint,
		queue:    make(chan delivery, QueueDepth),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// reload makes the subscriber set match the registered Processes. It runs on
// start, where the whole table is what the daemon has to go on; a single
// registration calls track instead.
//
// A subscriber whose endpoint changed is replaced rather than updated: the
// queue belongs to an endpoint, and records queued for the old one are not
// for the new.
func (f *fanout) reload(processes []store.Process) {
	wanted := map[string]store.Process{}
	for _, p := range processes {
		if !p.Subscribes() || p.Endpoint == "" {
			continue
		}
		wanted[p.ID] = p
	}

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	var stopped []*subscriber
	for id, sub := range f.subs {
		p, keep := wanted[id]
		if keep && p.Endpoint == sub.endpoint && p.Owner == sub.owner && p.Admin == sub.admin {
			delete(wanted, id)
			continue
		}
		delete(f.subs, id)
		stopped = append(stopped, sub)
	}
	var started []*subscriber
	for id, p := range wanted {
		sub := newSubscriber(p)
		f.subs[id] = sub
		started = append(started, sub)
	}
	f.mu.Unlock()

	for _, sub := range stopped {
		close(sub.stop)
	}
	for _, sub := range started {
		go f.work(sub)
	}
}

// close stops every worker. The daemon owns the fan out, so this is what its
// own Close calls.
func (f *fanout) close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	subs := make([]*subscriber, 0, len(f.subs))
	for id, sub := range f.subs {
		subs = append(subs, sub)
		delete(f.subs, id)
	}
	f.mu.Unlock()
	for _, sub := range subs {
		close(sub.stop)
	}
	for _, sub := range subs {
		<-sub.done
	}
}

// dispatch encodes one stored export and queues it for every eligible
// subscriber. It runs on the producer's goroutine and never blocks: encoding
// is bounded by the body the producer already sent, and enqueueing drops
// rather than waits.
//
// A record produced by a subscriber is not fanned out at all. The obvious rule,
// not delivering a record back to the subscriber that produced it, only stops a
// loop one hop long: with two kits subscribed, each one's records wake the
// other, whose records wake the first, and the pair amplifies for as long as
// they run. Stopping at the first hop is the only rule that terminates without
// kitbashd knowing what a kit does with what it receives.
//
// What a kit writes is therefore in the store and answered by tel_query, but it
// is not pushed. That is the documented contract: the fan out is best effort
// and the store is the source of truth, see PLAN.md section 5.5.
func (f *fanout) dispatch(e store.Export, producer string) {
	if f == nil || e.Empty() {
		return
	}
	f.mu.Lock()
	if _, producedByASubscriber := f.subs[producer]; producedByASubscriber {
		f.mu.Unlock()
		return
	}
	subs := make([]*subscriber, 0, len(f.subs))
	for _, sub := range f.subs {
		subs = append(subs, sub)
	}
	f.mu.Unlock()
	if len(subs) == 0 {
		return
	}

	// One body per set of records, so two admins' subscribers share the
	// encoding of the same export.
	bodies := map[string][]byte{}
	for _, sub := range subs {
		for _, part := range parts(e, sub) {
			body, ok := bodies[part.key]
			if !ok {
				encoded, err := part.encode()
				if err != nil {
					logger.Printf("fan out could not encode %s for %s: %v", part.path, sub.id, err)
					bodies[part.key] = nil
					continue
				}
				body = encoded
				bodies[part.key] = body
			}
			if body == nil {
				continue
			}
			sub.enqueue(delivery{path: part.path, body: body})
		}
	}
}

// part is the records of one signal that one subscriber is eligible for, with
// the key that lets two subscribers share an encoding.
type part struct {
	path   string
	key    string
	encode func() ([]byte, error)
}

// parts selects the records of an export a subscriber may see and pairs them
// with the standard path they go to.
func parts(e store.Export, sub *subscriber) []part {
	scope := sub.owner
	if sub.admin {
		scope = "*"
	}
	var out []part

	spans := make([]store.Span, 0, len(e.Spans))
	for _, sp := range e.Spans {
		if sub.eligible(sp.User) {
			spans = append(spans, sp)
		}
	}
	if len(spans) > 0 {
		out = append(out, part{
			path:   pathTraces,
			key:    scope + " " + pathTraces,
			encode: func() ([]byte, error) { return otlp.EncodeTraces(spans) },
		})
	}

	logs := make([]store.Log, 0, len(e.Logs))
	for _, l := range e.Logs {
		if sub.eligible(l.User) {
			logs = append(logs, l)
		}
	}
	if len(logs) > 0 {
		out = append(out, part{
			path:   pathLogs,
			key:    scope + " " + pathLogs,
			encode: func() ([]byte, error) { return otlp.EncodeLogs(logs) },
		})
	}

	metrics := make([]store.Metric, 0, len(e.Metrics))
	for _, m := range e.Metrics {
		if sub.eligible(m.User) {
			metrics = append(metrics, m)
		}
	}
	if len(metrics) > 0 {
		out = append(out, part{
			path:   pathMetrics,
			key:    scope + " " + pathMetrics,
			encode: func() ([]byte, error) { return otlp.EncodeMetrics(metrics) },
		})
	}
	return out
}

// work is one subscriber's goroutine: one request at a time, in the order they
// were queued, until the subscriber is unregistered.
//
// A subscriber that starts or stops failing is logged once, with what it has
// missed so far. Logging every failure would fill the log with one broken
// container's noise.
func (f *fanout) work(sub *subscriber) {
	defer close(sub.done)
	// A stopped subscriber cancels whatever it is waiting on, so unregistering
	// a Process that hung does not wait out the delivery timeout.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-sub.stop
		cancel()
	}()
	for {
		var d delivery
		select {
		case <-sub.stop:
			return
		case d = <-sub.queue:
			sub.release(d)
		}
		err := f.post(ctx, sub, d)
		switch {
		case err != nil && !sub.failing:
			sub.failing = true
			logger.Printf("fan out to the Process %s at %s is failing: %v; %d requests dropped so far",
				sub.id, sub.endpoint, err, sub.dropped.Load())
		case err == nil && sub.failing:
			sub.failing = false
			logger.Printf("fan out to the Process %s at %s is delivering again; %d requests were dropped",
				sub.id, sub.endpoint, sub.dropped.Load())
		}
	}
}

// post delivers one request. Anything but a 2xx is a failure, and there is no
// retry: the subscriber reads what it missed from the store.
func (f *fanout) post(ctx context.Context, sub *subscriber, d delivery) error {
	ctx, cancel := context.WithTimeout(ctx, DeliveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.endpoint+d.path, bytes.NewReader(d.body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", otlp.ContentTypeJSON)
	res, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	// The body is drained so the connection can be reused for the next
	// request rather than torn down after every one.
	io.Copy(io.Discard, io.LimitReader(res.Body, DiscardLimit))
	// A redirect is a refusal here, not a hop: the client is told to hand it
	// back rather than follow it, and 3xx falls into this same branch.
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("the Process answered %d", res.StatusCode)
	}
	return nil
}
