package db

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestRetryAborted(t *testing.T) {
	abort := fmt.Errorf("tombstone storage pool: %w", &pgconn.PgError{Code: "40001"})
	other := errors.New("boom")

	t.Run("re-runs an abort until it succeeds", func(t *testing.T) {
		calls := 0
		err := RetryAborted(context.Background(), func() error {
			calls++
			if calls < 3 {
				return abort
			}
			return nil
		})
		if err != nil || calls != 3 {
			t.Fatalf("err=%v calls=%d, want nil after 3", err, calls)
		}
	})

	t.Run("gives up after a bounded number of aborts", func(t *testing.T) {
		calls := 0
		err := RetryAborted(context.Background(), func() error { calls++; return abort })
		if !IsAbortedTransaction(err) || calls != abortedAttempts {
			t.Fatalf("err=%v calls=%d, want the abort after %d", err, calls, abortedAttempts)
		}
	})

	t.Run("never re-runs any other error", func(t *testing.T) {
		calls := 0
		err := RetryAborted(context.Background(), func() error { calls++; return other })
		if !errors.Is(err, other) || calls != 1 {
			t.Fatalf("err=%v calls=%d, want the error after 1", err, calls)
		}
	})

	t.Run("stops on a cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		err := RetryAborted(ctx, func() error { calls++; return abort })
		if !IsAbortedTransaction(err) || calls != 1 {
			t.Fatalf("err=%v calls=%d, want the abort after 1", err, calls)
		}
	})
}
