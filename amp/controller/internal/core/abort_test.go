package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/amp/controller/internal/db"
)

// TestMapBranchDropRaceErrPreservesTheClass is the endpoint path's half of the
// contract the task worker's retry rests on: an endpoint record that loses the
// branch-liveness interlock must keep SQLSTATE 40001 in its chain after this
// function has described it. %v in place of %w would read the same in a log and
// silently make every such collision a permanent task failure (#578).
func TestMapBranchDropRaceErrPreservesTheClass(t *testing.T) {
	abort := &pgconn.PgError{Code: "40001", Message: "could not serialize access due to concurrent update"}

	got := mapBranchDropRaceErr("validate branch liveness", abort)
	if !db.IsAbortedTransaction(got) {
		t.Fatalf("the abort is no longer in the chain, so the worker would not retry it: %v", got)
	}
	if !strings.Contains(got.Error(), "validate branch liveness") {
		t.Errorf("lost the statement that failed: %q", got)
	}
	if !strings.Contains(got.Error(), "concurrent drop of, or fork off, the branch") {
		t.Errorf("did not describe the conflict: %q", got)
	}
	// It must not acquire a sentinel: an abort is not a verdict on the request, and
	// ErrConflict would render as a 409 telling the caller to change it.
	if errors.Is(got, ErrConflict) || errors.Is(got, ErrNotFound) {
		t.Errorf("an abort must carry no sentinel, got %v", got)
	}
}

// TestMapBranchDropRaceErrLeavesOtherErrorsAlone pins that it classifies rather
// than decorates — a permanent failure must not acquire a race story.
func TestMapBranchDropRaceErrLeavesOtherErrorsAlone(t *testing.T) {
	plain := errors.New("boom")
	got := mapBranchDropRaceErr("insert endpoint", plain)
	if db.IsAbortedTransaction(got) {
		t.Fatalf("an ordinary error was classified as an abort: %v", got)
	}
	if !errors.Is(got, plain) {
		t.Fatalf("the cause was dropped: %v", got)
	}
	if strings.Contains(got.Error(), "concurrent drop") {
		t.Errorf("an ordinary error acquired an interlock story: %q", got)
	}
	if got.Error() != fmt.Sprintf("insert endpoint: %v", plain) {
		t.Errorf("unexpected wrapping: %q", got)
	}
}
