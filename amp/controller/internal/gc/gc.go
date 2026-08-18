// Package gc reclaims entities left behind by a failed create.
//
// The controller writes a resource's row mid-provisioning, before the steps that
// build the thing it describes, so a task that fails afterwards leaves the row
// behind. Two kinds leak that way and neither is reclaimed by anything else:
//
//   - clusters — create_cluster's EnsureCluster writes the row before the storage
//     steps run. The read path already hides those (core's clusterCreateNotFailed),
//     which stops them being mistaken for real clusters but leaves the rows and
//     their Kubernetes resources in place forever.
//   - endpoints — create_endpoint records the row before deploying, so it claims
//     the branch's single-writer slot before any compute is spent. A failed deploy
//     leaves that slot held by an endpoint with no pod, and the branch cannot be
//     given a writer again until the row is gone (core's failedCreateEndpoints).
//
// Storage pools are not swept: creating one is a single INSERT with nothing to
// provision, so a failed create leaves no row to reclaim.
//
// Cleanup cannot be a compensating step on the failure path: the controller can
// crash *during* that path, and an inline rollback that never runs leaks
// silently. It has to be a reconciler that is retried across restarts. This
// sweeper is that, built on the durability the task store already provides —
// it selects orphans and enqueues an ordinary delete_cluster or delete_endpoint
// task for each, so the teardown itself is the same code an operator's delete
// runs, and the task store's lease reclaim (a task running with an expired lease
// is claimable again) is what survives a crash mid-teardown.
//
// The sweeper therefore holds no state of its own between ticks. Enqueueing is
// the only thing it does; everything after that is the worker's problem.
package gc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/task"
)

// Orphans is the selection half of the sweeper's world: finding failed-create
// resources, and re-checking one before it is acted on. Satisfied by
// *core.PgBackend.
//
// One select/recheck pair per resource kind, rather than one generic pair over a
// resource name: the predicates have nothing in common but their shape, and each
// returns the ids its own teardown task is addressed by.
type Orphans interface {
	// FailedCreateClusters returns up to limit orphans whose create failed before
	// failedBefore, oldest first.
	FailedCreateClusters(ctx context.Context, failedBefore time.Time, limit int) ([]core.OrphanCluster, error)
	// IsFailedCreateCluster reports whether the cluster is *still* reclaimable:
	// live, its create_cluster task failed, and no teardown already pending or
	// running. Re-checked immediately before a destructive enqueue.
	IsFailedCreateCluster(ctx context.Context, clusterID uuid.UUID) (bool, error)
	// FailedCreateEndpoints returns up to limit endpoints whose creating task
	// failed before failedBefore without the endpoint ever becoming addressable,
	// oldest first.
	FailedCreateEndpoints(ctx context.Context, failedBefore time.Time, limit int) ([]core.OrphanEndpoint, error)
	// IsFailedCreateEndpoint is IsFailedCreateCluster's counterpart for endpoints.
	IsFailedCreateEndpoint(ctx context.Context, endpointID uuid.UUID) (bool, error)
}

// Enqueuer records the teardown task. Satisfied by *task.Service.
type Enqueuer interface {
	DeleteCluster(ctx context.Context, in task.DeleteClusterInput) (*task.Task, error)
	DeleteEndpoint(ctx context.Context, in task.DeleteEndpointInput) (*task.Task, error)
}

// Meter counts sweep outcomes. *metrics.Metrics satisfies it; a nil Meter is
// valid and means metrics are disabled.
type Meter interface {
	// GCSwept records one completed sweep — every resource kind's pass.
	GCSwept(dur time.Duration)
	// GCOrphans records what became of one resource kind's orphans in that sweep.
	GCOrphans(resource string, enqueued, skipped, failed int)
}

// The resource kinds the sweeper reclaims: the value of the metric's resource
// label and of the log's resource field.
const (
	resourceCluster  = "cluster"
	resourceEndpoint = "endpoint"
)

// orphan is one reclaimable resource flattened to what the pass loop needs, so
// that loop is identical for every kind: the fields to log it by, whether it is
// still reclaimable, and how to tear it down. The per-kind adapters below build
// these; nothing else in the package knows what a cluster or an endpoint is.
type orphan struct {
	logWith []any
	still   func(context.Context) (bool, error)
	delete  func(context.Context) (*task.Task, error)
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
	// BatchLimit caps how many orphans one sweep enqueues per resource kind, so a
	// backlog is worked off over several ticks instead of flooding the task queue
	// in one — and so a backlog of one kind cannot consume another's share.
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

// Sweep runs one pass per resource kind: select that kind's orphans past the
// grace window and enqueue the teardown task for each. Exported so a test — or an
// operator-triggered sweep — can run exactly one pass.
//
// BatchLimit is per kind, not per sweep, so a backlog of one kind cannot starve
// another of its share of a tick.
//
// Returns an error only when a selection itself failed, and it runs every pass
// regardless: one kind's query being broken must not stop the others' orphans
// being reclaimed, so the selection errors are joined rather than returned at the
// first one. A per-orphan failure is counted and logged, not returned — one
// unreclaimable orphan must not stop the rest of the batch.
func (s *Sweeper) Sweep(ctx context.Context) error {
	s.sweeping.Lock()
	defer s.sweeping.Unlock()

	start := s.now()
	cutoff := start.Add(-s.cfg.Grace)

	// Clusters before endpoints. A cluster teardown cascades to every endpoint
	// under it, and the endpoint selection leaves alone anything a failed-create
	// cluster already covers, so this order is what keeps one sweep from spending
	// two tasks on the same endpoint.
	passes := []struct {
		resource string
		selects  func(context.Context, time.Time, int) ([]orphan, error)
	}{
		{resourceCluster, s.clusterOrphans},
		{resourceEndpoint, s.endpointOrphans},
	}

	var errs []error
	var total int
	for _, pass := range passes {
		found, err := s.sweepResource(ctx, pass.resource, cutoff, pass.selects)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		total += found
		if err != nil {
			errs = append(errs, fmt.Errorf("%s pass: %w", pass.resource, err))
		}
	}

	dur := s.now().Sub(start)
	if total > 0 {
		s.logger.Info("sweep complete", "found", total, "duration", dur)
	}
	if s.meter != nil {
		s.meter.GCSwept(dur)
	}
	return errors.Join(errs...)
}

// sweepResource runs one kind's pass and returns how many orphans it found. The
// counts are metered per kind, so "endpoints are piling up while clusters are
// clean" is answerable from the metric alone.
func (s *Sweeper) sweepResource(ctx context.Context, resource string, cutoff time.Time,
	selects func(context.Context, time.Time, int) ([]orphan, error),
) (int, error) {
	found, err := selects(ctx, cutoff, s.cfg.BatchLimit)
	if err != nil {
		return 0, err
	}

	var enqueued, skipped, failed int
	for _, o := range found {
		if ctx.Err() != nil {
			return len(found), ctx.Err()
		}
		switch s.reclaim(ctx, resource, o) {
		case outcomeEnqueued:
			enqueued++
		case outcomeSkipped:
			skipped++
		default:
			failed++
		}
	}

	if len(found) > 0 {
		s.logger.Info("resource swept", "resource", resource,
			"found", len(found), "enqueued", enqueued, "skipped", skipped, "failed", failed)
	}
	if s.meter != nil {
		s.meter.GCOrphans(resource, enqueued, skipped, failed)
	}
	return len(found), nil
}

// clusterOrphans selects failed-create clusters as orphans. Teardown is an
// ordinary delete_cluster, which cascades to the cluster's branches and endpoints.
func (s *Sweeper) clusterOrphans(ctx context.Context, cutoff time.Time, limit int) ([]orphan, error) {
	found, err := s.orphans.FailedCreateClusters(ctx, cutoff, limit)
	if err != nil {
		return nil, err
	}
	out := make([]orphan, 0, len(found))
	for _, o := range found {
		out = append(out, orphan{
			logWith: []any{"cluster_id", o.ClusterID, "pool_id", o.PoolID, "failed_at", o.FailedAt},
			still:   func(ctx context.Context) (bool, error) { return s.orphans.IsFailedCreateCluster(ctx, o.ClusterID) },
			delete: func(ctx context.Context) (*task.Task, error) {
				return s.tasks.DeleteCluster(ctx, task.DeleteClusterInput{PoolID: o.PoolID, ClusterID: o.ClusterID})
			},
		})
	}
	return out, nil
}

// endpointOrphans selects failed-create endpoints as orphans. Teardown is an
// ordinary delete_endpoint, which retires the instance (a no-op if it was never
// deployed, which is the usual case here) and deletes the row that was holding the
// branch's writer slot.
func (s *Sweeper) endpointOrphans(ctx context.Context, cutoff time.Time, limit int) ([]orphan, error) {
	found, err := s.orphans.FailedCreateEndpoints(ctx, cutoff, limit)
	if err != nil {
		return nil, err
	}
	out := make([]orphan, 0, len(found))
	for _, o := range found {
		out = append(out, orphan{
			logWith: []any{
				"endpoint_id", o.EndpointID, "branch_id", o.BranchID,
				"cluster_id", o.ClusterID, "failed_at", o.FailedAt,
			},
			still: func(ctx context.Context) (bool, error) { return s.orphans.IsFailedCreateEndpoint(ctx, o.EndpointID) },
			delete: func(ctx context.Context) (*task.Task, error) {
				return s.tasks.DeleteEndpoint(ctx, task.DeleteEndpointInput{ClusterID: o.ClusterID, EndpointID: o.EndpointID})
			},
		})
	}
	return out, nil
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
// deliberately. The consequence is bounded rather than corrupting, since the
// teardown tasks must be idempotent anyway (task.Step's contract, because the
// worker re-runs steps after a crash), and the deployed topology is a single
// controller (the chart pins replicaCount: 1, as the controller holds in-pod
// state). Running two controllers would want an atomic conditional insert in the
// store, not a wider lock here.
func (s *Sweeper) reclaim(ctx context.Context, resource string, o orphan) outcome {
	logger := s.logger.With(append([]any{"resource", resource}, o.logWith...)...)

	still, err := o.still(ctx)
	if err != nil {
		logger.Error("precheck failed, leaving orphan for the next sweep", "error", err)
		return outcomeFailed
	}
	if !still {
		logger.Info("no longer reclaimable, skipping")
		return outcomeSkipped
	}

	t, err := o.delete(ctx)
	if err != nil {
		logger.Error("enqueueing teardown failed, leaving orphan for the next sweep", "error", err)
		return outcomeFailed
	}
	logger.Info("teardown enqueued", "task_id", t.ID)
	return outcomeEnqueued
}
