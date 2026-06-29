package task

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemStore is an in-memory Store for unit tests: a faithful model of the
// write-ahead task store — tasks and their ordered steps, an atomic claim under a
// lease (a task that is pending, or running with an expired lease, is claimable),
// and the step/task status transitions the worker drives. It lets the framework
// (and any package that creates tasks) be tested without a database; the real
// PgStore SQL is exercised by the e2e suite. It mirrors core.MemBackend, the
// in-memory core.Backend the API tests use. Safe for concurrent use.
type MemStore struct {
	mu        sync.Mutex
	order     []uuid.UUID
	tasks     map[uuid.UUID]*Task
	stepsByID map[uuid.UUID]*StepRecord
	now       func() time.Time
}

// NewMemStore returns an empty in-memory Store.
func NewMemStore() *MemStore {
	return &MemStore{
		tasks:     map[uuid.UUID]*Task{},
		stepsByID: map[uuid.UUID]*StepRecord{},
		now:       time.Now,
	}
}

// Seed inserts a task and its steps directly, bypassing Create — for preloading
// states Create can't produce (a running task with an expired lease, some steps
// already succeeded, a step left in flight). The task's StepIDs must reference the
// seeded steps in execution order. steps may be nil to seed a task alone (e.g. to
// exercise status reads).
func (s *MemStore) Seed(t *Task, steps []*StepRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, t.ID)
	s.tasks[t.ID] = t
	for _, st := range steps {
		s.stepsByID[st.ID] = st
	}
}

func (s *MemStore) Create(_ context.Context, spec TaskSpec) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := uuid.New()
	now := s.now()
	var stepIDs []uuid.UUID
	for _, sp := range spec.Steps {
		// Mirror PgStore: each step gets its own id; the task lists them in order.
		stepID := uuid.New()
		stepIDs = append(stepIDs, stepID)
		s.stepsByID[stepID] = &StepRecord{ID: stepID, TaskID: id, Kind: sp.Kind, Status: StatusPending, Args: sp.Args, UpdatedAt: now}
	}
	t := &Task{
		ID:        id,
		Kind:      spec.Kind,
		Status:    StatusPending,
		ProjectID: spec.ProjectID,
		TargetRef: spec.TargetRef,
		Params:    spec.Params,
		StepIDs:   stepIDs,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.order = append(s.order, id)
	s.tasks[id] = t
	return cloneTask(t), nil
}

func (s *MemStore) ClaimNext(_ context.Context, owner string, lease time.Duration) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, id := range s.order {
		t := s.tasks[id]
		claimable := t.Status == StatusPending ||
			(t.Status == StatusRunning && t.LeaseExpiry != nil && t.LeaseExpiry.Before(now))
		if !claimable {
			continue
		}
		t.Status = StatusRunning
		t.Owner = &owner
		exp := now.Add(lease)
		t.LeaseExpiry = &exp
		t.UpdatedAt = now
		return cloneTask(t), nil
	}
	return nil, nil
}

func (s *MemStore) Steps(_ context.Context, taskID uuid.UUID) ([]StepRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[taskID]
	if t == nil {
		return nil, nil
	}
	// Execution order is the task's step_ids order.
	out := make([]StepRecord, 0, len(t.StepIDs))
	for _, id := range t.StepIDs {
		if st := s.stepsByID[id]; st != nil {
			out = append(out, *st)
		}
	}
	return out, nil
}

func (s *MemStore) StartStep(_ context.Context, stepID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.stepsByID[stepID]; st != nil {
		st.Status = StatusRunning
		st.UpdatedAt = s.now()
	}
	return nil
}

func (s *MemStore) CompleteStep(_ context.Context, stepID uuid.UUID, result json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.stepsByID[stepID]; st != nil {
		st.Status = StatusSucceeded
		st.Result = result
		st.Error = nil
		st.UpdatedAt = s.now()
	}
	return nil
}

func (s *MemStore) FailStep(_ context.Context, taskID, stepID uuid.UUID, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := cause.Error()
	if st := s.stepsByID[stepID]; st != nil {
		st.Status = StatusFailed
		st.Error = &msg
		st.UpdatedAt = s.now()
	}
	if t := s.tasks[taskID]; t != nil {
		t.Status = StatusFailed
		t.Error = &msg
		t.UpdatedAt = s.now()
	}
	return nil
}

func (s *MemStore) CompleteTask(_ context.Context, taskID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.tasks[taskID]; t != nil {
		t.Status = StatusSucceeded
		t.Error = nil
		t.UpdatedAt = s.now()
	}
	return nil
}

func (s *MemStore) Get(_ context.Context, id uuid.UUID) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[id]
	if t == nil {
		return nil, nil
	}
	return cloneTask(t), nil
}

// Compile-time assertion that MemStore satisfies the Store interface.
var _ Store = (*MemStore)(nil)

func cloneTask(t *Task) *Task {
	c := *t
	return &c
}
