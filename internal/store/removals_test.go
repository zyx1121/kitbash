package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/zyx1121/kitbash/internal/store"
)

// The removals table is what makes a removal survive a restart: the mark is a
// row, so a daemon that stopped halfway through reads it at its next start and
// goes on, see internal/daemon/removal.go.

func TestBeginRemovalMarksOnceAndAnswersWhoStartedIt(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()
	at := time.Now().UTC()

	started, err := st.BeginRemoval(ctx, "alice", at)
	if err != nil || !started {
		t.Fatalf("BeginRemoval = %t, %v, want the first call to start it", started, err)
	}
	// A second call finds it running, which is what makes users_remove the
	// same answer rather than a second job.
	started, err = st.BeginRemoval(ctx, "alice", at)
	if err != nil || started {
		t.Fatalf("the second BeginRemoval = %t, %v, want it to find the first", started, err)
	}

	removal, held, err := st.Removal(ctx, "alice")
	if err != nil || !held {
		t.Fatalf("Removal: %t, %v", held, err)
	}
	if removal.State != store.Removing || removal.Step != "" || !removal.FinishedAt.IsZero() {
		t.Errorf("the removal is %+v, want it removing and unfinished", removal)
	}

	resuming, err := st.RemovalsInState(ctx, store.Removing)
	if err != nil || len(resuming) != 1 || resuming[0].Name != "alice" {
		t.Errorf("the removals to resume are %+v, %v, want alice", resuming, err)
	}
}

func TestFinishRemovalRecordsTheStepThatFailed(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()
	if _, err := st.BeginRemoval(ctx, "alice", time.Now().UTC()); err != nil {
		t.Fatalf("BeginRemoval: %v", err)
	}
	done := time.Now().UTC()
	if err := st.FinishRemoval(ctx, "alice", store.Failed, "account", "/org/.archive/alice", done); err != nil {
		t.Fatalf("FinishRemoval: %v", err)
	}

	removal, held, err := st.Removal(ctx, "alice")
	if err != nil || !held {
		t.Fatalf("Removal: %t, %v", held, err)
	}
	if removal.State != store.Failed || removal.Step != "account" {
		t.Errorf("the removal is %+v, want it failed at the account step", removal)
	}
	if removal.Archived != "/org/.archive/alice" || removal.FinishedAt.IsZero() {
		t.Errorf("the removal is %+v, want the archive path and a finish time", removal)
	}
	// A removal that is over is not one to resume.
	resuming, err := st.RemovalsInState(ctx, store.Removing)
	if err != nil || len(resuming) != 0 {
		t.Errorf("the removals to resume are %+v, %v, want none", resuming, err)
	}
}

// A name removed once and created again is a member of its own: the new
// removal replaces the old record rather than finding it and answering that
// the work is already running.
func TestBeginRemovalReplacesARemovalThatIsOver(t *testing.T) {
	st := openProcessStore(t)
	ctx := context.Background()
	if _, err := st.BeginRemoval(ctx, "alice", time.Now().UTC()); err != nil {
		t.Fatalf("BeginRemoval: %v", err)
	}
	if err := st.FinishRemoval(ctx, "alice", store.Removed, "", "/org/.archive/alice",
		time.Now().UTC()); err != nil {
		t.Fatalf("FinishRemoval: %v", err)
	}

	started, err := st.BeginRemoval(ctx, "alice", time.Now().UTC())
	if err != nil || !started {
		t.Fatalf("BeginRemoval after one that is over = %t, %v, want a new one", started, err)
	}
	removal, _, err := st.Removal(ctx, "alice")
	if err != nil {
		t.Fatalf("Removal: %v", err)
	}
	if removal.State != store.Removing || removal.Archived != "" || !removal.FinishedAt.IsZero() {
		t.Errorf("the removal is %+v, want a fresh one", removal)
	}

	if err := st.DeleteRemoval(ctx, "alice"); err != nil {
		t.Fatalf("DeleteRemoval: %v", err)
	}
	if _, held, err := st.Removal(ctx, "alice"); err != nil || held {
		t.Errorf("the removal is still remembered (%t, %v)", held, err)
	}
	list, err := st.Removals(ctx)
	if err != nil || len(list) != 0 {
		t.Errorf("Removals = %+v, %v, want none", list, err)
	}
}
