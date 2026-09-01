// Package task implements the controller's write-ahead task framework
// (amp-controller.md §8): every mutation is a durable, idempotent task
// built from ordered steps, persisted to YBDB so a crash resumes rather than
// corrupts. This package holds the stubs/skeleton the API mutation paths plug
// into; the real step catalog (create-cluster, branch, scale) lands later.
package task

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Status is the lifecycle state shared by tasks and steps.
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

// Task is one row of the `task` table: a single initiated operation. StepIDs is
// its steps in execution order; the steps themselves are StepRecord rows keyed by
// those ids.
type Task struct {
	ID        uuid.UUID
	Kind      string
	Status    Status
	ClusterID *uuid.UUID
	TargetRef *string
	Params    json.RawMessage
	StepIDs   []uuid.UUID
	Error     *string
	// Owner is the process that claimed the task, stamped once at claim. Its
	// expiry is not here: ownership is a lease in amp_registry.resource_token
	// (architecture/ac-locking.md), and a copy of the expiry would have to be
	// re-stamped on every renewal to stay true.
	Owner *string
	// YCP is the resource row and generation this task applies, when it was
	// planned from YCP desired state; nil for a task an operator or the UI planned
	// directly. The worker stamps it once every step has succeeded — see YCPTarget.
	YCP       *YCPTarget
	CreatedAt time.Time
	UpdatedAt time.Time
}

// YCPTarget is the resource row a task stamps with the YCP desired-state
// generation it applies, once every one of its steps has succeeded.
//
// It exists because a 202 means "task planned", not "task done": the reconcile
// agent used to record the generation as applied the moment the controller
// answered, so a task that failed afterwards left the resource unconverged with
// the agent reporting healthy forever (meko#985). Stamping from the worker, after
// the last step, makes the recorded generation mean the work actually landed —
// which is what lets the agent re-issue an update whose task failed.
//
// On the task rather than in a step's args so that "this task applies generation
// N to this resource" is a property of the task: queryable in the task log,
// returnable on GET /tasks/{id}, and impossible for a planner to half-wire.
type YCPTarget struct {
	// Resource is which table ID names: "endpoint" or "branch". Derived from the
	// task's Kind (see Kind.ycpResource), never passed by a planner.
	Resource   string
	ID         uuid.UUID
	Generation int64
}

// YCP resource values, the two kinds YCP drives as an update.
const (
	YCPResourceEndpoint = "endpoint"
	YCPResourceBranch   = "branch"
)

// StepRecord is one row of the `task_step` table: the persisted state of one
// idempotent step, keyed by its own id (referenced from Task.StepIDs, which fixes
// execution order). It is pure data — the executable Step (the interface below) is
// reconstructed from a record's Kind and Args by the Registry on claim and replay
// (§8.2).
type StepRecord struct {
	ID        uuid.UUID
	TaskID    uuid.UUID
	Kind      string
	Status    Status
	Args      json.RawMessage
	Result    json.RawMessage
	Error     *string
	UpdatedAt time.Time
}

// StepSpec is a step as planned: its kind and marshaled args, with no behavior and
// no deps. A CreateXTask builder produces a list of these; Store.Create persists
// them as task_step rows, and the worker later rebuilds the executable Step from
// the persisted kind+args via the Registry. It is deliberately *not* a Step — a
// planned step is never executed in place, so it needs neither deps nor logic.
type StepSpec struct {
	Kind string
	Args json.RawMessage
}

// TaskSpec is a task as planned: the task-level metadata plus its ordered steps.
// Store.Create persists it — assigning a step id per step — and returns the
// resulting Task. ID is normally zero (Store.Create assigns a fresh task id);
// planners whose step args must refer to the task before it is persisted (the
// migration launchers) fix it write-ahead, and Store.Create then honors it and
// converges on replay.
type TaskSpec struct {
	ID        uuid.UUID
	Kind      Kind
	ClusterID *uuid.UUID
	TargetRef *string
	Params    json.RawMessage
	// YCP is derived by Service.createWithID from the kind and the request's
	// generation, not set by planners. Nil for a task YCP did not drive.
	YCP   *YCPTarget
	Steps []StepSpec
}

// Step is one executable step in a task: a unit of work that carries its own
// typed args and its collaborators (both bound at construction — see the
// New…Step constructors). A plan is an ordered []Step (tasks.go: Service.CreateX
// builds the StepSpecs); the worker
// persists each as a StepRecord, then rebuilds and runs it via the Registry.
// Implementations must be idempotent: a step can be re-run after a crash or
// partial execution and must converge to the same result (§8). A step's deps are
// unexported fields, so json.Marshal(step) is exactly the args persisted to
// task_step.args, and the Registry decodes them back (re-binding deps).
type Step interface {
	// Kind is the step's task_step.kind, the key the Registry builds it under.
	Kind() string
	// Execute performs the step's effect (through the deps bound at construction)
	// and reports the outcome. It must not return a Go error: success and failure
	// are both StepResult (use Ok / Fail), so the executor has one path to record.
	Execute(ctx context.Context) StepResult
}

// StepResult is the outcome of executing a Step. Exactly one of Output (an
// informational, fixed-shape result persisted to task_step.result and read back
// via the task API) or Err (the cause of a failure) is meaningful, per Failed.
type StepResult struct {
	Output json.RawMessage
	Err    error
}

// Failed reports whether the step failed.
func (r StepResult) Failed() bool { return r.Err != nil }

// Ok is the success result carrying an informational output.
func Ok(output json.RawMessage) StepResult { return StepResult{Output: output} }

// Fail is the failure result carrying the cause.
func Fail(err error) StepResult { return StepResult{Err: err} }
