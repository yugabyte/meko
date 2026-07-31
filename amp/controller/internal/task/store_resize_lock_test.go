package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestIsResizeLockViolation covers how a failed task INSERT is classified as the
// resize lock rejecting a concurrent resize.
//
// The #400 regression is the first case: on YSQL the unique violation carries
// nothing but its SQLSTATE and a message (no ConstraintName, no Detail — see
// TestPgResizeLock), so a classifier that reads the constraint name never fires
// and the API returns 500 where it documents 409.
func TestIsResizeLockViolation(t *testing.T) {
	ref := uuid.NewString()
	resize := TaskSpec{Kind: KindResizeEndpoint, TargetRef: &ref, Steps: []StepSpec{{Kind: StepResizeEndpoint, Args: json.RawMessage(`{}`)}}}

	// A YSQL unique violation: SQLSTATE and message only. ConstraintName and
	// Detail are empty, which is exactly why they are not consulted.
	ysql := &pgconn.PgError{
		Code:    "23505",
		Message: `duplicate key value violates unique constraint "` + resizeLockConstraint + `"`,
	}
	// The same violation from upstream PostgreSQL, which does fill in the extra
	// fields. Both must classify identically.
	postgres := &pgconn.PgError{
		Code:           "23505",
		Message:        ysql.Message,
		Detail:         "Key (target_ref)=(" + ref + ") already exists.",
		ConstraintName: resizeLockConstraint,
	}

	cases := []struct {
		name string
		spec TaskSpec
		err  error
		want bool
	}{
		{"ysql unique violation", resize, ysql, true},
		{"postgres unique violation", resize, postgres, true},
		{"wrapped", resize, fmt.Errorf("exec: %w", ysql), true},
		// A unique violation while inserting some other kind of task is not the
		// resize lock — that lock's index only covers resize_endpoint rows.
		{"unique violation on another task kind", TaskSpec{Kind: KindCreateEndpoint}, ysql, false},
		// A resize task with no endpoint to lock is outside the index predicate.
		{"resize without a target", TaskSpec{Kind: KindResizeEndpoint}, ysql, false},
		{"other sqlstate", resize, &pgconn.PgError{Code: "23503", Message: "foreign key violation"}, false},
		{"not a pg error", resize, errors.New("connection reset"), false},
		{"no error", resize, nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isResizeLockViolation(tc.spec, tc.err); got != tc.want {
				t.Fatalf("isResizeLockViolation(%s, %v) = %v, want %v", tc.spec.Kind, tc.err, got, tc.want)
			}
		})
	}
}
