package migration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/deploy"
)

// serviceTestStore is a minimal Store: it only implements the two methods the
// Service touches (GetMigration, MarkRunTerminal) and derives status on read so
// the test asserts against the same state machine the real store uses.
type serviceTestStore struct {
	Store
	migration Migration
	marked    int
}

func (s *serviceTestStore) GetMigration(context.Context, uuid.UUID) (Migration, error) {
	status := DeriveStatus(s.migration)
	s.migration.State = status.State
	s.migration.Detail = status.Detail
	return s.migration, nil
}

func (s *serviceTestStore) MarkRunTerminal(
	_ context.Context,
	runID, _ uuid.UUID,
	status RunStatus,
	errorSummary string,
) error {
	s.marked++
	for i := range s.migration.Runs {
		if s.migration.Runs[i].ID != runID {
			continue
		}
		s.migration.Runs[i].Status = status
		if errorSummary == "" {
			s.migration.Runs[i].ErrorSummary = nil
		} else {
			s.migration.Runs[i].ErrorSummary = &errorSummary
		}
		return nil
	}
	return ErrNotFound
}

type serviceTestRunner struct {
	state deploy.RunState
	err   error
}

func (serviceTestRunner) StartRun(context.Context, deploy.VoyagerRunSpec) (deploy.RunRef, error) {
	return deploy.RunRef{}, errors.New("unexpected StartRun call")
}

func (r serviceTestRunner) RunStatus(context.Context, deploy.RunRef) (deploy.RunState, error) {
	return r.state, r.err
}

func TestServiceGetPersistsTerminalRun(t *testing.T) {
	migrationID := uuid.New()
	runID := uuid.New()
	taskID := uuid.New()
	deployerType := deploy.VoyagerDeployerKubernetes
	metadata, err := json.Marshal(deploy.RunRef{
		Namespace: "amp",
		JobName:   "voyager-assess-test-a1",
		Volume:    "voyager-exp-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &serviceTestStore{migration: Migration{
		ID: migrationID,
		Runs: []Run{{
			ID:               runID,
			MigrationID:      migrationID,
			Operation:        OperationAssess,
			Attempt:          1,
			Status:           RunRunning,
			TaskID:           &taskID,
			DeployerType:     &deployerType,
			DeployerMetadata: metadata,
		}},
	}}

	got, err := NewService(store, serviceTestRunner{state: deploy.RunStateSucceeded}).Get(
		context.Background(), migrationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if store.marked != 1 {
		t.Fatalf("MarkRunTerminal calls = %d, want 1", store.marked)
	}
	if got.State != StateAssessed {
		t.Fatalf("state = %q, want %q", got.State, StateAssessed)
	}
}

func TestServiceGetKeepsPersistedStateWhenDeployerUnavailable(t *testing.T) {
	migrationID := uuid.New()
	taskID := uuid.New()
	deployerType := deploy.VoyagerDeployerDocker
	metadata := json.RawMessage(`{"container_id":"gone","volume":"voyager-exp-test"}`)
	store := &serviceTestStore{migration: Migration{
		ID: migrationID,
		Runs: []Run{{
			ID:               uuid.New(),
			MigrationID:      migrationID,
			Operation:        OperationMigrate,
			Attempt:          1,
			Status:           RunRunning,
			TaskID:           &taskID,
			DeployerType:     &deployerType,
			DeployerMetadata: metadata,
		}},
	}}

	got, err := NewService(store, serviceTestRunner{err: errors.New("docker unavailable")}).Get(
		context.Background(), migrationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if store.marked != 0 {
		t.Fatalf("MarkRunTerminal calls = %d, want 0", store.marked)
	}
	if got.State != StateMigrating {
		t.Fatalf("state = %q, want %q", got.State, StateMigrating)
	}
}

func TestServiceGetPassesThroughWithoutRunner(t *testing.T) {
	migrationID := uuid.New()
	taskID := uuid.New()
	deployerType := deploy.VoyagerDeployerKubernetes
	store := &serviceTestStore{migration: Migration{
		ID: migrationID,
		Runs: []Run{{
			ID:               uuid.New(),
			MigrationID:      migrationID,
			Operation:        OperationAssess,
			Attempt:          1,
			Status:           RunRunning,
			TaskID:           &taskID,
			DeployerType:     &deployerType,
			DeployerMetadata: json.RawMessage(`{"volume":"voyager-exp-test"}`),
		}},
	}}

	if _, err := NewService(store, nil).Get(context.Background(), migrationID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if store.marked != 0 {
		t.Fatalf("MarkRunTerminal calls = %d, want 0 without a runner", store.marked)
	}
}
