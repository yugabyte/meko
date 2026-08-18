package core

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// OrphanCluster identifies one failed-create cluster the GC sweeper should
// reclaim, with the pool it belongs to (delete_cluster is addressed by both).
type OrphanCluster struct {
	PoolID    uuid.UUID
	ClusterID uuid.UUID
	// FailedAt is when the cluster's create_cluster task last moved, i.e. when it
	// failed. Carried for logging: it is what the grace window is measured
	// against, so an operator reading the log can see how long the orphan sat.
	FailedAt time.Time
}

// failedCreateClusters selects the live clusters whose create_cluster task
// failed, oldest first.
//
// The positive form of clusterCreateNotFailed, which the read path uses to hide
// these same rows: hiding keeps a half-provisioned cluster from surfacing as a
// real one, and this reclaims it. Both rest on the invariant documented there —
// a cluster has exactly one create_cluster task, because a retry mints a fresh
// cluster id — so one failed task is definitive, with no need to also prove no
// success exists.
//
// A cluster with a delete_cluster task already pending or running is skipped: the
// task store would refuse the duplicate anyway (its per-cluster lock holds until
// the in-flight task is terminal), and excluding it here keeps the sweeper from
// logging a conflict every tick for something already being handled. Succeeded
// deletes need no clause — they tombstone the row, and dropped_at excludes it.
const failedCreateClusters = `
	SELECT p.id, p.storage_pool_id, t.updated_at
	FROM amp_registry.clusters p
	JOIN tasks t ON t.cluster_id = p.id AND t.kind = 'create_cluster' AND t.status = 'failed'
	WHERE p.dropped_at IS NULL
	  AND t.updated_at < $1
	  AND NOT EXISTS (
	    SELECT 1 FROM tasks d
	    WHERE d.cluster_id = p.id AND d.kind = 'delete_cluster'
	      AND d.status IN ('pending', 'running'))
	ORDER BY t.updated_at, p.id
	LIMIT $2`

// FailedCreateClusters returns up to limit failed-create orphan clusters whose
// create_cluster task failed before failedBefore, oldest failure first.
//
// The grace window is the caller's: it exists so an orphan is not torn down
// while a human is still looking at the failure, and so a create that fails and
// is retried by hand has a chance to be superseded first.
//
// Not on the Backend interface. The selection is a join against the task log,
// which MemBackend has no knowledge of — it ignores includeFailed for the same
// reason — so putting it there would mean an interface method one implementation
// could not honestly satisfy. The sweeper depends on a narrow interface instead.
func (b *PgBackend) FailedCreateClusters(ctx context.Context, failedBefore time.Time, limit int) ([]OrphanCluster, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := b.pool.Query(ctx, failedCreateClusters, failedBefore, limit)
	if err != nil {
		return nil, fmt.Errorf("select failed-create clusters: %w", err)
	}
	defer rows.Close()

	var out []OrphanCluster
	for rows.Next() {
		var o OrphanCluster
		if err := rows.Scan(&o.ClusterID, &o.PoolID, &o.FailedAt); err != nil {
			return nil, fmt.Errorf("scan failed-create cluster: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate failed-create clusters: %w", err)
	}
	return out, nil
}

// isFailedCreateCluster is failedCreateClusters narrowed to one id and with the
// grace window dropped — the window governs selection, not re-validation, and
// re-applying it would make the answer drift with the clock between the two
// queries of a single sweep.
const isFailedCreateCluster = `
	SELECT EXISTS (
	  SELECT 1
	  FROM amp_registry.clusters p
	  JOIN tasks t ON t.cluster_id = p.id AND t.kind = 'create_cluster' AND t.status = 'failed'
	  WHERE p.id = $1::uuid
	    AND p.dropped_at IS NULL
	    AND NOT EXISTS (
	      SELECT 1 FROM tasks d
	      WHERE d.cluster_id = p.id AND d.kind = 'delete_cluster'
	        AND d.status IN ('pending', 'running')))`

// IsFailedCreateCluster reports whether the cluster is still reclaimable: live,
// its create_cluster task failed, and no teardown already pending or running.
//
// The sweeper calls this immediately before enqueueing a delete, so a batch that
// went stale — the cluster deleted by hand, tombstoned, or already claimed by
// another sweeper — cannot cause a redundant teardown. A missing cluster is not
// an error here: it answers false, which is the same "nothing to do" the caller
// acts on.
func (b *PgBackend) IsFailedCreateCluster(ctx context.Context, clusterID uuid.UUID) (bool, error) {
	var ok bool
	if err := b.pool.QueryRow(ctx, isFailedCreateCluster, clusterID).Scan(&ok); err != nil {
		return false, fmt.Errorf("recheck failed-create cluster %s: %w", clusterID, err)
	}
	return ok, nil
}

// OrphanEndpoint identifies one failed-create endpoint the GC sweeper should
// reclaim. ClusterID is what delete_endpoint is addressed by; BranchID is
// carried for the log, since "which branch lost its writer" is the question an
// operator asks next.
type OrphanEndpoint struct {
	ClusterID  uuid.UUID
	BranchID   uuid.UUID
	EndpointID uuid.UUID
	// FailedAt is when the creating task last moved, i.e. when it failed — what
	// the grace window is measured against.
	FailedAt time.Time
}

// failedCreateEndpoints selects the endpoints whose creating task failed before
// they were ever usable, oldest first.
//
// An endpoint leaks the same way a cluster does but costs more while it sits
// there. create_endpoint records the row *before* deploying, deliberately, so the
// row claims the branch's single-writer slot before any compute is spent — and a
// deploy that then fails leaves endpoints_one_writer_per_branch holding that slot
// against every later attempt to give the branch a writer. The branch cannot be
// written to again until the row is gone.
//
// Selection is by create_endpoint *step*, not by task kind, because three kinds
// create endpoints: create_cluster and branch each plan one for `with_writer`, and
// create_endpoint is the standalone scale-up. Only the last carries the endpoint id
// in tasks.target_ref, so the step's args are what cover all three — which is how
// this sees a `with_writer` fork whose branch was recorded but whose endpoint never
// came up. The join runs from `tasks` so the small set (failed tasks) drives it;
// a step whose args carry no endpoint_id yields NULL and matches nothing.
//
// Two conditions beyond "the creating task failed" decide reclaimability:
//
//   - address IS NULL — the endpoint never reached the point where its address was
//     recorded, so nothing usable was handed to a caller. One that did get an
//     address is left alone even if its task later failed: it may be serving, and
//     its owner can delete it through the API.
//   - the branch is live — a tombstoned branch has already had its endpoints deleted
//     by the cascade that tombstoned it.
//
// Skipped: an endpoint whose delete_endpoint is already pending or running (as in
// failedCreateClusters, to avoid re-enqueueing every tick), and one whose cluster is
// itself a failed-create orphan, since delete_cluster cascades to it anyway. A
// cascade started *after* this pass selects can still race it, harmlessly —
// delete_endpoint no-ops on a missing instance and a missing row.
const failedCreateEndpoints = `
	SELECT e.id, b.branch_id, b.cluster_id, t.updated_at
	FROM tasks t
	JOIN task_steps s ON s.task_id = t.id AND s.kind = 'create_endpoint'
	JOIN amp_registry.endpoints e ON e.id = (s.args->>'endpoint_id')::uuid
	JOIN amp_registry.branches b ON b.branch_id = e.branch_id
	WHERE t.status = 'failed'
	  AND t.updated_at < $1
	  AND e.address IS NULL
	  AND b.dropped_at IS NULL
	  AND NOT EXISTS (
	    SELECT 1 FROM tasks d
	    WHERE d.kind = 'delete_endpoint' AND d.target_ref = e.id::text
	      AND d.status IN ('pending', 'running'))
	  AND NOT EXISTS (
	    SELECT 1 FROM tasks ct
	    WHERE ct.cluster_id = b.cluster_id AND ct.kind = 'create_cluster'
	      AND ct.status = 'failed')
	ORDER BY t.updated_at, e.id
	LIMIT $2`

// FailedCreateEndpoints returns up to limit failed-create orphan endpoints whose
// creating task failed before failedBefore, oldest failure first. The grace
// window is the caller's, for the reasons FailedCreateClusters gives.
func (b *PgBackend) FailedCreateEndpoints(ctx context.Context, failedBefore time.Time, limit int) ([]OrphanEndpoint, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := b.pool.Query(ctx, failedCreateEndpoints, failedBefore, limit)
	if err != nil {
		return nil, fmt.Errorf("select failed-create endpoints: %w", err)
	}
	defer rows.Close()

	var out []OrphanEndpoint
	for rows.Next() {
		var o OrphanEndpoint
		if err := rows.Scan(&o.EndpointID, &o.BranchID, &o.ClusterID, &o.FailedAt); err != nil {
			return nil, fmt.Errorf("scan failed-create endpoint: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate failed-create endpoints: %w", err)
	}
	return out, nil
}

// isFailedCreateEndpoint is failedCreateEndpoints narrowed to one id and with the
// grace window dropped, for the reason isFailedCreateCluster gives.
const isFailedCreateEndpoint = `
	SELECT EXISTS (
	  SELECT 1
	  FROM tasks t
	  JOIN task_steps s ON s.task_id = t.id AND s.kind = 'create_endpoint'
	  JOIN amp_registry.endpoints e ON e.id = (s.args->>'endpoint_id')::uuid
	  JOIN amp_registry.branches b ON b.branch_id = e.branch_id
	  WHERE e.id = $1::uuid
	    AND t.status = 'failed'
	    AND e.address IS NULL
	    AND b.dropped_at IS NULL
	    AND NOT EXISTS (
	      SELECT 1 FROM tasks d
	      WHERE d.kind = 'delete_endpoint' AND d.target_ref = e.id::text
	        AND d.status IN ('pending', 'running'))
	    AND NOT EXISTS (
	      SELECT 1 FROM tasks ct
	      WHERE ct.cluster_id = b.cluster_id AND ct.kind = 'create_cluster'
	        AND ct.status = 'failed'))`

// IsFailedCreateEndpoint reports whether the endpoint is still reclaimable:
// present, addressless, on a live branch, its creating task failed, and no
// teardown already covering it. Called immediately before a destructive enqueue,
// so a batch that went stale cannot cause a redundant teardown. A missing
// endpoint answers false — the same "nothing to do" the caller acts on.
func (b *PgBackend) IsFailedCreateEndpoint(ctx context.Context, endpointID uuid.UUID) (bool, error) {
	var ok bool
	if err := b.pool.QueryRow(ctx, isFailedCreateEndpoint, endpointID).Scan(&ok); err != nil {
		return false, fmt.Errorf("recheck failed-create endpoint %s: %w", endpointID, err)
	}
	return ok, nil
}
