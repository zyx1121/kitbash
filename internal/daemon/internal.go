package daemon

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
)

// notInternal is the value a query filters with for every caller who is not an
// admin: the records that are not the cause of an internal problem. It is a
// package level variable because store.Filter takes a pointer, and nothing
// ever writes to it.
var notInternal = false

// isInternal is the other side of it, what a recorded cause carries.
var isInternal = true

// InternalProducer is the producer and the user of a cause kitbashd recorded
// for itself. The daemon serves every member, so a failure inside it is not a
// member's record even when it happened while serving one; an admin finds it
// by the instance the member quotes.
const InternalProducer = "kitbashd"

// internalSeverity is the severity of a recorded cause. Every one of them is
// a failure inside kitbash.
const internalSeverity = "ERROR"

// attrError is the wire name of the error class attribute, spelled here rather
// than imported from internal/telemetry: that package is the producer side and
// carries the OpenTelemetry SDK, which kitbashd does not link.
const attrError = "kitbash.error"

// InternalCauseLimit bounds one cause. A cause is an error message, and one
// that arrived from a command's standard error is as long as that command felt
// like being. Whoever sends a longer one gets it cut, not refused: the point
// of the record is the first lines of it.
const InternalCauseLimit = 8 << 10

// InternalRate and InternalRateWindow bound how many causes one peer may
// record. A session recording sixty failures a minute is a session in a loop,
// and the store is not the place to keep the evidence of it.
const (
	InternalRate       = 60
	InternalRateWindow = time.Minute
)

// InternalQueue is how many causes wait to be written. Recording one never
// holds up the request that failed, so the writes are queued and a queue that
// is full drops rather than blocks: the caller already has its problem, and
// the daemon has other work.
const InternalQueue = 256

// InternalWriteTimeout bounds the write of one cause.
const InternalWriteTimeout = 5 * time.Second

// internalCause is one queued record: who it is about, who wrote it, the
// problem instance the caller was given, and the cause itself.
type internalCause struct {
	user     string
	producer string
	instance string
	cause    string
	tool     string
	at       time.Time
}

// internalRequest is the body of POST /kitbash/v1/internal, which is how
// kitbash-mcp reports a cause: it has no way to write the store, and no
// producer may assert kitbash.internal over OTLP, see PLAN.md section 2.4.
type internalRequest struct {
	Instance string `json:"instance,omitempty"`
	Cause    string `json:"cause"`
	Tool     string `json:"tool,omitempty"`
}

// RecordInternalCauses sends the causes raised inside this process to the
// store as well as to the log. kitbash-mcp reports its own over the socket;
// this is the daemon's own, which it writes directly because it is the thing
// holding the store.
//
// It returns the function that stops recording, which the caller runs before
// the store is closed.
func (s *Server) RecordInternalCauses() func() {
	problem.OnInternal(s.recordInternal)
	return func() { problem.OnInternal(nil) }
}

// recordInternal queues one cause of the daemon's own. It never writes here:
// this runs on the goroutine of the request that failed, and that request is
// already answering.
func (s *Server) recordInternal(instance, cause string) {
	s.queueInternal(internalCause{
		user:     InternalProducer,
		producer: InternalProducer,
		instance: instance,
		cause:    cause,
		at:       s.now(),
	})
}

// queueInternal puts one cause on the queue, or drops it and says so. Nothing
// in kitbash waits for a cause to be stored.
func (s *Server) queueInternal(c internalCause) {
	c.cause = clipCause(c.cause, InternalCauseLimit)
	select {
	case s.internalCauses <- c:
	default:
		logger.Printf("dropped the internal cause at %s: the queue is full", c.instance)
	}
}

// internalWriter is the one goroutine that writes causes, started with the
// server and ended by Close.
func (s *Server) internalWriter() {
	for {
		select {
		case <-s.stopped:
			return
		case c := <-s.internalCauses:
			s.writeInternal(c)
		}
	}
}

// writeInternal stores one cause as a log record. The instance is kept as
// kitbash.path, which is what a member quotes to an admin and what the admin
// queries by, and the body is the cause itself: host paths, git output,
// whatever failed.
//
// A cause that cannot be stored goes to the log alone. Nothing here reports an
// internal problem, because this is what an internal problem calls.
func (s *Server) writeInternal(c internalCause) {
	other := map[string]any{attrError: problem.SlugInternal}
	record := store.Log{
		TimeNS:   c.at.UnixNano(),
		Severity: internalSeverity,
		Body:     c.cause,
		Attributes: store.Attributes{
			User:     c.user,
			Producer: c.producer,
			Path:     c.instance,
			Tool:     c.tool,
			Internal: &isInternal,
			Other:    other,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), InternalWriteTimeout)
	defer cancel()
	if err := s.store.Insert(ctx, store.Export{Logs: []store.Log{record}}); err != nil {
		logger.Printf("could not record the internal cause at %s: %v", c.instance, err)
	}
}

// internal answers POST /kitbash/v1/internal. The caller is the peer, member
// or admin: a session records the causes of its own calls, and kitbashd
// stamps the record from the connection exactly as it stamps an export. What
// the body carries is the cause, never who it belongs to.
func (s *Server) internal(w http.ResponseWriter, r *http.Request) {
	caller, prob := s.caller(r)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	var req internalRequest
	if prob := decodeBody(w, r, &req); prob != nil {
		writeProblem(w, prob)
		return
	}
	if strings.TrimSpace(req.Cause) == "" {
		writeProblem(w, problem.BadRequest(r.URL.Path, "the request carries no cause",
			"Send the cause of the internal problem as cause, and the problem instance as instance."))
		return
	}
	if !s.internalRate.allow(caller.User, s.now()) {
		writeProblem(w, problem.TooMany("Too many causes", r.URL.Path,
			fmt.Sprintf("%s has recorded %d internal causes in the last %s", caller.User, InternalRate, InternalRateWindow),
			"Record the causes of failures, not of every call; the ones over the limit are dropped."))
		return
	}
	s.queueInternal(internalCause{
		user:     caller.User,
		producer: caller.User,
		instance: req.Instance,
		cause:    req.Cause,
		tool:     req.Tool,
		at:       s.now(),
	})
	w.WriteHeader(http.StatusNoContent)
}

// rateLimiter counts what one peer did in a window. It is the only limiter in
// the daemon that is per caller rather than per connection, because the cost
// it bounds is store rows and not file descriptors.
type rateLimiter struct {
	limit  int
	window time.Duration

	mu    sync.Mutex
	peers map[string]rateWindow
}

// rateWindow is one peer's count and when it resets.
type rateWindow struct {
	count int
	until time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, peers: map[string]rateWindow{}}
}

// allow reports whether this peer may do one more, and counts it when it may.
func (l *rateLimiter) allow(peer string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	current, seen := l.peers[peer]
	if !seen || !now.Before(current.until) {
		// A peer whose window has passed starts a new one, which is also
		// where a host with many members stops the map from growing: an entry
		// nobody has used since its window ended is dropped below.
		l.prune(now)
		l.peers[peer] = rateWindow{count: 1, until: now.Add(l.window)}
		return true
	}
	if current.count >= l.limit {
		return false
	}
	current.count++
	l.peers[peer] = current
	return true
}

// prune drops the windows that have passed. It runs when a window is opened,
// which is rare enough that the scan costs nothing and often enough that the
// map is the size of the members recording causes right now.
func (l *rateLimiter) prune(now time.Time) {
	for peer, window := range l.peers {
		if !now.Before(window.until) {
			delete(l.peers, peer)
		}
	}
}

// clipCause shortens a cause to a byte budget, saying that it did. The cut is
// moved back off a partial rune, so what is stored is still text.
func clipCause(cause string, limit int) string {
	if len(cause) <= limit {
		return cause
	}
	cut := limit
	for cut > 0 && !utf8.ValidString(cause[:cut]) {
		cut--
	}
	return cause[:cut] + fmt.Sprintf("... (%d bytes truncated)", len(cause)-cut)
}
