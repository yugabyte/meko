package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
	applog "github.com/yugabyte/amp/controller/internal/log"
)

// StepPlanEndpointUpgrades is the task_step.kind for the plan_endpoint_upgrades
// step — the fan-out parent of a fleet roll.
const StepPlanEndpointUpgrades = "plan_endpoint_upgrades"

const (
	// DefaultUpgradeParallel is how many endpoints a roll moves at once when the
	// caller does not say. Small on purpose: an upgrade replaces the pod, so this
	// is how much of the fleet is down at a time.
	DefaultUpgradeParallel = 4

	// MaxUpgradeParallel bounds it. Children run in-process off ONE execution
	// slot, so this is the only cap on a roll's concurrency.
	MaxUpgradeParallel = 50
)

// The outcome of one endpoint in a roll, as reported on the step result.
const (
	rollUpgraded = "upgraded"
	rollSkipped  = "skipped"
	rollFailed   = "failed"
	// rollUnattempted is work the roll did not do and does not claim failed: a
	// child halted by fail-fast, and one whose endpoint lock was held (§5.4).
	rollUnattempted = "unattempted"
)

// PlanEndpointUpgradesArgs are the inputs to the plan_endpoint_upgrades step.
// EndpointIDs empty means the whole fleet; MaxParallel is validated rather than
// defaulted here, so a zero is rejected instead of read as unbounded.
type PlanEndpointUpgradesArgs struct {
	Version     string      `json:"version"`
	EndpointIDs []uuid.UUID `json:"endpoint_ids,omitempty"`
	MaxParallel int         `json:"max_parallel"`
}

// endpointRoll is one line of the roll's report.
type endpointRoll struct {
	EndpointID string `json:"endpoint_id"`
	Outcome    string `json:"outcome"`
	Reason     string `json:"reason,omitempty"`
	// SubtaskID names the `subtasks` row the child ran as — the id in the lease
	// owner that held the endpoint. Empty when no child was created.
	SubtaskID string `json:"subtask_id,omitempty"`
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
}

// NewPlanEndpointUpgradesStep binds plan_endpoint_upgrades args + its deps into
// an executable step.
func NewPlanEndpointUpgradesStep(args PlanEndpointUpgradesArgs, deps Deps) Step {
	return &planEndpointUpgradesStep{
		PlanEndpointUpgradesArgs: args,
		state:                    deps.State,
		subtasks:                 deps.Subtasks,
		deployer:                 deps.Deployer,
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
	if s.MaxParallel < 1 || s.MaxParallel > MaxUpgradeParallel {
		return Fail(fmt.Errorf("plan_endpoint_upgrades max_parallel is %d, want 1..%d",
			s.MaxParallel, MaxUpgradeParallel))
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
	lines := append(done, s.roll(ctx, run.owner, subs)...)
	sort.Slice(lines, func(i, j int) bool { return lines[i].EndpointID < lines[j].EndpointID })

	report, counts := rollReport(s.Version, lines)
	logger.Info("endpoint upgrade roll finished", "version", s.Version,
		rollUpgraded, counts[rollUpgraded], rollSkipped, counts[rollSkipped],
		rollFailed, counts[rollFailed], rollUnattempted, counts[rollUnattempted])
	if counts[rollFailed] > 0 || counts[rollUnattempted] > 0 {
		// The report is lost on this path — a failed step records its cause, not its
		// output — so the error carries the counts and the first failure. The
		// per-endpoint detail stays durable in the children's own rows.
		return Fail(rollFailure(s.Version, lines, counts))
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
	for _, d := range targets {
		// The already-at-target filter applies to whole-fleet enumeration ONLY:
		// naming an endpoint is the operator's remedy after a partial roll, and
		// skipping a named one would drop exactly the endpoint that needs re-running.
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

// roll runs the children through a bounded pool and returns a line for each.
//
// A pool, not waves: a child starts as soon as a slot frees, which is what lets
// fail-fast halt precisely (§5.4).
//
// FAIL-FAST: the first real failure stops NEW children from starting; the ones
// already running finish, because killing a child mid-`helm --wait` leaves a
// release the next roll has to clean up. A child refused its endpoint lock is not
// a real failure — ErrLockHeld says somebody else holds the endpoint, not that
// the upgrade would fail.
func (s *planEndpointUpgradesStep) roll(ctx context.Context, owner string, subs []*Subtask) []*endpointRoll {
	out := make(chan *endpointRoll, len(subs))
	sem := make(chan struct{}, s.MaxParallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	halted := false

	// rest reports the children from i on, which never started.
	rest := func(i int, reason string) {
		for _, sub := range subs[i:] {
			out <- &endpointRoll{
				EndpointID: endpointOf(sub), Outcome: rollUnattempted,
				Reason: reason, SubtaskID: sub.ID.String(),
			}
		}
	}

	for i := range subs {
		// Take a slot BEFORE reading `halted`, so the check happens as late as it
		// can: a slot frees only when a child finishes, and a failing child records
		// its outcome before releasing.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			rest(i, "not started: the roll was interrupted")
			break
		}
		mu.Lock()
		stop := halted
		mu.Unlock()
		if stop {
			<-sem
			rest(i, "not started: the roll stopped after an earlier failure")
			break
		}

		wg.Add(1)
		go func(sub *Subtask) {
			defer wg.Done()
			defer func() { <-sem }()
			line := &endpointRoll{EndpointID: endpointOf(sub), SubtaskID: sub.ID.String()}
			err := s.subtasks.Run(ctx, sub, owner)
			mu.Lock()
			switch {
			case err == nil:
				line.Outcome = rollUpgraded
			case errors.Is(err, ErrLockHeld):
				line.Outcome = rollUnattempted
				line.Reason = "the endpoint lock was held by another task: " + err.Error()
			default:
				line.Outcome = rollFailed
				line.Reason = err.Error()
				halted = true
			}
			mu.Unlock()
			out <- line
		}(subs[i])
	}
	wg.Wait()
	close(out)

	lines := make([]*endpointRoll, 0, len(subs))
	for line := range out {
		lines = append(lines, line)
	}
	return lines
}

// endpointOf reads the endpoint a child was created for back out of its args, so
// a report line names the endpoint rather than only the child.
func endpointOf(sub *Subtask) string {
	var args UpgradeEndpointArgs
	if err := json.Unmarshal(sub.Args, &args); err != nil {
		return ""
	}
	return args.EndpointID.String()
}

// rollReport marshals the per-endpoint report and tallies the outcomes.
func rollReport(version string, lines []*endpointRoll) (json.RawMessage, map[string]int) {
	counts := map[string]int{rollUpgraded: 0, rollSkipped: 0, rollFailed: 0, rollUnattempted: 0}
	for _, l := range lines {
		counts[l.Outcome]++
	}
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
