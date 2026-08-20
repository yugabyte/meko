package registry

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/amp/controller/internal/db"
)

// TestExplainAbortPreservesTheClass pins the one property the task worker's retry
// depends on from this side: a lost interlock keeps SQLSTATE 40001 in its chain
// after RecordBranch has described it. Break that — %v instead of %w — and the log
// line still reads correctly while every fork conflict becomes a permanent task
// failure again (#578).
//
// Deliberately not a concurrency test. The live-DB sibling test cannot guard this:
// whether any fork loses the interlock depends on the machine, and CI's
// transactions are quick enough that six concurrent forks can all win, which
// leaves that test's loser assertions unexecuted. The property has to hold
// everywhere, so it is pinned where nothing environmental can skip it.
func TestExplainAbortPreservesTheClass(t *testing.T) {
	abort := &pgconn.PgError{Code: "40001", Message: "could not serialize access due to concurrent update"}
	// As the interlock raises it: already wrapped by the statement that lost.
	raised := fmt.Errorf("validate fork parent liveness: %w", abort)

	got := explainAbort(raised)
	if !db.IsAbortedTransaction(got) {
		t.Fatalf("explainAbort dropped the abort from the chain, so the worker would not retry it: %v", got)
	}
	var pgErr *pgconn.PgError
	if !errors.As(got, &pgErr) || pgErr.Code != "40001" {
		t.Fatalf("the driver error is no longer reachable: %v", got)
	}
	// The whole point of the wrap: say which interlock, and keep what was there.
	if !strings.Contains(got.Error(), "conflicted with a concurrent write to the ancestor") {
		t.Errorf("explainAbort did not describe the conflict: %q", got)
	}
	if !strings.Contains(got.Error(), "validate fork parent liveness") {
		t.Errorf("explainAbort lost the statement that failed: %q", got)
	}
}

// TestExplainAbortPassesThroughEverythingElse pins that it classifies rather than
// decorates: a permanent verdict must not acquire an interlock story, or a caller
// reading the message would retry something that can never succeed.
func TestExplainAbortPassesThroughEverythingElse(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"a sentinel verdict", fmt.Errorf("%w: fork point below the cutoff", ErrInvalid)},
		{"a unique violation", fmt.Errorf("declare branch: %w", &pgconn.PgError{Code: "23505"})},
		{"an ordinary error", errors.New("boom")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := explainAbort(tc.err); got != tc.err {
				t.Fatalf("explainAbort(%v) = %v, want it returned untouched", tc.err, got)
			}
		})
	}
}
