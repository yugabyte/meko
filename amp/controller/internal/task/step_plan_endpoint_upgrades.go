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

// StepPlanEndpointUpgrades is the task_step.kind for the plan_endpoint_upgrades
// step — the fan-out parent of a fleet roll.
const StepPlanEndpointUpgrades = "plan_endpoint_upgrades"

// rollUpgraded is this roll's success word; the rest of the outcomes, the
// report line and the bounded pool are shared (endpointroll.go).
const rollUpgraded = "upgraded"

// PlanEndpointUpgradesArgs are the inputs to the plan_endpoint_upgrades step.
// EndpointIDs empty means the whole fleet; MaxParallel is validated rather than
// defaulted here, so a zero is rejected instead of read as unbounded.
type PlanEndpointUpgradesArgs struct {
	Version     string      `json:"version"`
	EndpointIDs []uuid.UUID `json:"endpoint_ids,omitempty"`
	MaxParallel int         `json:"max_parallel"`
}

// planEndpointUpgradesStep is the fleet roll: one self-contained step that owns N
// `upgrade_endpoint` children and runs them itself (ac-endpoint-upgrade.md §5).
//
// Self-contained for the same reason delete_cluster is — the set is not known
// until Execute time, and the worker has no dynamic sub-step expansion. It is the
// only step holding a resource lock over work it does not do itself:
// `fleet:endpoints` while its children each take `endpoint:<id>`, which is why
// the one-lock rule became a lock ORDER (ac-locking.md).
type planEndpointUpgradesStep struct {
	PlanEndpointUpgradesArgs
	state    StateStore
	subtasks Subtasks
	deployer deploy.Deployer
	// tasks re-reads this roll's own task row, which is the only way a running
	// step sees a cancel (see TaskReader). Nil disables the poll rather than
	// failing: every other roll dep is required because the work cannot be done
	// without it, and a roll that cannot be cancelled still converges.
	tasks TaskReader
}

// NewPlanEndpointUpgradesStep binds plan_endpoint_upgrades args + its deps into
// an executable step.
func NewPlanEndpointUpgradesStep(args PlanEndpointUpgradesArgs, deps Deps) Step {
	return &planEndpointUpgradesStep{
		PlanEndpointUpgradesArgs: args,
		state:                    deps.State,
		subtasks:                 deps.Subtasks,
		deployer:                 deps.Deployer,
		tasks:                    deps.Tasks,
	}
}

func buildPlanEndpointUpgradesStep(raw json.RawMessage, deps Deps) (Step, error) {
	var args PlanEndpointUpgradesArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode plan_endpoint_upgrades args: %w", err)
	}
	return NewPlanEndpointUpgradesStep(args, deps), nil
}

func (*planEndpointUpgradesStep) Kind() string { return StepPlanEndpointUpgrades }

func (s *planEndpointUpgradesStep) Execute(ctx context.Context) StepResult {
	// Refuse the whole roll rather than fan out N children that each fail on the
	// same missing dep, as delete_cluster refuses its cascade.
	if s.deployer == nil {
		return Fail(fmt.Errorf("plan_endpoint_upgrades step requires a deployer, but none is configured"))
	}
	if s.subtasks == nil {
		return Fail(fmt.Errorf("plan_endpoint_upgrades step requires a subtask runner, but none is configured"))
	}
	if s.Version == "" {
		return Fail(fmt.Errorf("plan_endpoint_upgrades step requires a target version, but none was given"))
	}
	if s.MaxParallel < 1 || s.MaxParallel > MaxRollParallel {
		return Fail(fmt.Errorf("plan_endpoint_upgrades max_parallel is %d, want 1..%d",
			s.MaxParallel, MaxRollParallel))
	}
	// Children hang off the parent's task id and run under the owner every task
	// write is fenced on. A reclaimed parent re-stamps them with the NEW owner,
	// which is what fences out a predecessor.
	run, ok := runningFrom(ctx)
	if !ok {
		return Fail(fmt.Errorf("plan_endpoint_upgrades step must run under a task worker"))
	}
	logger := applog.FromContext(ctx)

	subs, done, err := s.plan(ctx, run.taskID)
	if err != nil {
		return Fail(err)
	}
	rolled, cancelled := rollSubtasks(ctx, s.subtasks, run.owner, subs, s.MaxParallel, rollUpgraded,
		func() bool { return cancelRequested(ctx, s.tasks, run.taskID) })
	lines := append(done, rolled...)
	sort.Slice(lines, func(i, j int) bool { return lines[i].EndpointID < lines[j].EndpointID })

	report, counts := rollReport(s.Version, lines)
	logger.Info("endpoint upgrade roll finished", "version", s.Version,
		rollUpgraded, counts[rollUpgraded], rollSkipped, counts[rollSkipped],
		rollFailed, counts[rollFailed], rollUnattempted, counts[rollUnattempted])
	if cancelled {
		// Wrapped in ErrCancelled so the worker records "an operator stopped this"
		// rather than "this broke"; the counts say how far it got, since a failed
		// step records its cause and not its result.
		return Fail(fmt.Errorf("%w: endpoint upgrade roll to %s stopped after %d upgraded, %d skipped, %d failed",
			ErrCancelled, s.Version, counts[rollUpgraded], counts[rollSkipped], counts[rollFailed]))
	}
	if counts[rollFailed] > 0 || counts[rollUnattempted] > 0 {
		// The report is lost on this path — a failed step records its cause, not its
		// output — so the error carries the counts and the first failure. The
		// per-endpoint detail stays durable in the children's own rows.
		return Fail(rollFailure(s.Version, lines, counts))
	}
	// A roll that succeeded can still leave the fleet short — it rolled a named
	// subset, or an endpoint appeared behind it — which AdvanceFleetVersion's re-read
	// catches. The frozen target rather than the controller's current pin: a
	// controller upgraded mid-roll has moved the pin past what this roll deployed.
	//
	// Logged, not failed: every endpoint did move, and failing would drop this
	// report and tell every reader otherwise. A missed record only lags; the next
	// roll, which skips every endpoint, records it.
	if err := core.AdvanceFleetVersion(ctx, s.state, s.Version); err != nil {
		logger.Warn("record fleet version after the roll", "version", s.Version, "error", err)
	}
	return Ok(report)
}

// targets reads the endpoints the roll acts on, plus a report line for each
// caller-named id that resolved to nothing. A named set is read by id, which
// keeps an address-less endpoint attemptable; the whole fleet excludes those.
func (s *planEndpointUpgradesStep) targets(ctx context.Context) ([]core.EndpointDeploy, []*endpointRoll, error) {
	if len(s.EndpointIDs) == 0 {
		fleet, err := s.state.AllEndpointDeploys(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("enumerate endpoints: %w", err)
		}
		return fleet, nil, nil
	}
	// Deduplicated because Subtasks.Create converges on a derived id: a repeated
	// id would yield two work-list entries pointing at ONE row, and two runners
	// over one row hold one lease between them (§5.2).
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

// plan records a child per endpoint that needs one. It returns the children to
// run, and the finished lines for endpoints that need no child.
func (s *planEndpointUpgradesStep) plan(ctx context.Context, taskID uuid.UUID) ([]*Subtask, []*endpointRoll, error) {
	targets, done, err := s.targets(ctx)
	if err != nil {
		return nil, nil, err
	}
	explicit := len(s.EndpointIDs) > 0

	subs := make([]*Subtask, 0, len(targets))
	for _, d := range orderRoll(targets) {
		// Whole-fleet enumeration only: naming an endpoint is the operator's remedy
		// after a partial roll, and skipping a named one would drop exactly the
		// endpoint that needs re-running. Safe on a replay because `version` is what
		// the endpoint is RUNNING — a child that failed its deploy never wrote it, so
		// the endpoint still reads as not-at-target.
		if !explicit && d.Version == s.Version {
			done = append(done, &endpointRoll{
				EndpointID: d.ID.String(), Outcome: rollSkipped,
				Reason: "already recorded at " + s.Version,
			})
			continue
		}

		// Through the typed struct, never hand-assembled: the child's id is derived
		// from the args BYTES, so a replay that marshalled them differently would
		// create a second child — and a second deploy (subtask.go).
		args, err := json.Marshal(UpgradeEndpointArgs{EndpointID: d.ID, Version: s.Version})
		if err != nil {
			return nil, nil, fmt.Errorf("marshal upgrade args for endpoint %s: %w", d.ID, err)
		}
		sub, err := s.subtasks.Create(ctx, taskID, StepUpgradeEndpoint, args)
		if err != nil {
			return nil, nil, fmt.Errorf("record upgrade subtask for endpoint %s: %w", d.ID, err)
		}
		// Progress on a replay comes from the CHILD's status and nothing else (§5.5).
		// Pending, running and failed children are all re-run — the step idempotency
		// contract covers the running one.
		if sub.Status == StatusSucceeded {
			done = append(done, &endpointRoll{
				EndpointID: d.ID.String(), Outcome: rollUpgraded,
				Reason: "already upgraded by this roll", SubtaskID: sub.ID.String(),
			})
			continue
		}
		subs = append(subs, sub)
	}
	return subs, done, nil
}

// orderRoll puts the work set in the order the pool consumes it: round-robin
// across clusters, and within a cluster the readers before the writer.
//
// Both matter because the pool is bounded, not because of any staging. The
// enumeration comes back grouped by cluster, so `max_parallel` endpoints in
// flight would otherwise all belong to one cluster — every one of a single
// customer's endpoints down at the same moment. Round-robin makes that the
// pathological case rather than the default one, and writer-last means a
// branch's spare read capacity moves before the thing every write goes through.
//
// Ties break on the endpoint id and cluster order is the cluster ids', so a
// reclaimed parent re-derives the identical sequence.
func orderRoll(members []core.EndpointDeploy) []core.EndpointDeploy {
	byCluster := make(map[uuid.UUID][]core.EndpointDeploy, len(members))
	clusters := make([]uuid.UUID, 0, len(members))
	for _, m := range members {
		if _, seen := byCluster[m.ClusterID]; !seen {
			clusters = append(clusters, m.ClusterID)
		}
		byCluster[m.ClusterID] = append(byCluster[m.ClusterID], m)
	}
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].String() < clusters[j].String() })
	for _, id := range clusters {
		g := byCluster[id]
		sort.Slice(g, func(i, j int) bool {
			if a, b := writerLast(g[i].Role), writerLast(g[j].Role); a != b {
				return a < b
			}
			return g[i].ID.String() < g[j].ID.String()
		})
	}
	out := make([]core.EndpointDeploy, 0, len(members))
	for i := 0; len(out) < len(members); i++ {
		for _, id := range clusters {
			if g := byCluster[id]; i < len(g) {
				out = append(out, g[i])
			}
		}
	}
	return out
}

// writerLast ranks a role within its cluster: the read_write endpoint is the last
// thing the roll takes down.
func writerLast(role string) int {
	if role == core.RoleReadWrite {
		return 1
	}
	return 0
}

// rollReport marshals the per-endpoint report and tallies the outcomes.
func rollReport(version string, lines []*endpointRoll) (json.RawMessage, map[string]int) {
	counts := rollTally(lines, rollUpgraded)
	return mustResult(map[string]any{
		"version":     version,
		"endpoints":   lines,
		"upgraded":    counts[rollUpgraded],
		"skipped":     counts[rollSkipped],
		"failed":      counts[rollFailed],
		"unattempted": counts[rollUnattempted],
	}), counts
}

// rollFailure is the cause a roll that did not converge fails with. It carries
// the tally and the first failure, since the report itself is not persisted for a
// failed step; the whole per-endpoint account is in the children's own rows.
func rollFailure(version string, lines []*endpointRoll, counts map[string]int) error {
	first := ""
	for _, l := range lines {
		if l.Outcome == rollFailed {
			first = fmt.Sprintf("; first failure: endpoint %s (subtask %s): %s", l.EndpointID, l.SubtaskID, l.Reason)
			break
		}
	}
	return fmt.Errorf("endpoint upgrade roll to %s did not converge: %d upgraded, %d skipped, %d failed, "+
		"%d unattempted (see the task's subtasks for each endpoint)%s",
		version, counts[rollUpgraded], counts[rollSkipped], counts[rollFailed], counts[rollUnattempted], first)
}
