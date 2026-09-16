package task

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
	applog "github.com/yugabyte/amp/controller/internal/log"
)

// StepPlanEndpointRerenders is the task_step.kind for the
// plan_endpoint_rerenders step — the fan-out parent of a re-render roll.
const StepPlanEndpointRerenders = "plan_endpoint_rerenders"

// rollRerendered is this roll's success word; the rest of the outcomes, the
// report line and the bounded pool are shared (endpointroll.go).
const rollRerendered = "rerendered"

// PlanEndpointRerendersArgs are the inputs to the plan_endpoint_rerenders step.
// EndpointIDs empty means the whole fleet; MaxParallel is validated rather than
// defaulted here, so a zero is rejected instead of read as unbounded.
type PlanEndpointRerendersArgs struct {
	EndpointIDs []uuid.UUID `json:"endpoint_ids,omitempty"`
	MaxParallel int         `json:"max_parallel"`
}

// planEndpointRerendersStep re-renders endpoints against the controller's
// current configuration: one self-contained step owning a rerender_endpoint
// child per endpoint, run through the shared bounded pool.
//
// There is no filter. Nothing on the endpoint row records which deployer
// configuration it was rendered with, so the step cannot tell an endpoint that
// already carries the current values from one that does not, and a re-render of
// a converged endpoint is a no-op Helm upgrade that does not recreate the pod.
// Recording a rendered-configuration fingerprint so a roll could skip is
// meko#1202.
type planEndpointRerendersStep struct {
	PlanEndpointRerendersArgs
	state    StateStore
	subtasks Subtasks
	deployer deploy.Deployer
}

// NewPlanEndpointRerendersStep binds plan_endpoint_rerenders args + its deps
// into an executable step.
func NewPlanEndpointRerendersStep(args PlanEndpointRerendersArgs, deps Deps) Step {
	return &planEndpointRerendersStep{
		PlanEndpointRerendersArgs: args,
		state:                     deps.State,
		subtasks:                  deps.Subtasks,
		deployer:                  deps.Deployer,
	}
}

func buildPlanEndpointRerendersStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args PlanEndpointRerendersArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode plan_endpoint_rerenders args: %w", err)
	}
	return NewPlanEndpointRerendersStep(args, deps), nil
}

func (*planEndpointRerendersStep) Kind() string { return StepPlanEndpointRerenders }

func (s *planEndpointRerendersStep) Execute(ctx context.Context) StepResult {
	if s.deployer == nil {
		return Fail(fmt.Errorf("plan_endpoint_rerenders step requires a deployer, but none is configured"))
	}
	if s.subtasks == nil {
		return Fail(fmt.Errorf("plan_endpoint_rerenders step requires a subtask runner, but none is configured"))
	}
	if s.MaxParallel < 1 || s.MaxParallel > MaxRollParallel {
		return Fail(fmt.Errorf("plan_endpoint_rerenders max_parallel is %d, want 1..%d",
			s.MaxParallel, MaxRollParallel))
	}
	run, ok := runningFrom(ctx)
	if !ok {
		return Fail(fmt.Errorf("plan_endpoint_rerenders step must run under a task worker"))
	}
	logger := applog.FromContext(ctx)

	subs, done, err := s.plan(ctx, run.taskID)
	if err != nil {
		return Fail(err)
	}
	lines := append(done, rollSubtasks(ctx, s.subtasks, run.owner, subs, s.MaxParallel, rollRerendered)...)
	sort.Slice(lines, func(i, j int) bool { return lines[i].EndpointID < lines[j].EndpointID })

	counts := rollTally(lines, rollRerendered)
	logger.Info("endpoint re-render roll finished",
		rollRerendered, counts[rollRerendered], rollSkipped, counts[rollSkipped],
		rollFailed, counts[rollFailed], rollUnattempted, counts[rollUnattempted])
	if counts[rollFailed] > 0 || counts[rollUnattempted] > 0 {
		return Fail(rerenderFailure(lines, counts))
	}
	return Ok(mustResult(map[string]any{
		"endpoints":       lines,
		rollRerendered:    counts[rollRerendered],
		rollSkipped:       counts[rollSkipped],
		rollFailed:        counts[rollFailed],
		rollUnattempted:   counts[rollUnattempted],
		"max_parallel":    s.MaxParallel,
		"named_endpoints": len(s.EndpointIDs),
	}))
}

// targets reads the endpoints the roll acts on, plus a report line for each
// caller-named id that resolved to nothing.
func (s *planEndpointRerendersStep) targets(ctx context.Context) ([]core.EndpointDeploy, []*endpointRoll, error) {
	if len(s.EndpointIDs) == 0 {
		fleet, err := s.state.AllEndpointDeploys(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("enumerate endpoints: %w", err)
		}
		return fleet, nil, nil
	}
	// Deduplicated because Subtasks.Create converges on a derived id: a repeated
	// id would yield two work-list entries pointing at ONE row, and two runners
	// over one row hold one lease between them.
	ids := make([]uuid.UUID, 0, len(s.EndpointIDs))
	seen := make(map[uuid.UUID]struct{}, len(s.EndpointIDs))
	for _, id := range s.EndpointIDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	found, err := s.state.EndpointDeploysByIDs(ctx, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("read named endpoints: %w", err)
	}
	if len(found) == len(ids) {
		return found, nil, nil
	}
	live := make(map[uuid.UUID]struct{}, len(found))
	for _, d := range found {
		live[d.ID] = struct{}{}
	}
	var gone []*endpointRoll
	for _, id := range ids {
		if _, ok := live[id]; !ok {
			gone = append(gone, &endpointRoll{
				EndpointID: id.String(), Outcome: rollSkipped, Reason: "no such endpoint",
			})
		}
	}
	return found, gone, nil
}

// plan records a rerender_endpoint child per target. It returns the children to
// run, and the finished lines for endpoints that need no child.
func (s *planEndpointRerendersStep) plan(ctx context.Context, taskID uuid.UUID) ([]*Subtask, []*endpointRoll, error) {
	targets, done, err := s.targets(ctx)
	if err != nil {
		return nil, nil, err
	}
	subs := make([]*Subtask, 0, len(targets))
	for _, d := range targets {
		// Through the typed struct, never hand-assembled: the child's id is derived
		// from the args BYTES, so a replay that marshalled them differently would
		// create a second child — and a second deploy (subtask.go).
		args, err := json.Marshal(RerenderEndpointArgs{EndpointID: d.ID})
		if err != nil {
			return nil, nil, fmt.Errorf("marshal rerender args for endpoint %s: %w", d.ID, err)
		}
		sub, err := s.subtasks.Create(ctx, taskID, StepRerenderEndpoint, args)
		if err != nil {
			return nil, nil, fmt.Errorf("record rerender subtask for endpoint %s: %w", d.ID, err)
		}
		// Progress on a replay comes from the CHILD's status: a re-render leaves
		// nothing on the endpoint row to re-derive it from.
		if sub.Status == StatusSucceeded {
			done = append(done, &endpointRoll{
				EndpointID: d.ID.String(), Outcome: rollRerendered,
				Reason: "already re-rendered by this roll", SubtaskID: sub.ID.String(),
			})
			continue
		}
		subs = append(subs, sub)
	}
	return subs, done, nil
}

// rerenderFailure is the cause a roll that did not converge fails with. It
// carries the tally and the first failure, since the report itself is not
// persisted for a failed step; the per-endpoint account is in the children's rows.
func rerenderFailure(lines []*endpointRoll, counts map[string]int) error {
	first := ""
	for _, l := range lines {
		if l.Outcome == rollFailed {
			first = fmt.Sprintf("; first failure: endpoint %s (subtask %s): %s", l.EndpointID, l.SubtaskID, l.Reason)
			break
		}
	}
	return fmt.Errorf("endpoint re-render roll did not converge: %d re-rendered, %d skipped, %d failed, "+
		"%d unattempted (see the task's subtasks for each endpoint)%s",
		counts[rollRerendered], counts[rollSkipped], counts[rollFailed], counts[rollUnattempted], first)
}
