package db

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestIsAbortedTransaction pins the class the task worker re-runs a step on. The
// wrapped case is the one that matters in practice: both interlock owners attach a
// legible message over the driver error, so a predicate that only matched a bare
// *pgconn.PgError would silently never fire (#578).
func TestIsAbortedTransaction(t *testing.T) {
	abort := &pgconn.PgError{Code: "40001", Message: "could not serialize access due to concurrent update"}

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"bare serialization failure", abort, true},
		{"wrapped once, as the stores wrap it", fmt.Errorf("validate fork parent liveness: %w", abort), true},
		{"wrapped twice", fmt.Errorf("record branch recipe: %w", fmt.Errorf("declare branch: %w", abort)), true},
		// Not in the class: the transaction did not roll back for a conflict, so
		// re-running cannot win — and for the unique violation it can never win.
		{"unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"foreign key violation", &pgconn.PgError{Code: "23503"}, false},
		// Same family, but a separate decision: it satisfies the clean-rollback
		// precondition, so admitting it is a deliberate widening, not an oversight.
		{"deadlock detected is not admitted yet", &pgconn.PgError{Code: "40P01"}, false},
		{"object in use (transient, but no rollback)", &pgconn.PgError{Code: "55006"}, false},
		{"an ordinary error", errors.New("boom"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAbortedTransaction(tc.err); got != tc.want {
				t.Fatalf("IsAbortedTransaction(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
