package task

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Subtask is one row of the `subtasks` table: a child a fan-out parent task owns
// and runs itself. It is ONE registered step with durable status, not a task with
// a step list — Kind is a STEP kind and Status is the step lifecycle.
//
// It is deliberately not in `tasks`, so the claim loop cannot see it and nothing
// can claim a child out from under the parent already running it
// (architecture/ac-endpoint-upgrade.md §5.2).
type Subtask struct {
	ID       uuid.UUID
	ParentID uuid.UUID
	Kind     string
	Status   Status
	Args     json.RawMessage
	Result   json.RawMessage
	Error    *string
	// Owner is the PARENT's owner, stamped when the child starts, so every child
	// write is fenced on what the parent's task lease says.
	Owner     *string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Holder is a unit of work a resource lease is held for. lockFor stamps its id
// and kind onto the lease owner, so a lock row names what holds it.
//
// The methods are not called ID/Kind because both implementations carry fields of
// those names.
type Holder interface {
	HolderID() uuid.UUID
	HolderKind() string
}

func (t *Task) HolderID() uuid.UUID { return t.ID }

func (t *Task) HolderKind() string { return t.Kind }

// A child is keyed on its OWN id, never its parent's: two children of one parent
// would otherwise compute an identical lease owner, and pglease grants
// unconditionally to the current holder (ac-endpoint-upgrade.md §5.2).
func (s *Subtask) HolderID() uuid.UUID { return s.ID }

func (s *Subtask) HolderKind() string { return s.Kind }

var (
	_ Holder = (*Task)(nil)
	_ Holder = (*Subtask)(nil)
)

// subtaskID derives a child's id from its parent and the work it describes, so a
// reclaimed parent re-creating its children converges on the existing rows
// through the primary key instead of minting duplicate children — and duplicate
// deploys. Same shape as the Voyager run id (voyager_tasks.go).
//
// It hashes the args BYTES, so two Creates agree only if they marshal the same
// value the same way. encoding/json is deterministic for a fixed-shape struct and
// one planner produces both the original and the replay, so they do; a caller that
// hand-assembled the JSON differently on a replay would create a second child.
func subtaskID(parentID uuid.UUID, kind string, args json.RawMessage) uuid.UUID {
	return uuid.NewSHA1(parentID, fmt.Appendf(nil, "subtask:%s:%s", kind, args))
}

// subtaskArgs normalizes args to what is stored, so the id is derived from the
// same bytes the row will hold.
func subtaskArgs(args json.RawMessage) json.RawMessage {
	if len(args) == 0 {
		return json.RawMessage("{}")
	}
	return args
}

// SubtaskStore persists the subtasks a fan-out parent owns. Every method is
// idempotent per the YB retry contract.
//
// The three transitions are fenced on the PARENT task's owner, as Store.StartStep
// and friends are fenced on the task's. A fenced-out write is a silent no-op: a
// process that has lost the parent has nothing to report, and erroring would fail
// work another instance is legitimately doing.
type SubtaskStore interface {
	// Create records a subtask under parentID, pending. A replayed parent gets back
	// the row it already created, at whatever status it has since reached — which is
	// how it reconciles what it did before it was reclaimed.
	Create(ctx context.Context, parentID uuid.UUID, kind string, args json.RawMessage) (*Subtask, error)

	// ByParent returns a parent's subtasks, oldest first.
	ByParent(ctx context.Context, parentID uuid.UUID) ([]*Subtask, error)

	// Start marks a subtask running and stamps owner on it. Re-stamping fences out
	// a process whose parent was reclaimed while it limped along (§5.3).
	Start(ctx context.Context, id uuid.UUID, owner string) error

	// Complete marks a subtask succeeded and records its result.
	Complete(ctx context.Context, id uuid.UUID, owner string, result json.RawMessage) error

	// Fail marks the SUBTASK failed and only the subtask: the parent fails through
	// its own step result, which reports what succeeded, failed and was never
	// attempted (§5.4).
	Fail(ctx context.Context, id uuid.UUID, owner string, cause error) error
}
