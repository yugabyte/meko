// Package gc reclaims entities left behind by a failed create.
//
// The controller writes a cluster row mid-provisioning (create_cluster's
// EnsureCluster) before the storage steps run, so a task that fails afterwards
// leaves a half-built cluster. The read path already hides those (core's
// clusterCreateNotFailed), which stops them being mistaken for real clusters but
// leaves the rows and their Kubernetes resources in place forever.
//
// Cleanup cannot be a compensating step on the failure path: the controller can
// crash *during* that path, and an inline rollback that never runs leaks
// silently. It has to be a reconciler that is retried across restarts. This
// sweeper is that, built on the durability the task store already provides —
// it selects orphans and enqueues an ordinary delete_cluster task for each, so
// the teardown itself is the same code an operator's delete runs, and the task
// store's lease reclaim (a task running with an expired lease is claimable
// again) is what survives a crash mid-teardown.
//
// The sweeper therefore holds no state of its own between ticks. Enqueueing is
// the only thing it does; everything after that is the worker's problem.
package gc

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/task"
)

// Orphans is the selection half of the sweeper's world: finding failed-create
// clusters, and re-checking one before it is acted on. Satisfied by
// *core.PgBackend.
type Orphans interface {
	// FailedCreateClusters returns up to limit orphans whose create failed before
	// failedBefore, oldest first.
	FailedCreateClusters(ctx context.Context, failedBefore time.Time, limit int) ([]core.OrphanCluster, error)
	// IsFailedCreateCluster reports whether the cluster is *still* reclaimable:
	// live, its create_cluster task failed, and no teardown already pending or
	// running. Re-checked immediately before a destructive enqueue.
	IsFailedCreateCluster(ctx context.Context, clusterID uuid.UUID) (bool, error)
}

// Enqueuer records the teardown task. Satisfied by *task.Service.
type Enqueuer interface {
	DeleteCluster(ctx context.Context, in task.DeleteClusterInput) (*task.Task, error)
}

// Meter counts sweep outcomes. *metrics.Metrics satisfies it; a nil Meter is
// valid and means metrics are disabled.
type Meter interface {
	GCSwept(found, enqueued, skipped, failed int, dur time.Duration)
}

// Config tunes the sweeper.
type Config struct {
	// Enabled turns the sweeper on. Off leaves orphans in place — they stay
	// hidden from the read path either way, so the only cost is that they
	// accumulate.
	Enabled bool
	// Interval is how often a sweep runs.
	Interval time.Duration
	// Grace is how long an orphan is left alone after its create failed. It buys
	// two things: a human looking at a fresh failure sees the wreckage rather
	// than a tidied-up absence, and a create being retried by hand is not raced
	// by the sweeper.
	Grace time.Duration
	// BatchLimit caps how many orphans one sweep enqueues, so a backlog is worked
	// off over several ticks instead of flooding the task queue in one.
	BatchLimit int
}

// Sweeper enqueues teardown for failed-create orphans, on an interval.
type Sweeper struct {
	// sweeping serialises Sweep against itself. Run cannot overlap its own ticks
	// (a Ticker drops ticks rather than queueing them), but Sweep is exported for
	// an operator-triggered pass, and two overlapping passes would each pass the
	// precheck before either enqueued — the one window the precheck cannot close.
	// Holding it for the whole pass makes "this Sweeper enqueues a cluster once"
	// true regardless of how Sweep is called.
	sweeping sync.Mutex

	orphans Orphans
	tasks   Enqueuer
	cfg     Config
	logger  *slog.Logger
	meter   Meter
	now     func() time.Time
}

// New builds a Sweeper. meter may be nil.
func New(orphans Orphans, tasks Enqueuer, cfg Config, logger *slog.Logger, meter Meter) *Sweeper {
	return &Sweeper{
		orphans: orphans,
		tasks:   tasks,
		cfg:     cfg,
		logger:  logger.With("component", "gc"),
		meter:   meter,
		now:     time.Now,
	}
}

// Run sweeps every Interval until ctx is cancelled. It returns immediately when
// disabled, so the caller can start it unconditionally.
//
// A sweep failure is logged and the loop continues: the next tick re-selects
// from the database, so there is nothing to carry forward and no backoff state
// to keep. Nothing here is fatal to the controller.
func (s *Sweeper) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		s.logger.Info("orphan gc disabled")
		return
	}
	s.logger.Info("orphan gc started",
		"interval", s.cfg.Interval, "grace", s.cfg.Grace, "batch_limit", s.cfg.BatchLimit)

	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("orphan gc stopped")
			return
		case <-ticker.C:
			if err := s.Sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Error("sweep failed", "error", err)
			}
		}
	}
}

// Sweep runs one pass: select orphans past the grace window and enqueue a
// delete_cluster for each. Exported so a test — or an operator-triggered sweep —
// can run exactly one pass.
//
// Returns an error only when selection itself failed. A per-orphan failure is
// counted and logged, not returned: one unreclaimable orphan must not stop the
// rest of the batch.
func (s *Sweeper) Sweep(ctx context.Context) error {
	s.sweeping.Lock()
	defer s.sweeping.Unlock()

	start := s.now()
	cutoff := start.Add(-s.cfg.Grace)

	found, err := s.orphans.FailedCreateClusters(ctx, cutoff, s.cfg.BatchLimit)
	if err != nil {
		return err
	}

	var enqueued, skipped, failed int
	for _, o := range found {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch s.reclaim(ctx, o) {
		case outcomeEnqueued:
			enqueued++
		case outcomeSkipped:
			skipped++
		default:
			failed++
		}
	}

	dur := s.now().Sub(start)
	if len(found) > 0 {
		s.logger.Info("sweep complete",
			"found", len(found), "enqueued", enqueued, "skipped", skipped, "failed", failed, "duration", dur)
	}
	s.observe(len(found), enqueued, skipped, failed, dur)
	return nil
}

func (s *Sweeper) observe(found, enqueued, skipped, failed int, dur time.Duration) {
	if s.meter == nil {
		return
	}
	s.meter.GCSwept(found, enqueued, skipped, failed, dur)
}

type outcome int

const (
	outcomeFailed outcome = iota
	outcomeEnqueued
	outcomeSkipped
)

// reclaim enqueues teardown for one orphan, after re-checking that it is still
// one.
//
// The precheck is not redundant with the selection query. Between the select and
// this call — a window that widens with BatchLimit — an operator can have deleted
// the cluster by hand, the row can have been tombstoned, or another sweeper can
// have enqueued the same teardown. Re-reading immediately before a destructive
// enqueue makes a stale batch harmless.
//
// It is a check, not a lock. Within one Sweeper that is enough, because Sweep
// holds s.sweeping for the whole pass. Across *separate* Sweepers — two
// controller processes — two prechecks can both pass before either enqueues, and
// nothing in the task store prevents the duplicate: its only mutual exclusion is
// the endpoint resize lock, which does not apply here. That is left unguarded
// deliberately. The consequence is bounded rather than corrupting, since
// delete_cluster must be idempotent anyway (task.Step's contract, because the
// worker re-runs steps after a crash), and the deployed topology is a single
// controller (the chart pins replicaCount: 1, as the controller holds in-pod
// state). Running two controllers would want an atomic conditional insert in the
// store, not a wider lock here.
func (s *Sweeper) reclaim(ctx context.Context, o core.OrphanCluster) outcome {
	logger := s.logger.With("cluster_id", o.ClusterID, "pool_id", o.PoolID, "failed_at", o.FailedAt)

	still, err := s.orphans.IsFailedCreateCluster(ctx, o.ClusterID)
	if err != nil {
		logger.Error("precheck failed, leaving orphan for the next sweep", "error", err)
		return outcomeFailed
	}
	if !still {
		logger.Info("no longer reclaimable, skipping")
		return outcomeSkipped
	}

	t, err := s.tasks.DeleteCluster(ctx, task.DeleteClusterInput{PoolID: o.PoolID, ClusterID: o.ClusterID})
	if err != nil {
		logger.Error("enqueueing teardown failed, leaving orphan for the next sweep", "error", err)
		return outcomeFailed
	}
	logger.Info("teardown enqueued", "task_id", t.ID)
	return outcomeEnqueued
}
