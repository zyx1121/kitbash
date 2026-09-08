package daemon

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/zyx1121/kitbash/internal/store"
	"github.com/zyx1121/kitbash/internal/sysusers"
)

// RestoreTimeout is how long one container may take to come back, which is
// what spec/kitbashd-api.yaml gives it.
const RestoreTimeout = 60 * time.Second

// RestoreCounts is what one restore did, and what its log line reports.
type RestoreCounts struct {
	// Started is how many containers came back.
	Started int
	// Missing is how many the runtime no longer has, which are unregistered.
	Missing int
	// Failed is how many the runtime refused to start, which stay registered
	// so the next session or the next boot can try again.
	Failed int
	// Legacy is how many carried no container name, which are unregistered:
	// a registration written before M5 names nothing restore could start, and
	// no later boot will make it restorable.
	Legacy int
}

// Restore starts every registered Process again as its owner, which is what
// makes "all Processes come back after a reboot" true, see PLAN.md section 2.3.
// It runs once per daemon start.
//
// The daemon does not wait for it before it serves: a host with fifty
// containers would keep every member's session waiting on podman. Restore runs
// beside the listeners and logs one line when it is done.
//
// Owners run in parallel and the containers of one owner run one at a time: a
// rootless podman serialises its own store per user anyway, and starting one
// member's containers must not be held up by another member's.
func (s *Server) Restore(ctx context.Context) RestoreCounts {
	if s.noRestore {
		logger.Printf("restore: skipped, the daemon was started with restore off")
		return RestoreCounts{}
	}
	list, err := s.store.Processes(ctx, "")
	if err != nil {
		logger.Printf("restore: could not read the registered Processes: %v", err)
		return RestoreCounts{}
	}

	var counts RestoreCounts
	byOwner := map[string][]store.Process{}
	for _, p := range list {
		// A registration without a container name was written before M5. It
		// names nothing the runtime could start and no later boot will change
		// that, so it goes: the row is deleted, which revokes its token and
		// drops its fan out subscription.
		if p.Container == "" {
			counts.Legacy++
			logger.Printf("restore: the Process %s of %s carries no container name, unregistering it",
				p.ID, p.Owner)
			if _, err := s.store.DeleteProcess(ctx, p.ID); err != nil {
				logger.Printf("restore: unregistering %s: %v", p.ID, err)
			}
			s.fanout.untrack(p.ID)
			s.endMCPSessions(p.ID)
			continue
		}
		byOwner[p.Owner] = append(byOwner[p.Owner], p)
	}
	owners := make([]string, 0, len(byOwner))
	for owner := range byOwner {
		owners = append(owners, owner)
	}
	sort.Strings(owners)

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, owner := range owners {
		processes := byOwner[owner]
		wg.Add(1)
		go func() {
			defer wg.Done()
			owned := s.restoreOwner(ctx, owner, processes)
			mu.Lock()
			counts.Started += owned.Started
			counts.Missing += owned.Missing
			counts.Failed += owned.Failed
			mu.Unlock()
		}()
	}
	wg.Wait()

	logger.Printf("restore: started %d, missing %d, failed %d, legacy %d",
		counts.Started, counts.Missing, counts.Failed, counts.Legacy)
	return counts
}

// restoreOwner starts one member's containers, one after the other.
func (s *Server) restoreOwner(ctx context.Context, owner string, processes []store.Process) RestoreCounts {
	var counts RestoreCounts
	m, found, err := s.users.Lookup(ctx, owner)
	if err != nil {
		logger.Printf("restore: looking up %s: %v", owner, err)
		counts.Failed += len(processes)
		return counts
	}
	if !found {
		// A registration whose owner is gone is a Process nobody can start.
		// Removing the member is what deletes those registrations; one that
		// survived that is left alone rather than run as somebody else.
		logger.Printf("restore: %s owns %d registered Processes and is no longer a member",
			owner, len(processes))
		counts.Failed += len(processes)
		return counts
	}

	for _, p := range processes {
		start, cancel := context.WithTimeout(ctx, RestoreTimeout)
		err := s.runner.Start(start, m, p.Container)
		cancel()
		switch {
		case err == nil:
			counts.Started++
		case errors.Is(err, sysusers.ErrNoContainer):
			// The container is gone, so the registration names nothing and
			// its token belongs to no Process. Unregistering revokes it.
			counts.Missing++
			logger.Printf("restore: %s of %s no longer exists, unregistering the Process %s",
				p.Container, owner, p.ID)
			if _, err := s.store.DeleteProcess(ctx, p.ID); err != nil {
				logger.Printf("restore: unregistering %s: %v", p.ID, err)
			}
			s.fanout.untrack(p.ID)
			s.endMCPSessions(p.ID)
		default:
			counts.Failed++
			logger.Printf("restore: starting %s of %s: %v", p.Container, owner, err)
		}
	}
	return counts
}
