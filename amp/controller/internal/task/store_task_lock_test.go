package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestTaskLockViolation covers how a failed task INSERT is classified as a
// first-writer-wins lock rejecting a concurrent task, and which typed conflict
// each kind maps to.
//
// The #400 regression is the first case: on YSQL the unique violation carries
// nothing but its SQLSTATE and a message (no ConstraintName, no Detail — see
// TestPgTaskLocks), so a classifier that reads the constraint name never fires
// and the API returns 500 where it documents 409.
func TestTaskLockViolation(t *testing.T) {
	ref := uuid.NewString()
	resize := TaskSpec{Kind: KindResizeEndpoint, TargetRef: &ref, Steps: []StepSpec{{Kind: StepResizeEndpoint, Args: json.RawMessage(`{}`)}}}
	extAdd := TaskSpec{Kind: KindAddBranchExtensions, TargetRef: &ref, Steps: []StepSpec{{Kind: StepAddBranchExtensions, Args: json.RawMessage(`{}`)}}}
	migrationRun := TaskSpec{Kind: KindVoyagerStartMigrationRun, TargetRef: &ref, Steps: []StepSpec{{Kind: StepVoyagerEnsureRun, Args: json.RawMessage(`{}`)}}}

	// A YSQL unique violation: SQLSTATE and message only. ConstraintName and
	// Detail are empty, which is exactly why they are not consulted.
	ysql := &pgconn.PgError{
		Code:    "23505",
		Message: `duplicate key value violates unique constraint "` + KindResizeEndpoint.lockIndex + `"`,
	}
	// The same violation from upstream PostgreSQL, which does fill in the extra
	// fields. Both must classify identically.
	postgres := &pgconn.PgError{
		Code:           "23505",
		Message:        ysql.Message,
		Detail:         "Key (target_ref)=(" + ref + ") already exists.",
		ConstraintName: KindResizeEndpoint.lockIndex,
	}

	cases := []struct {
		name string
		spec TaskSpec
		err  error
		want error // nil = not a lock violation
	}{
		{"ysql unique violation", resize, ysql, ErrResizeInProgress},
		{"postgres unique violation", resize, postgres, ErrResizeInProgress},
		{"wrapped", resize, fmt.Errorf("exec: %w", ysql), ErrResizeInProgress},
		// Classification is by KIND, not by the constraint's name — so the same
		// YSQL error inserting an extensions task maps to that kind's own conflict.
		// Sound because the two indexes' predicates are kind-disjoint.
		{"extensions add maps to its own conflict", extAdd, ysql, ErrBranchExtensionsInProgress},
		{"migration run maps to its own conflict", migrationRun, ysql, ErrMigrationRunInProgress},
		// A unique violation while inserting a kind with no lock at all is not one:
		// no index covers those rows.
		{"unique violation on an unlocked kind", TaskSpec{Kind: KindCreateEndpoint}, ysql, nil},
		// A locked kind with no target to lock is outside the index predicate.
		{"resize without a target", TaskSpec{Kind: KindResizeEndpoint}, ysql, nil},
		{"other sqlstate", resize, &pgconn.PgError{Code: "23503", Message: "foreign key violation"}, nil},
		{"not a pg error", resize, errors.New("connection reset"), nil},
		{"no error", resize, nil, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := taskLockViolation(tc.spec, tc.err)
			if !errors.Is(got, tc.want) {
				t.Fatalf("taskLockViolation(%s, %v) = %v, want %v", tc.spec.Kind, tc.err, got, tc.want)
			}
		})
	}
}
