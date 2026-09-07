package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
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

// enqueue never blocks. A full queue loses its oldest request to make room,
// because the producer's 200 must not wait for a subscriber that is behind.
func (s *subscriber) enqueue(d delivery) {
	for {
		select {
		case s.queue <- d:
			return
		default:
		}
		select {
		case <-s.queue:
			s.dropped.Add(1)
		default:
			// Another goroutine drained it first; try to enqueue again.
		}
	}
}

// fanout holds the subscribers and delivers stored records to them.
type fanout struct {
	client *http.Client
	mu     sync.Mutex
	subs   map[string]*subscriber
	closed bool
}

// newFanout builds the fan out with the delivery timeout of the spec.
func newFanout() *fanout {
	return &fanout{
		client: &http.Client{Timeout: DeliveryTimeout},
		subs:   map[string]*subscriber{},
	}
}

// count is how many subscribers the fan out is delivering to, which health
// reports.
func (f *fanout) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

// reload makes the subscriber set match the registered Processes. It runs on
// start and whenever the processes table changes, so a Process that stops
// receiving is one that was unregistered.
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
		sub := &subscriber{
			id:       id,
			owner:    p.Owner,
			admin:    p.Admin,
			endpoint: p.Endpoint,
			queue:    make(chan delivery, QueueDepth),
			stop:     make(chan struct{}),
			done:     make(chan struct{}),
		}
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
// A subscriber never receives a record it produced itself. Without that an
// evaluation kit would judge its own judgments and a subscriber that exports
// its own spans would feed itself forever.
func (f *fanout) dispatch(e store.Export, producer string) {
	if f == nil || e.Empty() {
		return
	}
	f.mu.Lock()
	subs := make([]*subscriber, 0, len(f.subs))
	for _, sub := range f.subs {
		if sub.id == producer {
			continue
		}
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
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("the Process answered %d", res.StatusCode)
	}
	return nil
}
