package task

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemStore is an in-memory Store for unit tests: a faithful model of the
// write-ahead task store — tasks and their ordered steps, an atomic claim under a
// lease (a task that is pending, or running with an expired lease, is claimable),
// and the step/task status transitions the worker drives. It lets the framework
// (and any package that creates tasks) be tested without a database; the real
// PgStore SQL is exercised by the integration suite. It mirrors core.MemBackend, the
// in-memory core.Backend the API tests use. Safe for concurrent use.
type MemStore struct {
	mu        sync.Mutex
	order     []uuid.UUID
	tasks     map[uuid.UUID]*Task
	stepsByID map[uuid.UUID]*StepRecord
	now       func() time.Time
	// The `subtasks` table, in the same store because the real one is in the same
	// database: a child's writes are fenced on its parent's owner, which lives in
	// `tasks`. Subtasks() is the SubtaskStore over it (memsubtasks.go).
	subtasks     map[uuid.UUID]*Subtask
	subtaskOrder []uuid.UUID
	// locks is the lock table, held here because the real one lives in the same
	// database as the tasks: Candidates has to be able to ask whether a running
	// task's lease is still held. Locks(owner) is one instance's view of it.
	locks *memLockTable
}

// NewMemStore returns an empty in-memory Store.
func NewMemStore() *MemStore {
	return &MemStore{
		tasks:     map[uuid.UUID]*Task{},
		stepsByID: map[uuid.UUID]*StepRecord{},
		subtasks:  map[uuid.UUID]*Subtask{},
		now:       time.Now,
		locks:     newMemLockTable(),
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
	// First-writer-wins (mirrors the PgStore partial unique indexes): reject a task
	// of a locked kind while a non-terminal one exists for the same target_ref. The
	// lock releases when that task reaches terminal. Neither store names a kind —
	// both read Kind.lockIndex — so the two cannot drift as kinds are added.
	if spec.Kind.lockIndex != "" && spec.TargetRef != nil {
		for _, t := range s.tasks {
			if t.Kind == spec.Kind.name && t.TargetRef != nil && *t.TargetRef == *spec.TargetRef &&
				(t.Status == StatusPending || t.Status == StatusRunning) {
				return nil, spec.Kind.inProgress
			}
		}
	}
	id := spec.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	// Mirror PgStore's ON CONFLICT behavior for a replayed preassigned id: keep
	// and return the original durable plan rather than generating replacement
	// steps under a second set of ids.
	if existing := s.tasks[id]; existing != nil {
		return cloneTask(existing), nil
	}
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
		Kind:      spec.Kind.name,
		YCP:       spec.YCP,
		Status:    StatusPending,
		ClusterID: spec.ClusterID,
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

// ActiveByKind mirrors PgStore's: the oldest pending-or-running task of kind, or
// nil. s.order is insertion order, which is created_at order here.
func (s *MemStore) ActiveByKind(_ context.Context, kind string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		t := s.tasks[id]
		if t.Kind == kind && (t.Status == StatusPending || t.Status == StatusRunning) {
			return cloneTask(t), nil
		}
	}
	return nil, nil
}

// Candidates mirrors PgStore's: pending tasks, plus running ones whose execution
// lease nobody holds, oldest first. The lock table it consults is this store's
// own, the way the real one lives in the same database as the tasks.
func (s *MemStore) Candidates(_ context.Context, limit int) ([]*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		return nil, nil // as LIMIT 0 does; a negative one is an error in SQL
	}
	var out []*Task
	for _, id := range s.order {
		t := s.tasks[id]
		switch {
		case t.Status == StatusPending:
		case t.Status == StatusRunning && s.locks.heldBy(taskLockKey(id)) == "":
		default:
			continue
		}
		out = append(out, cloneTask(t))
	}
	// Oldest first, as PgStore's ORDER BY created_at does. Insertion order is not a
	// substitute: without this a regression dropping the ORDER BY — losing FIFO
	// claiming and starving the oldest work — would still pass these tests.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID.String() < out[j].ID.String()
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return page(out, limit, 0), nil
}

func (s *MemStore) Claim(_ context.Context, taskID uuid.UUID, owner string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[taskID]
	if t == nil || (t.Status != StatusPending && t.Status != StatusRunning) {
		return nil, nil
	}
	t.Status = StatusRunning
	t.Owner = &owner
	t.UpdatedAt = s.now()
	return cloneTask(t), nil
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

// ownsLocked reports whether taskID exists and is currently owned by owner. The
// step/task mutations use it to mirror PgStore's owner fence: a worker that has
// lost the lease does not mutate a task another instance now owns. Caller holds
// s.mu.
func (s *MemStore) ownsLocked(taskID uuid.UUID, owner string) bool {
	t := s.tasks[taskID]
	return t != nil && t.Owner != nil && *t.Owner == owner
}

func (s *MemStore) StartStep(_ context.Context, taskID, stepID uuid.UUID, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ownsLocked(taskID, owner) {
		return nil
	}
	if st := s.stepsByID[stepID]; st != nil {
		st.Status = StatusRunning
		st.UpdatedAt = s.now()
	}
	return nil
}

func (s *MemStore) CompleteStep(_ context.Context, taskID, stepID uuid.UUID, owner string, result json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ownsLocked(taskID, owner) {
		return nil
	}
	if st := s.stepsByID[stepID]; st != nil {
		st.Status = StatusSucceeded
		st.Result = result
		st.Error = nil
		st.UpdatedAt = s.now()
	}
	return nil
}

func (s *MemStore) FailStep(_ context.Context, taskID, stepID uuid.UUID, owner string, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ownsLocked(taskID, owner) {
		return nil
	}
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

func (s *MemStore) CompleteTask(_ context.Context, taskID uuid.UUID, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ownsLocked(taskID, owner) {
		return nil
	}
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

// List returns a page of tasks matching f, ordered created_at DESC (newest
// first), plus the total matching count. It mirrors PgStore.List so the two
// stores paginate identically: ties on created_at break on id DESC.
func (s *MemStore) List(_ context.Context, f TaskFilter, limit, offset int) ([]*Task, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var matched []*Task
	for _, t := range s.tasks {
		if f.Status != "" && t.Status != f.Status {
			continue
		}
		if f.Kind != "" && t.Kind != f.Kind {
			continue
		}
		if f.ClusterID != nil && (t.ClusterID == nil || *t.ClusterID != *f.ClusterID) {
			continue
		}
		matched = append(matched, t)
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].ID.String() > matched[j].ID.String() // id DESC tiebreak
		}
		return matched[i].CreatedAt.After(matched[j].CreatedAt) // created_at DESC
	})
	total := len(matched)
	matched = page(matched, limit, offset)
	out := make([]*Task, len(matched))
	for i, t := range matched {
		out[i] = cloneTask(t)
	}
	return out, total, nil
}

// page returns the half-open window items[offset : offset+limit], clamped to the
// slice bounds. The API layer guarantees limit >= 1 and offset >= 0.
func page[T any](items []T, limit, offset int) []T {
	start := min(offset, len(items))
	end := min(start+limit, len(items))
	return items[start:end]
}

// Compile-time assertion that MemStore satisfies the Store interface.
var _ Store = (*MemStore)(nil)

func cloneTask(t *Task) *Task {
	c := *t
	return &c
}
