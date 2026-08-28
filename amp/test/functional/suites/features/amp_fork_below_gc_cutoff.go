// This file carries the NEGATIVE fork test: a fork whose fork point the
// ancestor's GC has already reclaimed must be rejected outright, not registered
// and then discovered broken at the child's first endpoint deploy (a
// crash-looping compute pod, which is what the unvalidated path produced).
package features

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pageserver"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	// cutoffTable is the table the churn phase writes and truncates to push WAL
	// through the ancestor branch.
	cutoffTable = "amp_gc_cutoff"
	// cutoffHistoryRounds is how many churn rounds run AFTER the fork LSN is
	// captured, so GC has history past it to reclaim.
	cutoffHistoryRounds = 3
	// cutoffChurnRows is how many wide rows one churn round writes before deleting
	// them again — insert + delete + CHECKPOINT is WAL the GC horizon can move past.
	cutoffChurnRows = 20000
	// cutoffForceGCHorizonBytes is the space-based retention the forced GC runs
	// with, small enough that the cutoff lands just behind the write frontier
	// (the amp integration suite forces GC the same way).
	cutoffForceGCHorizonBytes = 1024
	// cutoffDoomedEndpointBudget bounds the endpoint deploy on a fork the system
	// ACCEPTED, so a crash-looping compute fails the test inside the test's own
	// budget instead of burning the controller's full helm --wait.
	cutoffDoomedEndpointBudget = 6 * time.Minute
	// gcDrainTicksMetric counts the pageserver's GC drain ticks by outcome. The
	// "disabled" outcome means the tick reclaimed nothing because GC is switched
	// off — `enable_gc`, or the loop-level `gc_period`/`gc_horizon` at 0. None of
	// those is the stock configuration, so it marks a deployment that turned GC
	// off rather than a default.
	gcDrainTicksMetric = "pageserver_yb_gc_drain_ticks_total"
	// endpointPodContainer is the endpoint pod's amp container (the pod also
	// carries a postgres-exporter sidecar, and only this one answers on the
	// pageserver's management port).
	endpointPodContainer = "endpoint"
)

// forkCutoffPageserverSettings makes the ancestor's applied GC cutoff reachable
// inside a test window. GC itself is on by default; what blocks the cutoff is
// retention. `pitr_interval` defaults to 1 day and is authoritative over the
// space horizon whenever it is non-zero, so the forced GC's small gc_horizon
// cannot move the cutoff until it is 0. `lsn_lease_length` doubles as GC's
// startup grace (`lsn_lease_deadline = startup + lsn_lease_length`), so at the
// 10-minute default no tick in this test would run at all. Same trap
// `features/amp_gc_reclaim` documents.
var forkCutoffPageserverSettings = map[string]any{
	"tenant_config.pitr_interval":    "0s",
	"tenant_config.lsn_lease_length": "2s",
}

// errNoPageserverAccess marks a deployment that cannot reach a pageserver's
// management port at all: no kube context, no pod, or nothing listening. That is
// an environment condition. An HTTP error FROM the pageserver is not.
var errNoPageserverAccess = errors.New("pageserver management API unreachable")

func init() {
	core.Register(core.Test{
		Name:        "features/amp_fork_below_gc_cutoff",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Force the ancestor's GC past a captured LSN, then assert a fork at that reclaimed LSN is rejected at registration rather than registered and left unusable.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		// A doomed-endpoint deploy (6m) on top of two endpoint creates cannot fit
		// the category default; timing out mid-classification reads as a hang.
		Timeout: 20 * time.Minute,
		Run:     runAmpForkBelowGCCutoff,
	})
}

// runAmpForkBelowGCCutoff is the negative half of the fork-at-LSN coverage.
//
// The guarantee under test: registering a fork whose fork point is below the
// ancestor's applied GC cutoff is rejected (the create_branch task fails naming
// the cutoff), and no endpoint is ever deployed on such a branch.
//
// The fork LSN is captured first, more history is written past it, and the
// ancestor's GC is then FORCED so the cutoff moves deterministically instead of
// being churned at and hoped for. The pageserver reports the applied cutoff, so
// "the cutoff passed the fork LSN" is observed, not assumed. A rejection that
// does not name the cutoff or the fork point is still a failure: the fork has to
// be refused for the right reason. Only a FAILED task is evidence about the
// product: a pod the cluster never placed is an environment failure.
func runAmpForkBelowGCCutoff(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"fork_below_gc_cutoff", a.forkBelowGCCutoff}}
	})
}

func (a *ampFeatureRun) forkBelowGCCutoff(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	return forkBelowGCCutoffSteps(ctx, cl, a.proj, "gc-fork-"+a.f.suffix(), a.f.endpointSQL,
		a.f.endpointPageserver, a.f.logf, a.f.t.Skip)
}

// pageserverAdmin is the pageserver management surface this test drives.
type pageserverAdmin interface {
	BranchInfo(ctx context.Context, branchID string) (pageserver.BranchInfo, error)
	ForceGC(ctx context.Context, branchID string, gcHorizonBytes uint64) (pageserver.GCResult, error)
	FlushBranch(ctx context.Context, branchID, lsn string) error
	Metrics(ctx context.Context) (pageserver.Samples, error)
}

// pageserverFinder reaches the pageserver serving an endpoint's branch.
type pageserverFinder func(ctx context.Context, ep controller.Endpoint) (pageserverAdmin, error)

// endpointPageserver is the live pageserverFinder: the pageserver serving a
// branch runs inside that branch's endpoint pod, and its management port is
// container-local, so requests go over an exec into that pod.
func (f *flow) endpointPageserver(ctx context.Context, ep controller.Endpoint) (pageserverAdmin, error) {
	kube := f.ampKube()
	if kube == nil {
		return nil, fmt.Errorf("%w: this control plane is reached by a direct URL, so there is no kube context to exec into endpoint %s's pod", errNoPageserverAccess, ep.ID)
	}
	pod, err := (&kubeComputeRoller{kube: kube}).podName(ctx, ep.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errNoPageserverAccess, err)
	}
	return pageserver.New(podPageserverExec(kube, pod), pageserver.DefaultHTTPPort), nil
}

// podPageserverExec runs a pageserver request script inside the endpoint pod,
// separating a transport/shape failure from an error the pageserver returned.
func podPageserverExec(kube *k8s.Kubectl, pod string) pageserver.ExecFunc {
	return func(ctx context.Context, script string) (string, error) {
		res, err := kube.ForPod(pod, endpointPodContainer).Run(ctx, script)
		if err != nil {
			return "", fmt.Errorf("%w: exec in pod %s: %w", errNoPageserverAccess, pod, err)
		}
		switch res.ExitCode {
		case 0:
			return res.Stdout, nil
		case pageserver.ConnectRefusedExit:
			return "", fmt.Errorf("%w: nothing answers on the pageserver's management port in pod %s", errNoPageserverAccess, pod)
		default:
			return "", fmt.Errorf("pageserver request in pod %s exited %d: %s", pod, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
	}
}

func forkBelowGCCutoffSteps(ctx context.Context, cl ampCtl, proj ampCluster, childName string, connect endpointConnector, findPS pageserverFinder, logf func(string, ...any), skip func(string)) error {
	baseEP, err := createEndpointReq(ctx, cl, proj.poolID, proj.projID, proj.branchID, controller.CreateEndpointRequest{
		Role:               roleReadWrite,
		PageserverSettings: forkCutoffPageserverSettings,
	})
	if err != nil {
		return fmt.Errorf("create ancestor endpoint with retention pageserver_settings: %w", err)
	}
	base, err := connect(ctx, baseEP)
	if err != nil {
		return fmt.Errorf("connect ancestor endpoint: %w", err)
	}
	defer base.Close()

	ps, err := findPS(ctx, baseEP)
	if err != nil {
		if skipUnreachablePageserver(err, skip) {
			return nil
		}
		return err
	}
	// The pageserver addresses a branch by the id its own endpoint reports.
	branchID, err := base.QueryString(ctx, "SELECT current_setting('yb_amp.branch_id')")
	if err != nil {
		return fmt.Errorf("read the ancestor endpoint's branch id: %w", err)
	}

	if _, err := base.Exec(ctx, "CREATE TABLE "+cutoffTable+" (id int primary key, payload text)"); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := base.Exec(ctx, "INSERT INTO "+cutoffTable+" (id, payload) VALUES (1, 'anchor')"); err != nil {
		return fmt.Errorf("write the anchor row: %w", err)
	}
	// The fork point: captured BEFORE the churn, so every byte churned after it is
	// history GC may reclaim from under it.
	earlyLSN, err := currentLSN(ctx, base)
	if err != nil {
		return err
	}
	if err := churnWAL(ctx, base, cutoffHistoryRounds); err != nil {
		return err
	}
	frontierLSN, err := currentLSN(ctx, base)
	if err != nil {
		return err
	}
	// Ingest has to have reached the frontier before GC can reclaim up to it.
	if err := ps.FlushBranch(ctx, branchID, frontierLSN); err != nil {
		if skipUnreachablePageserver(err, skip) {
			return nil
		}
		return err
	}

	forced, err := ps.ForceGC(ctx, branchID, cutoffForceGCHorizonBytes)
	if err != nil {
		if skipUnreachablePageserver(err, skip) {
			return nil
		}
		return err
	}
	info, err := ps.BranchInfo(ctx, branchID)
	if err != nil {
		if skipUnreachablePageserver(err, skip) {
			return nil
		}
		return err
	}
	logf("forced GC on branch %s at gc_horizon %d bytes after %d churn round(s) of %d rows: %d candidate(s) dequeued, %d row(s) deleted; applied cutoff %s, min readable %s, last record %s, fork LSN %s",
		branchID, cutoffForceGCHorizonBytes, cutoffHistoryRounds, cutoffChurnRows,
		forced.CandidatesDequeued, forced.RowsDeleted,
		info.AppliedGCCutoffLSN, info.MinReadableLSN, info.LastRecordLSN, earlyLSN)

	passed, err := cutoffPassed(info.AppliedGCCutoffLSN, earlyLSN)
	if err != nil {
		return err
	}
	if !passed {
		samples, mErr := ps.Metrics(ctx)
		if mErr != nil {
			if skipUnreachablePageserver(mErr, skip) {
				return nil
			}
			return mErr
		}
		if ticks, known := gcDrainDisabled(samples); known && ticks > 0 {
			skip(fmt.Sprintf("this deployment's pageserver is not running GC (%s{outcome=\"disabled\"} = %.0f — enable_gc off, or gc_period/gc_horizon at 0), so branch %s's applied GC cutoff stays at %s whatever is forced and no fork point can be below it. None of those is the default, so this deployment turned GC off",
				gcDrainTicksMetric, ticks, branchID, info.AppliedGCCutoffLSN))
			return nil
		}
		return fmt.Errorf("branch %s's applied GC cutoff is %s, at or below the captured fork LSN %s, after %d churn round(s) of %d rows and a forced GC at gc_horizon %d bytes (last record %s, min readable %s, GC reported %d candidate(s) dequeued and %d row(s) deleted). The pageserver does not report GC as switched off and the endpoint was created with pitr_interval=0s, so the cutoff was expected to advance past the fork LSN and the below-cutoff rejection could not be exercised",
			branchID, info.AppliedGCCutoffLSN, earlyLSN, cutoffHistoryRounds, cutoffChurnRows, cutoffForceGCHorizonBytes,
			info.LastRecordLSN, info.MinReadableLSN, forced.CandidatesDequeued, forced.RowsDeleted)
	}

	_, forkErr := cl.CreateBranchAndWait(ctx, proj.poolID, proj.projID, controller.CreateBranchRequest{
		Name:           childName,
		ParentBranchID: proj.branchID,
		ForkPointLSN:   earlyLSN,
	})
	if forkErr != nil {
		if !mentionsGCCutoff(forkErr) {
			return fmt.Errorf("forking at LSN %s (below branch %s's applied GC cutoff %s) was rejected, but for an unrelated reason, want a rejection naming the GC cutoff / fork point: %w", earlyLSN, branchID, info.AppliedGCCutoffLSN, forkErr)
		}
		logf("fork at %s rejected as expected: %v", earlyLSN, forkErr)
		return nil
	}

	// ACCEPTED below a cutoff we observed. The endpoint says whether the fork is
	// merely useless or the cluster never got to place a pod.
	child, err := findBranchCtl(ctx, cl, proj.poolID, proj.projID, childName)
	if err != nil {
		return err
	}
	ectx, cancel := context.WithTimeout(ctx, cutoffDoomedEndpointBudget)
	defer cancel()
	childEP, err := createEndpointReq(ectx, cl, proj.poolID, proj.projID, child.BranchID,
		controller.CreateEndpointRequest{Role: roleReadWrite})
	if err != nil {
		// Only a FAILED create_endpoint task is evidence: a pod the cluster never
		// placed looks identical and says nothing about the fork.
		if !hasFailedEndpointTask(ctx, cl, proj.projID) {
			return core.AsEnvFailure(fmt.Errorf("the fork at LSN %s was accepted, but its endpoint neither came up nor failed within %s — no create_endpoint task failed, so the cluster never placed the pod and whether the fork is usable stays unknown: %w",
				earlyLSN, cutoffDoomedEndpointBudget, err))
		}
		return fmt.Errorf("the fork at LSN %s (below branch %s's applied GC cutoff %s) was ACCEPTED but its endpoint FAILED to come up. Such a fork point has to be rejected at registration instead: %w", earlyLSN, branchID, info.AppliedGCCutoffLSN, err)
	}
	kid, err := connect(ectx, childEP)
	if err != nil {
		return fmt.Errorf("the fork at LSN %s was ACCEPTED and its endpoint deployed, but it does not serve connections: %w", earlyLSN, err)
	}
	defer kid.Close()
	if _, err := kid.QueryInt(ectx, "SELECT count(*) FROM "+cutoffTable); err != nil {
		return fmt.Errorf("the fork at LSN %s was ACCEPTED but reading its inherited data fails: %w", earlyLSN, err)
	}
	return fmt.Errorf("forking at LSN %s was ACCEPTED and its endpoint serves reads, but branch %s's applied GC cutoff is %s, above the fork point, so that history is already reclaimed and the fork had to be rejected at registration", earlyLSN, branchID, info.AppliedGCCutoffLSN)
}

// skipUnreachablePageserver records an environment skip when the pageserver's
// management port cannot be reached at all, and reports whether it did. An HTTP
// error from the pageserver itself falls through to the caller as a failure.
func skipUnreachablePageserver(err error, skip func(string)) bool {
	if !errors.Is(err, errNoPageserverAccess) {
		return false
	}
	skip("cannot reach the pageserver's management API in this deployment, so the ancestor's GC cannot be forced or observed: " + err.Error())
	return true
}

// cutoffPassed reports whether the applied GC cutoff has advanced past the
// captured fork LSN.
func cutoffPassed(cutoff, forkLSN string) (bool, error) {
	c, err := pageserver.ParseLSN(cutoff)
	if err != nil {
		return false, fmt.Errorf("read the applied GC cutoff: %w", err)
	}
	f, err := pageserver.ParseLSN(forkLSN)
	if err != nil {
		return false, fmt.Errorf("read the captured fork LSN: %w", err)
	}
	return c > f, nil
}

// gcDrainDisabled reports the pageserver's count of GC ticks that reclaimed
// nothing because GC is switched off (`enable_gc`, or `gc_period`/`gc_horizon`
// at 0), and whether it exports that counter at all.
func gcDrainDisabled(s pageserver.Samples) (ticks float64, known bool) {
	return s.Value(gcDrainTicksMetric, map[string]string{"outcome": "disabled"})
}

// churnWAL writes and deletes wide rows in a CHECKPOINTed loop, leaving history
// past the fork LSN for GC to reclaim.
func churnWAL(ctx context.Context, c sqlConn, rounds int) error {
	for round := range rounds {
		if _, err := c.Exec(ctx, "INSERT INTO "+cutoffTable+" (id, payload) SELECT g, repeat('x', 512) FROM generate_series($1::int, $2::int) g",
			round*cutoffChurnRows+2, (round+1)*cutoffChurnRows+1); err != nil {
			return fmt.Errorf("churn round %d insert: %w", round, err)
		}
		if _, err := c.Exec(ctx, "DELETE FROM "+cutoffTable+" WHERE id > 1"); err != nil {
			return fmt.Errorf("churn round %d delete: %w", round, err)
		}
		if _, err := c.Exec(ctx, "CHECKPOINT"); err != nil {
			return fmt.Errorf("churn round %d checkpoint: %w", round, err)
		}
	}
	return nil
}

// mentionsGCCutoff reports a rejection that names why the fork point is invalid.
// It matches the reason, not a typed error: the fork-LSN validation surfaces as
// a message on the failed create_branch task, and the typed pageserver error for
// an out-of-range fork LSN does not exist yet.
func mentionsGCCutoff(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "gc cutoff") ||
		strings.Contains(s, "already reclaimed") ||
		(strings.Contains(s, "fork point") && strings.Contains(s, "below"))
}
