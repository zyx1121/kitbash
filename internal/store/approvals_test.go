package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the migration test opens the store file directly

	"github.com/zyx1121/kitbash/internal/store"
)

// queued is one approval as a member's session queues it.
func queued(id, requester string, at time.Time) store.Approval {
	return store.Approval{
		ID:          id,
		Requester:   requester,
		Tool:        store.ToolFSWrite,
		Input:       json.RawMessage(`{"path":"/org/handbook/README.md","message":"Add a line"}`),
		State:       store.StatePending,
		RequestedAt: at,
	}
}

func TestApprovalsRoundTrip(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Millisecond)

	if err := st.CreateApproval(ctx, queued(idOne, "alice", at), 0); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	got, found, err := st.Approval(ctx, idOne)
	if err != nil || !found {
		t.Fatalf("Approval: %t, %v", found, err)
	}
	if got.Requester != "alice" || got.State != store.StatePending || got.Tool != store.ToolFSWrite {
		t.Errorf("approval = %+v, want alice's pending fs_write", got)
	}
	if !got.RequestedAt.Equal(at) {
		t.Errorf("requestedAt = %s, want %s", got.RequestedAt, at)
	}
	if got.DecidedAt != nil || got.DecidedBy != "" || len(got.Result) != 0 {
		t.Errorf("a pending approval carries a decision: %+v", got)
	}

	// The listing filters by requester and by state, which is what a member
	// and an admin read respectively.
	if err := st.CreateApproval(ctx, queued(idTwo, "bob", at), 0); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	all, err := st.Approvals(ctx, "", store.StatePending)
	if err != nil || len(all) != 2 {
		t.Fatalf("Approvals for an admin = %d, %v, want 2", len(all), err)
	}
	mine, err := st.Approvals(ctx, "alice", store.StatePending)
	if err != nil || len(mine) != 1 || mine[0].ID != idOne {
		t.Fatalf("Approvals for alice = %+v, %v, want her own alone", mine, err)
	}

	if _, found, err := st.Approval(ctx, "0192f2c0-1234-7abc-8def-0123456789ff"); err != nil || found {
		t.Errorf("Approval of an unknown id = %t, %v, want not found", found, err)
	}
}

// TestApprovalIsDecidedOnce is the state machine: pending to approved with a
// result, or pending to rejected, and nothing else after that.
func TestApprovalIsDecidedOnce(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Millisecond)

	if err := st.CreateApproval(ctx, queued(idOne, "alice", at), 0); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	claimed, found, err := st.ClaimApproval(ctx, idOne, "root", "looks right", at)
	if err != nil || !found {
		t.Fatalf("ClaimApproval: %t, %v", found, err)
	}
	if claimed.State != store.StateApproved || claimed.DecidedBy != "root" || claimed.Note != "looks right" {
		t.Errorf("claimed = %+v, want approved by root with the note", claimed)
	}
	if claimed.DecidedAt == nil || !claimed.DecidedAt.Equal(at) {
		t.Errorf("decidedAt = %v, want %s", claimed.DecidedAt, at)
	}

	if _, _, err := st.ClaimApproval(ctx, idOne, "root", "", at); !errors.Is(err, store.ErrApprovalState) {
		t.Errorf("second claim = %v, want ErrApprovalState", err)
	}
	if _, _, err := st.RejectApproval(ctx, idOne, "root", "changed my mind", at); !errors.Is(err, store.ErrApprovalState) {
		t.Errorf("reject after approve = %v, want ErrApprovalState", err)
	}

	result := json.RawMessage(`{"commit":"abc1234"}`)
	if _, err := st.ResultApproval(ctx, idOne, "someone-else", result); !errors.Is(err, store.ErrApprovalClaimant) {
		t.Errorf("result from another admin = %v, want ErrApprovalClaimant", err)
	}
	found, err = st.ResultApproval(ctx, idOne, "root", result)
	if err != nil || !found {
		t.Fatalf("ResultApproval: %t, %v", found, err)
	}
	if _, err := st.ResultApproval(ctx, idOne, "root", result); !errors.Is(err, store.ErrApprovalState) {
		t.Errorf("second result = %v, want ErrApprovalState", err)
	}

	got, _, err := st.Approval(ctx, idOne)
	if err != nil {
		t.Fatalf("Approval: %v", err)
	}
	if string(got.Result) != string(result) {
		t.Errorf("result = %s, want it stored verbatim", got.Result)
	}
}

func TestApprovalRejectAndUnknown(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Millisecond)

	if err := st.CreateApproval(ctx, queued(idOne, "alice", at), 0); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	rejected, found, err := st.RejectApproval(ctx, idOne, "root", "write it under your own home", at)
	if err != nil || !found {
		t.Fatalf("RejectApproval: %t, %v", found, err)
	}
	if rejected.State != store.StateRejected || rejected.Reason == "" {
		t.Errorf("rejected = %+v, want the state and the reason", rejected)
	}

	unknown := "0192f2c0-1234-7abc-8def-0123456789ff"
	if _, found, err := st.ClaimApproval(ctx, unknown, "root", "", at); err != nil || found {
		t.Errorf("claim of an unknown id = %t, %v, want not found", found, err)
	}
	if found, err := st.ResultApproval(ctx, unknown, "root", json.RawMessage(`{}`)); err != nil || found {
		t.Errorf("result of an unknown id = %t, %v, want not found", found, err)
	}
}

// TestApprovalsPerMemberLimit is enforced inside the transaction, so two
// sessions queueing at once cannot both pass the check and both write.
func TestApprovalsPerMemberLimit(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()
	at := time.Now().UTC()

	for i := range 3 {
		id := fmt.Sprintf("0192f2c0-1234-7abc-8def-01234567890%d", i)
		if err := st.CreateApproval(ctx, queued(id, "alice", at), 3); err != nil {
			t.Fatalf("CreateApproval %d: %v", i, err)
		}
	}
	if err := st.CreateApproval(ctx, queued(idOne, "alice", at), 3); !errors.Is(err, store.ErrTooManyApprovals) {
		t.Errorf("the fourth = %v, want ErrTooManyApprovals", err)
	}
	// The limit is per member, and a decided approval is not pending.
	if err := st.CreateApproval(ctx, queued(idTwo, "bob", at), 3); err != nil {
		t.Errorf("another member's approval = %v, want it queued", err)
	}
	if _, _, err := st.RejectApproval(ctx, "0192f2c0-1234-7abc-8def-012345678900", "root", "no", at); err != nil {
		t.Fatalf("RejectApproval: %v", err)
	}
	if err := st.CreateApproval(ctx, queued(idOne, "alice", at), 3); err != nil {
		t.Errorf("after one was decided = %v, want it queued", err)
	}
}

// TestDeleteApprovalsAndProcessesOfAMember is what removing a member does to
// the store: nothing they queued runs afterwards and no token outlives them.
func TestDeleteApprovalsAndProcessesOfAMember(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()
	at := time.Now().UTC()

	if err := st.CreateApproval(ctx, queued(idOne, "alice", at), 0); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	if err := st.CreateApproval(ctx, queued(idTwo, "bob", at), 0); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	_, hash, err := store.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if err := st.RegisterProcess(ctx, process(idOne, "alice", false), hash, store.Quota{}); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}

	counts, err := st.ProcessCounts(ctx)
	if err != nil || counts["alice"] != 1 {
		t.Fatalf("ProcessCounts = %v, %v, want one for alice", counts, err)
	}

	ids, err := st.DeleteProcessesByOwner(ctx, "alice")
	if err != nil || len(ids) != 1 || ids[0] != idOne {
		t.Fatalf("DeleteProcessesByOwner = %v, %v, want the one id", ids, err)
	}
	n, err := st.DeleteApprovals(ctx, "alice")
	if err != nil || n != 1 {
		t.Fatalf("DeleteApprovals = %d, %v, want 1", n, err)
	}
	left, err := st.Approvals(ctx, "", "")
	if err != nil || len(left) != 1 || left[0].Requester != "bob" {
		t.Errorf("approvals = %+v, %v, want bob's alone", left, err)
	}
	if ids, err := st.DeleteProcessesByOwner(ctx, "alice"); err != nil || len(ids) != 0 {
		t.Errorf("second delete = %v, %v, want nothing", ids, err)
	}
}

// TestRegistrationStoresTheContainerAndDigest is the field restore reads: the
// container name and the image the Process runs, see spec/kitbashd-api.yaml.
func TestRegistrationStoresTheContainerAndDigest(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()
	_, hash, err := store.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	p := process(idOne, "alice", false)
	p.Container = "kitbash-echo-echo"
	p.Digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := st.RegisterProcess(ctx, p, hash, store.Quota{}); err != nil {
		t.Fatalf("RegisterProcess: %v", err)
	}
	got, found, err := st.Process(ctx, idOne)
	if err != nil || !found {
		t.Fatalf("Process: %t, %v", found, err)
	}
	if got.Container != p.Container || got.Digest != p.Digest {
		t.Errorf("process = %+v, want the container and the digest back", got)
	}

	// A replacement keeps whatever the new registration says, including
	// nothing at all.
	p.Container = ""
	p.Digest = ""
	if err := st.RegisterProcess(ctx, p, hash, store.Quota{}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	got, _, err = st.Process(ctx, idOne)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got.Container != "" || got.Digest != "" {
		t.Errorf("process = %+v, want both fields cleared by the replacement", got)
	}
}

// TestMigrationAddsTheM5Columns is the upgrade path from a store written
// before M5: the columns are added rather than the registrations lost.
func TestMigrationAddsTheM5Columns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kitbashd.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Put the file back the way a version without the columns left it.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open the store file directly: %v", err)
	}
	for _, column := range []string{"container", "digest"} {
		if _, err := db.Exec("ALTER TABLE processes DROP COLUMN " + column); err != nil {
			t.Fatalf("drop the column: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st, err = store.Open(path)
	if err != nil {
		t.Fatalf("reopen a store written without the columns: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	_, hash, err := store.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	p := process(idOne, "alice", false)
	p.Container = "kitbash-echo-echo"
	if err := st.RegisterProcess(ctx, p, hash, store.Quota{}); err != nil {
		t.Fatalf("RegisterProcess after the migration: %v", err)
	}
	got, found, err := st.Process(ctx, idOne)
	if err != nil || !found {
		t.Fatalf("Process: %t, %v", found, err)
	}
	if got.Container != "kitbash-echo-echo" {
		t.Errorf("container = %q, want it stored after the migration", got.Container)
	}
}
