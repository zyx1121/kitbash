package daemon

import (
	"testing"
	"time"
)

// waitBudget is how long a test waits for something the daemon does on its own
// schedule: a background loop, a connection the daemon hangs up, a goroutine
// that returns when its context is cancelled. Every wait built on it returns
// the moment the thing happens, so an idle machine spends milliseconds and a
// loaded one is given room rather than a false failure. It is deliberately far
// longer than any of these waits takes on an idle host.
const waitBudget = 30 * time.Second

// waitStep is how often a poll looks again.
const waitStep = 5 * time.Millisecond

// waitFor polls until the condition holds or the budget runs out, which is how
// the tests wait for a child to exit or a session to be swept.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitBudget)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s", waitBudget, what)
		}
		time.Sleep(waitStep)
	}
}

// waitRecv waits for one value on a channel and fails the test, naming what it
// waited for, when the budget runs out first.
func waitRecv[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	value, ok := recvWithin(ch)
	if !ok {
		t.Fatalf("waited %s for %s", waitBudget, what)
	}
	return value
}

// recvWithin waits for one value on a channel and reports whether it arrived
// inside the budget. It is what a cleanup uses, where a fatal failure would
// skip the cleanups still to run.
func recvWithin[T any](ch <-chan T) (T, bool) {
	timer := time.NewTimer(waitBudget)
	defer timer.Stop()
	select {
	case value := <-ch:
		return value, true
	case <-timer.C:
		var zero T
		return zero, false
	}
}
