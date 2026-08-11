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
