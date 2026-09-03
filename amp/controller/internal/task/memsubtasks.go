package task

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// MemSubtasks is the in-memory SubtaskStore: one MemStore's `subtasks` rows, a
// faithful model of PgSubtasks — a create that converges on the derived id, and
// the three transitions fenced on the parent task's owner.
//
// It hangs off MemStore, and shares its mutex, because the real table shares its
// database with `tasks`: the fence is an EXISTS against the parent, so a subtask
// store that could not see the tasks could not model it. Safe for concurrent use,
// which the runner needs — a parent runs up to max_parallel children at once.
type MemSubtasks struct {
	store *MemStore
}

// Subtasks returns this store's subtask table.
func (s *MemStore) Subtasks() *MemSubtasks { return &MemSubtasks{store: s} }

var _ SubtaskStore = (*MemSubtasks)(nil)

func (m *MemSubtasks) Create(_ context.Context, parentID uuid.UUID, kind string, args json.RawMessage) (*Subtask, error) {
	args = subtaskArgs(args)
	id := subtaskID(parentID, kind, args)
	s := m.store
	s.mu.Lock()
	defer s.mu.Unlock()
	// Mirror PgSubtasks' ON CONFLICT (id) DO NOTHING: a replay hands back the row
	// already there, at whatever status it has reached, rather than a second child
	// or a reset one.
	if existing := s.subtasks[id]; existing != nil {
		return cloneSubtask(existing), nil
	}
	now := s.now()
	sub := &Subtask{
		ID:        id,
		ParentID:  parentID,
		Kind:      kind,
		Status:    StatusPending,
		Args:      args,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.subtaskOrder = append(s.subtaskOrder, id)
	s.subtasks[id] = sub
	return cloneSubtask(sub), nil
}

func (m *MemSubtasks) ByParent(_ context.Context, parentID uuid.UUID) ([]*Subtask, error) {
	s := m.store
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Subtask
	for _, id := range s.subtaskOrder {
		if sub := s.subtasks[id]; sub != nil && sub.ParentID == parentID {
			out = append(out, cloneSubtask(sub))
		}
	}
	return out, nil
}

// fencedLocked returns the subtask only when its parent is currently owned by
// owner — the mirror of PgSubtasks' EXISTS-on-tasks fence, and nil (a silent
// no-op for the caller) otherwise. Caller holds the store's mutex.
func (m *MemSubtasks) fencedLocked(id uuid.UUID, owner string) *Subtask {
	sub := m.store.subtasks[id]
	if sub == nil || !m.store.ownsLocked(sub.ParentID, owner) {
		return nil
	}
	return sub
}

func (m *MemSubtasks) Start(_ context.Context, id uuid.UUID, owner string) error {
	s := m.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if sub := m.fencedLocked(id, owner); sub != nil {
		sub.Status = StatusRunning
		sub.Owner = &owner
		sub.UpdatedAt = s.now()
	}
	return nil
}

func (m *MemSubtasks) Complete(_ context.Context, id uuid.UUID, owner string, result json.RawMessage) error {
	s := m.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if sub := m.fencedLocked(id, owner); sub != nil {
		sub.Status = StatusSucceeded
		sub.Result = result
		sub.Error = nil
		sub.UpdatedAt = s.now()
	}
	return nil
}

// Fail marks the child failed and leaves the parent alone, as PgSubtasks does.
func (m *MemSubtasks) Fail(_ context.Context, id uuid.UUID, owner string, cause error) error {
	s := m.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if sub := m.fencedLocked(id, owner); sub != nil {
		msg := cause.Error()
		sub.Status = StatusFailed
		sub.Error = &msg
		sub.UpdatedAt = s.now()
	}
	return nil
}

func cloneSubtask(s *Subtask) *Subtask {
	c := *s
	return &c
}
