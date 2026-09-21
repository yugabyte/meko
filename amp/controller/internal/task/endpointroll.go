package task

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/google/uuid"

	applog "github.com/yugabyte/amp/controller/internal/log"
)

// The fan-out machinery a parent step drives its per-endpoint children with:
// enumerate, one child per endpoint, run them in a bounded pool, report a line
// each. Shared by every roll over the fleet — today the version upgrade
// (ac-endpoint-upgrade.md §5) and the TLS re-render (ac-endpoint-tls.md).
//
// What is NOT shared yet is the report shaping and the planners themselves, so
// the two are still near-copies of each other. Collapsing them into one
// parameterised pair is meko#1202.

const (
	// DefaultRollParallel is how many endpoints a roll moves at once when the
	// caller does not say. Small on purpose: a roll replaces the pod, so this is
	// how much of the fleet is down at a time.
	DefaultRollParallel = 4

	// MaxRollParallel bounds it. Children run in-process off ONE execution slot,
	// so this is the only cap on a roll's concurrency.
	MaxRollParallel = 50
)

// The outcome of one endpoint in a roll, as reported on the step result. The
// success word is the roll's own (upgraded, rerendered), so it is passed in.
const (
	rollSkipped = "skipped"
	rollFailed  = "failed"
	// rollUnattempted is work the roll did not do and does not claim failed: a
	// child halted by fail-fast, and one whose endpoint lock was held.
	rollUnattempted = "unattempted"
)

// endpointRoll is one line of a roll's report.
type endpointRoll struct {
	EndpointID string `json:"endpoint_id"`
	Outcome    string `json:"outcome"`
	Reason     string `json:"reason,omitempty"`
	// SubtaskID names the `subtasks` row the child ran as — the id in the lease
	// owner that held the endpoint. Empty when no child was created.
	SubtaskID string `json:"subtask_id,omitempty"`
}

// rollSubtasks runs a parent's children through a bounded pool and returns a
// line for each, plus whether it stopped because a cancel was requested.
// `succeeded` is the outcome word a child that returns cleanly gets.
//
// A pool, not waves: a child starts as soon as a slot frees, which is what lets
// fail-fast halt precisely (ac-endpoint-upgrade.md §5.4).
//
// FAIL-FAST: the first real failure stops NEW children from starting; the ones
// already running finish, because killing a child mid-`helm --wait` leaves a
// release the next roll has to clean up. A child refused its endpoint lock is not
// a real failure — ErrLockHeld says somebody else holds the endpoint, not that
// the work would fail.
//
// cancelRequested is polled before each child starts and may be nil. That gap is
// the only place a roll can notice a cancel: the worker's own check runs between
// STEPS, and a roll is one step that can run for hours.
func rollSubtasks(ctx context.Context, runner Subtasks, owner string, subs []*Subtask,
	maxParallel int, succeeded string, cancelRequested func() bool) ([]*endpointRoll, bool) {
	out := make(chan *endpointRoll, len(subs))
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	halted, cancelled := false, false

	// rest reports the children from i on, which never started. abandon says whether
	// their subtask ROWS must also be closed: only on an exit that makes the parent
	// terminal, since a terminal parent is never reclaimed and a pending row under
	// one is work nothing will ever run. An interrupted parent stays reclaimable, so
	// its children are left for the replay.
	rest := func(i int, reason string, abandon bool) {
		for _, sub := range subs[i:] {
			out <- &endpointRoll{
				EndpointID: endpointOf(sub), Outcome: rollUnattempted,
				Reason: reason, SubtaskID: sub.ID.String(),
			}
			if !abandon {
				continue
			}
			if err := runner.Abandon(ctx, sub, owner, reason); err != nil {
				applog.FromContext(ctx).Warn("closing an unattempted subtask",
					applog.KeySubtaskID, sub.ID.String(), "error", err)
			}
		}
	}

	for i := range subs {
		// Take a slot BEFORE reading `halted`, so the check happens as late as it
		// can: a slot frees only when a child finishes, and a failing child records
		// its outcome before releasing.
		took := false
		select {
		case sem <- struct{}{}:
			took = true
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			// Both cases can be ready at once, so give the slot back before leaving —
			// as the halted branch below does. Only when one was actually taken: the
			// ctx branch leaves the channel empty, and a receive on it would block.
			if took {
				<-sem
			}
			rest(i, "not started: the roll was interrupted", false)
			break
		}
		mu.Lock()
		stop := halted
		mu.Unlock()
		if stop {
			<-sem
			rest(i, "not started: the roll stopped after an earlier failure", true)
			break
		}
		if cancelRequested != nil && cancelRequested() {
			<-sem
			cancelled = true
			// Abandoned, like the fail-fast exit: a cancelled roll fails the parent, and
			// a terminal parent is never reclaimed, so a child left pending under one is
			// work nothing will ever run.
			rest(i, "not started: the roll was cancelled", true)
			break
		}

		wg.Add(1)
		go func(sub *Subtask) {
			defer wg.Done()
			defer func() { <-sem }()
			line := &endpointRoll{EndpointID: endpointOf(sub), SubtaskID: sub.ID.String()}
			err := runner.Run(ctx, sub, owner)
			mu.Lock()
			switch {
			case err == nil:
				line.Outcome = succeeded
			case errors.Is(err, ErrLockHeld):
				line.Outcome = rollUnattempted
				line.Reason = "the endpoint lock was held by another task: " + err.Error()
			default:
				line.Outcome = rollFailed
				line.Reason = err.Error()
				halted = true
			}
			mu.Unlock()
			out <- line
		}(subs[i])
	}
	wg.Wait()
	close(out)

	lines := make([]*endpointRoll, 0, len(subs))
	for line := range out {
		lines = append(lines, line)
	}
	return lines, cancelled
}

// cancelRequested reports whether a cancel has been asked for on a roll's own
// task. Both roll kinds poll it between children: each plans a single step, so
// the worker's between-steps check never fires while one is running, and this is
// the only boundary either has.
//
// A nil reader, or a read that fails, reports "no" — cancellation is cooperative,
// and a database blip must not stop a roll that is otherwise converging.
func cancelRequested(ctx context.Context, tasks TaskReader, taskID uuid.UUID) bool {
	if tasks == nil {
		return false
	}
	t, err := tasks.Get(ctx, taskID)
	if err != nil {
		applog.FromContext(ctx).Warn("re-reading the roll's task", "error", err)
		return false
	}
	return t != nil && t.CancelRequested()
}

// endpointOf reads the endpoint a child was created for back out of its args, so
// a report line names the endpoint rather than only the child. Every child kind a
// roll drives carries `endpoint_id`, so this decodes that alone.
func endpointOf(sub *Subtask) string {
	var args struct {
		EndpointID uuid.UUID `json:"endpoint_id"`
	}
	if err := json.Unmarshal(sub.Args, &args); err != nil {
		return ""
	}
	return args.EndpointID.String()
}

// rollTally counts the outcomes, with the roll's own success word first.
func rollTally(lines []*endpointRoll, succeeded string) map[string]int {
	counts := map[string]int{succeeded: 0, rollSkipped: 0, rollFailed: 0, rollUnattempted: 0}
	for _, l := range lines {
		counts[l.Outcome]++
	}
	return counts
}
