package migration

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/deploy"
)

// Service composes the persisted control tables, live deployer state, and
// Voyager's own visualizer projection into the derive-on-read migration view the
// polling API returns. It is intentionally thin: the store owns persistence and
// status derivation, the runner owns runtime truth, and Service only reconciles
// the two — refreshing non-terminal runs from their deployer and lazily
// persisting terminal transitions so a later read re-derives from durable state.
type Service struct {
	store  Store
	runner deploy.VoyagerJobRunner
}

// NewService returns the derive-on-read migration service. runner may be nil
// (no deployer configured), in which case Get returns the store's view verbatim.
func NewService(store Store, runner deploy.VoyagerJobRunner) *Service {
	return &Service{store: store, runner: runner}
}

// Get returns the migration, first refreshing every in-flight run from its
// deployer. A run observed terminal is persisted once (MarkRunTerminal), then
// the migration is re-read so its derived state reflects the new durable facts.
// A deployer error is advisory: the last good persisted view is kept and a later
// poll retries, so transient Kubernetes/Docker unavailability never corrupts the
// response.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Migration, error) {
	m, err := s.store.GetMigration(ctx, id)
	if err != nil || s.runner == nil {
		return m, err
	}

	changed := false
	for _, run := range m.Runs {
		if !run.Status.InFlight() || run.DeployerType == nil || len(run.DeployerMetadata) == 0 {
			continue
		}
		// A terminal transition is fenced on the run's owning task, so we can
		// only persist an observation for a run that recorded its launcher's
		// task id. A run without one is left for its launcher to converge.
		if run.TaskID == nil {
			continue
		}
		var ref deploy.RunRef
		if err := json.Unmarshal(run.DeployerMetadata, &ref); err != nil {
			return Migration{}, fmt.Errorf("decode deployer metadata for migration run %s: %w", run.ID, err)
		}
		ref.DeployerType = *run.DeployerType
		state, err := s.runner.RunStatus(ctx, ref)
		if err != nil {
			// Live deployer state is advisory. Keep the last persisted run and
			// visualizer view available while the deployer is temporarily
			// unreachable; a later poll retries the observation.
			continue
		}
		switch state {
		case deploy.RunStateSucceeded:
			if err := s.store.MarkRunTerminal(ctx, run.ID, *run.TaskID, RunSucceeded, ""); err != nil {
				return Migration{}, err
			}
			changed = true
		case deploy.RunStateFailed:
			summary := fmt.Sprintf("Voyager %s job failed", run.Operation)
			if err := s.store.MarkRunTerminal(ctx, run.ID, *run.TaskID, RunFailed, summary); err != nil {
				return Migration{}, err
			}
			changed = true
		}
	}
	if changed {
		return s.store.GetMigration(ctx, id)
	}
	return m, nil
}
