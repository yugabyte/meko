// Package task implements the controller's write-ahead task framework
// (amp-controller.md §8): every mutation is a durable, idempotent task
// built from ordered steps, persisted to YBDB so a crash resumes rather than
// corrupts. This package holds the stubs/skeleton the API mutation paths plug
// into; the real step catalog (create-database, branch, scale) lands later.
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

// Task is one row of the `task` table: a single initiated operation.
type Task struct {
	ID          uuid.UUID
	Kind        string
	Status      Status
	TenantID    *uuid.UUID
	TargetRef   *string
	Params      json.RawMessage
	Error       *string
	Owner       *string
	LeaseExpiry *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Step is one row of the `task_step` table: one ordered, idempotent step.
type Step struct {
	TaskID    uuid.UUID
	Ordinal   int
	Kind      string
	Status    Status
	Args      json.RawMessage
	Result    json.RawMessage
	Error     *string
	UpdatedAt time.Time
}

// StepPlan describes a step to be written write-ahead when a task is planned.
type StepPlan struct {
	Kind string
	Args json.RawMessage
}

// StepInput is handed to a Handler when its step runs.
type StepInput struct {
	Task Task
	Step Step
}

// Handler executes one kind of step. Implementations must be idempotent: a
// handler can be re-run after a crash or partial execution and must converge to
// the same result (§8).
type Handler interface {
	Kind() string
	Run(ctx context.Context, in StepInput) (json.RawMessage, error)
}
