package task

import (
	"encoding/json"
)

// This file anchors the step catalog (amp-controller.md §8): the re-usable,
// idempotent steps every task is composed of, one per step_<kind>.go file. Each
// step implements the Step interface (task.go): its constructor binds both its
// typed args and the narrow ports it needs (deps.go) — at plan time from a
// builder, or on replay from the persisted JSON + the Registry's deps — and
// Execute performs the effect (needing only a context) and returns a StepResult.
// Every Execute must converge when re-run after a crash or partial execution —
// the worker replays the first non-succeeded step on reclaim (§8.1, §8.2), so a
// step that already half-applied must finish cleanly the second time rather than
// double-apply or error.
//
// The step kinds are the task_step.kind values; the worker looks a step's kind
// up in the Registry (registry.go) to rebuild its Step. Each kind constant lives
// alongside its step in the step_<kind>.go file:
//
//	create_project        step_create_project.go
//	create_branch         step_create_branch.go
//	record_branch_recipe  step_record_branch_recipe.go
//	create_endpoint       step_create_endpoint.go
//	delete_endpoint       step_delete_endpoint.go
//	delete_branch         step_delete_branch.go
//	delete_project        step_delete_project.go

// mustResult marshals a step result. The values are small, fixed-shape maps, so
// marshaling cannot fail; a result is informational (read back via the task API)
// and never load-bearing, so a marshal error degrades to an empty result rather
// than failing an otherwise-successful step.
func mustResult(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
