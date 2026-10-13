package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yugabyte/amp/controller/internal/db"
)

// A branch's floor is its fork_lsn (Branch.ForkLSN). A fork's is written with
// the row; a root's starts NULL and is written once, by SetRootForkLSN after its
// first writer's password is applied or by BackfillRootForkLSN.

// RootForkLSNPending reports whether branchID is a live root with no floor yet,
// which is when the create_endpoint step has a floor to record. False for a
// fork, a root whose floor is set, and an absent or tombstoned branch.
func (b *PgBackend) RootForkLSNPending(ctx context.Context, branchID uuid.UUID) (bool, error) {
	var pending bool
	err := b.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM amp_registry.branches
		                 WHERE branch_id = $1::uuid AND parent_branch_id IS NULL
		                   AND fork_lsn IS NULL AND dropped_at IS NULL)`,
		branchID.String()).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("read root fork lsn: %w", err)
	}
	return pending, nil
}

// setRootForkLSNAttempts bounds SetRootForkLSN's re-runs of a serialization
// abort. A fork of this root writes the root's row in the same transaction as
// its own insert (registry.RecordBranch's liveness interlock), so the two can
// abort each other, and the step that calls this does not fail over a floor.
const setRootForkLSNAttempts = 3

// SetRootForkLSN records lsn as a root branch's floor. First writer wins: a
// fork, a root whose floor is already set, and an absent or tombstoned branch
// are left alone, so a replayed step cannot move a floor once written.
func (b *PgBackend) SetRootForkLSN(ctx context.Context, branchID uuid.UUID, lsn string) error {
	var err error
	for range setRootForkLSNAttempts {
		_, err = b.pool.Exec(ctx,
			`UPDATE amp_registry.branches SET fork_lsn = $2::pg_lsn
			  WHERE branch_id = $1::uuid AND parent_branch_id IS NULL
			    AND fork_lsn IS NULL AND dropped_at IS NULL`,
			branchID.String(), lsn)
		if !db.IsAbortedTransaction(err) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("set root fork lsn: %w", err)
	}
	return nil
}

// BackfillRootForkLSN gives a floor of 0/0 to every live root that has had a
// read_write endpoint created successfully but has no floor, and returns how
// many it updated. Those roots got their first writer before floors were
// recorded, or from a controller that does not record them, so their catalog
// already has a password and the API must stop requiring one the supervisor
// would not apply. 0/0 because where that password was set is unknown: forks
// into the passwordless history before it stay allowed, as they always were.
//
// A create whose step result (compute_password, see the create_endpoint step)
// says the password was not applied (unset, failed), or cannot say whether it
// was (unknown), is no evidence: a floor would drop every later writer's
// password and leave the catalog with none for good, so such a root stays
// password_required until a writer's password lands. The outcomes that do mean
// a password (already_set, unsupported, applied_no_lsn) get 0/0 from the step
// itself; this covers roots whose writer predates the step recording floors.
//
// Idempotent. Meant to run once per start, off the request path: YSQL can plan
// it as a full pass over task_steps rather than through tasks_by_cluster.
func (b *PgBackend) BackfillRootForkLSN(ctx context.Context) (int64, error) {
	tag, err := b.pool.Exec(ctx, backfillRootForkLSNSQL)
	if err != nil {
		return 0, fmt.Errorf("backfill root fork lsn: %w", err)
	}
	return tag.RowsAffected(), nil
}

// BackfillRootForkLSNOf runs the backfill for branchID alone, for a fork of a
// root with no floor: a writer created by a controller that does not record
// floors (an older replica mid-rollout) leaves its root without one until the
// next start's backfill, and its forks would be refused until then.
func (b *PgBackend) BackfillRootForkLSNOf(ctx context.Context, branchID uuid.UUID) (bool, error) {
	tag, err := b.pool.Exec(ctx, backfillRootForkLSNSQL+` AND b.branch_id = $1::uuid`, branchID.String())
	if err != nil {
		return false, fmt.Errorf("backfill root fork lsn of %s: %w", branchID, err)
	}
	return tag.RowsAffected() > 0, nil
}

const backfillRootForkLSNSQL = `UPDATE amp_registry.branches b SET fork_lsn = '0/0'
  WHERE b.parent_branch_id IS NULL AND b.fork_lsn IS NULL AND b.dropped_at IS NULL
    AND EXISTS (SELECT 1 FROM tasks t JOIN task_steps s ON s.task_id = t.id
                 WHERE t.cluster_id = b.cluster_id
                   AND s.kind = 'create_endpoint' AND s.status = 'succeeded'
                   AND s.args->>'role' = 'read_write'
                   AND s.args->>'branch_id' = b.branch_id::text
                   AND COALESCE(s.result->>'compute_password', '') NOT IN ('unset', 'failed', 'unknown'))`

// rowQuerier is what forkBelowFloor reads through: the pool, or a transaction.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// forkBelowFloor is the classifier for a fork insert that selected no parent
// row: an error wrapping ErrForkBelowFloor when the parent is live in clusterID
// and its floor is above forkLSN, and nil when that is not why.
func forkBelowFloor(ctx context.Context, q rowQuerier, clusterID, parentBranchID uuid.UUID, forkLSN string) error {
	var floor string
	err := q.QueryRow(ctx,
		`SELECT fork_lsn::text FROM amp_registry.branches
		  WHERE branch_id = $1::uuid AND cluster_id = $2::uuid AND dropped_at IS NULL
		    AND fork_lsn > $3::pg_lsn`,
		parentBranchID.String(), clusterID.String(), forkLSN).Scan(&floor)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read fork parent floor: %w", err)
	}
	return fmt.Errorf("%w: %s is below %s, the lowest LSN branch %s can be forked at",
		ErrForkBelowFloor, forkLSN, floor, parentBranchID)
}
