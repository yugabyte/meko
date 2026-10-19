package service

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// desiredRevReader reads many environments' change tokens at once;
// repository.EnvironmentRepo.DesiredRevs. An environment absent from the map reads as 0.
type desiredRevReader interface {
	DesiredRevs(envUUIDs []string) (map[string]int64, error)
}

// desiredRevWatcher holds agents' desired-state long-polls. Every held read registers
// here, and one loop per replica reads the change tokens of every watched environment
// in a single query each DesiredPollEvery, then wakes the reads whose token moved. A
// replica holding N reads therefore costs one query per tick, not N.
//
// The loop runs only while something is waiting, so an idle replica issues no queries.
type desiredRevWatcher struct {
	revs    desiredRevReader
	waiters map[string]map[*revWaiter]struct{} // env -> its held reads; guarded by mu
	every   time.Duration                      // DesiredPollEvery, read once so the loop never races a change to it
	mu      sync.Mutex
	running bool
}

type revWaiter struct {
	moved chan int64 // buffered 1; sent at most once, by the loop
	since int64
}

func newDesiredRevWatcher(revs desiredRevReader) *desiredRevWatcher {
	return &desiredRevWatcher{revs: revs, waiters: map[string]map[*revWaiter]struct{}{}, every: DesiredPollEvery}
}

// wait blocks until envUUID's token differs from since, wait elapses, or ctx ends, and
// returns the token as last seen: the new one, or since when the wait ran out. A failed
// token read does not end the wait; the loop logs it and retries on its next tick, so a
// database blip lets holds time out rather than failing every held request at once.
func (w *desiredRevWatcher) wait(ctx context.Context, envUUID string, since int64, wait time.Duration) (int64, error) {
	r := &revWaiter{since: since, moved: make(chan int64, 1)}
	w.add(envUUID, r)
	defer w.remove(envUUID, r)

	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-deadline.C:
		return since, nil
	case token := <-r.moved:
		return token, nil
	}
}

func (w *desiredRevWatcher) add(envUUID string, r *revWaiter) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.waiters[envUUID] == nil {
		w.waiters[envUUID] = map[*revWaiter]struct{}{}
	}
	w.waiters[envUUID][r] = struct{}{}
	if !w.running {
		w.running = true
		go w.loop()
	}
}

func (w *desiredRevWatcher) remove(envUUID string, r *revWaiter) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.waiters[envUUID], r)
	if len(w.waiters[envUUID]) == 0 {
		delete(w.waiters, envUUID)
	}
}

func (w *desiredRevWatcher) loop() {
	tick := time.NewTicker(w.every)
	defer tick.Stop()
	for range tick.C {
		held := w.watched()
		if held == nil {
			return // nothing waiting; the next add starts a new loop
		}
		envs := make([]string, 0, len(held))
		for env := range held {
			envs = append(envs, env)
		}
		revs, err := w.revs.DesiredRevs(envs)
		if err != nil {
			slog.Warn("desired-state long-poll: reading change tokens failed; retrying next tick",
				"environments", len(envs), "error", err)
			continue
		}
		w.wake(held, revs)
	}
}

// watched snapshots the held reads, by environment, or returns nil after marking the
// loop stopped when there are none. Deciding both under one lock is what keeps an add
// that races the loop's exit from being left without a loop.
func (w *desiredRevWatcher) watched() map[string][]*revWaiter {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.waiters) == 0 {
		w.running = false
		return nil
	}
	held := make(map[string][]*revWaiter, len(w.waiters))
	for env, rs := range w.waiters {
		for r := range rs {
			held[env] = append(held[env], r)
		}
	}
	return held
}

// wake wakes the reads in held, the snapshot the query was issued for, whose token
// moved. Only the snapshot: a read registered while the query ran is judged on the next
// tick. Its environment may be missing from revs, which would read as 0, and its own
// token read may be newer than the query's, either of which would wake it with a wrong
// token. A read in the snapshot read its token before registering, so the query, issued
// after, sees that token or a newer one.
func (w *desiredRevWatcher) wake(held map[string][]*revWaiter, revs map[string]int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for env, rs := range held {
		token := revs[env]
		for _, r := range rs {
			if _, still := w.waiters[env][r]; !still || token == r.since {
				continue // returned already (timed out, hung up), or nothing moved
			}
			r.moved <- token
			// The waiter returns on its own and must not be sent to twice.
			delete(w.waiters[env], r)
		}
		if len(w.waiters[env]) == 0 {
			delete(w.waiters, env)
		}
	}
}
