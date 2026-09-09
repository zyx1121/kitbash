package daemon

import (
	"context"
	"time"

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
// itself. The daemon serves every member, so a failure inside it is not a
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

// InternalWriteTimeout bounds the write of one cause. The call that failed is
// already answering with a problem, so this write is never allowed to hold it.
const InternalWriteTimeout = 5 * time.Second

// RecordInternalCauses sends every internal cause raised in this process to
// the store, as a log record only an admin's query returns, see PLAN.md
// section 2.4. kitbash-mcp exports its causes through its exporter; kitbashd
// has none, so it writes its own records itself.
//
// It returns the function that stops recording, which the caller runs before
// the store is closed.
func (s *Server) RecordInternalCauses() func() {
	problem.OnInternal(s.recordInternal)
	return func() { problem.OnInternal(nil) }
}

// recordInternal stores one cause. The instance is kept as kitbash.path, which
// is what a member quotes to an admin and what the admin queries by, and the
// body is the cause itself: host paths, git output, whatever failed.
//
// A cause that cannot be stored goes to the log alone. Nothing here reports an
// internal problem, because this is what an internal problem calls.
func (s *Server) recordInternal(instance, cause string) {
	record := store.Log{
		TimeNS:   s.now().UnixNano(),
		Severity: internalSeverity,
		Body:     cause,
		Attributes: store.Attributes{
			User:     InternalProducer,
			Producer: InternalProducer,
			Path:     instance,
			Internal: &isInternal,
			Other:    map[string]any{attrError: problem.SlugInternal},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), InternalWriteTimeout)
	defer cancel()
	if err := s.store.Insert(ctx, store.Export{Logs: []store.Log{record}}); err != nil {
		logger.Printf("could not record the internal cause at %s: %v", instance, err)
	}
}
