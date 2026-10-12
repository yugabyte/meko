package db

import (
	"context"
	"time"
)

// abortedAttempts bounds RetryAborted: enough to outlast ordinary contention on
// a row, and past it the caller gets the abort back rather than a hot loop. Under
// fail-on-conflict YB aborts by priority, so an abort does not prove the other
// side committed — both can lose.
const abortedAttempts = 4

// RetryAborted runs fn and re-runs it while it fails with IsAbortedTransaction,
// up to abortedAttempts in all, returning the last error.
//
// fn must be one whole transaction — begin through commit — so a re-run starts
// from nothing. Re-running anything wider would repeat whatever it committed
// before the abort.
func RetryAborted(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 1; ; attempt++ {
		if err = fn(); !IsAbortedTransaction(err) || attempt == abortedAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(attempt) * 10 * time.Millisecond):
		}
	}
}
