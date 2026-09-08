package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/telemetry"
)

// Approvals is what Files needs from kitbashd to queue a call: who the caller
// is and somewhere to put the call. internal/telemetry.Client implements it,
// which keeps the socket out of this package.
type Approvals interface {
	// Admin reports whether the caller is in kitbash-admin, asked once per
	// session. The second return says whether kitbashd answered: a daemon
	// that is not there makes the caller neither, and the write goes to the
	// kernel unqueued.
	Admin(ctx context.Context) (admin, known bool)
	// CreateApproval queues one call with the tool's input verbatim.
	CreateApproval(ctx context.Context, tool string, input json.RawMessage) (*telemetry.Approval, *problem.Problem)
}

// SetApprovals gives the service the approval queue. Without it a write under
// the shared root is attempted and refused by the kernel, which is what a host
// without kitbashd does.
func (s *Service) SetApprovals(approvals Approvals) {
	s.approvals = approvals
}

// SetShared names the root that belongs to the whole organization, /org on a
// kitbash host. A member's write under it is queued for an admin rather than
// attempted, see PLAN.md section 2.1. New sets it already when /org is one of
// the roots; a test standing a temporary folder in for /org sets it here.
func (s *Service) SetShared(root string) {
	s.shared = cleanRoot(root)
}

// Shared is the root writes are queued under, empty when this service has none.
func (s *Service) Shared() string { return s.shared }

// UnderShared reports whether a path lies under the shared root. It is the
// check an admin's session makes on an approved call before running it: the
// only calls that queue are writes to the shared root, so an approval naming
// anything else is not a call this session should carry out on someone else's
// behalf. A service with no shared root queues nothing and so approves
// nothing.
func (s *Service) UnderShared(path string) bool {
	if s.shared == "" || path == "" || !filepath.IsAbs(path) {
		return false
	}
	for _, seg := range strings.Split(path, string(filepath.Separator)) {
		if seg == ".." {
			return false
		}
	}
	return within(filepath.Clean(path), s.shared)
}

// Queue offers one call to the approval queue before it is carried out. It
// returns the queued problem when the call was queued, the daemon's problem
// when queueing failed, and nil when the caller may go ahead: an admin, a path
// outside the shared root, or a session with no queue behind it.
//
// The caller validates the request first. A call that would have been refused
// is refused now rather than queued: an admin approving an operation reads the
// input, not the ten ways it could be malformed.
func (s *Service) Queue(ctx context.Context, path, tool string, input json.RawMessage) *problem.Problem {
	clean, prob := s.resolve(path)
	if prob != nil {
		return prob
	}
	if !s.queues(ctx, clean) {
		return nil
	}
	approval, prob := s.approvals.CreateApproval(ctx, tool, input)
	if prob != nil {
		return prob
	}
	return problem.Queued(approval.ID, fmt.Sprintf(
		"%s is shared, so this %s is waiting for an admin to approve it", clean, tool))
}

// queues reports whether a call about this path goes to the queue instead of
// to the filesystem.
//
// Only a caller kitbashd named a member is queued. An admin writes /org
// directly, and so does a session that could not ask, because a queue nothing
// answers for is not a queue: the write is attempted and the kernel decides,
// which is the whole permission model in version 1 and the same answer a host
// without kitbashd gives.
func (s *Service) queues(ctx context.Context, clean string) bool {
	if s.approvals == nil || s.shared == "" {
		return false
	}
	if !within(clean, s.shared) {
		return false
	}
	admin, known := s.approvals.Admin(ctx)
	return known && !admin
}
