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

	pitrTable = "amp_gc_pitr"
	// pitrRetention is waited out once, so it trades against the test's budget.
	pitrRetention = 45 * time.Second
	// pitrSlack keeps the protected LSN off the window's edge and absorbs clock
	// skew between the coordinator and the pageserver.
	pitrSlack = 20 * time.Second
	// pitrRounds writes enough WAL that the space horizon alone would pass the
	// protected LSN, so the first assertion is about pitr, not an idle branch.
	pitrRounds = 2
)

// pitrCutoffPageserverSettings put the branch under time-based retention: once
// the timestamp resolves, find_gc_cutoffs overwrites the space horizon with it.
var pitrCutoffPageserverSettings = map[string]any{
	"tenant_config.enable_gc":        true,
	"tenant_config.pitr_interval":    pitrRetention.String(),
	"tenant_config.lsn_lease_length": "2s",
}

// forkCutoffPageserverSettings make the applied cutoff reachable in a test
// window: pitr_interval defaults to 1 day and overwrites the space horizon once
// resolved, and lsn_lease_length doubles as GC's startup grace.
var forkCutoffPageserverSettings = map[string]any{
	"tenant_config.pitr_interval":    "0s",
	"tenant_config.lsn_lease_length": "2s",
}

// errNoPageserverAccess marks a deployment that cannot reach a pageserver's
// management port at all: no kube context, no pod, or nothing listening. That is
// an environment condition. An HTTP error FROM the pageserver is not.
var errNoPageserverAccess = errors.New("pageserver management API unreachable")

// errNoKubeContext is the structural half of the above: no retry can help.
var errNoKubeContext = errors.New("no kube context to reach a pageserver through")

func init() {
	core.Register(core.Test{
		Name:        "features/amp_fork_below_gc_cutoff",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "The GC cutoff and what it makes legal. Under a test-sized pitr_interval an LSN committed inside the window survives a forced GC at a 1 KiB horizon and a fork at it is accepted, and it stops being protected once the window passes it; then force the ancestor's GC past a captured LSN and assert a fork at that reclaimed LSN is rejected at registration rather than registered and left unusable.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		// A doomed-endpoint deploy (6m) on top of three endpoint creates and the
		// pitr phase's wait cannot fit the category default; timing out
		// mid-classification reads as a hang.
		Timeout: 30 * time.Minute,
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
		return []phase{
			{"pitr_cutoff", a.pitrCutoff},
			{"fork_below_gc_cutoff", a.forkBelowGCCutoff},
		}
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
		return nil, fmt.Errorf("%w: %w: this control plane is reached by a direct URL, so there is no kube context to exec into endpoint %s's pod", errNoPageserverAccess, errNoKubeContext, ep.ID)
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

// pitrCutoff checks the time-based cutoff both ways: an LSN inside the window
// survives a forced GC and is forkable, and stops being protected once the
// window passes it. https://github.com/yugabyte/meko/issues/924
func (a *ampFeatureRun) pitrCutoff(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	// Its own cluster: the endpoint outlives this phase and a branch takes one
	// writer, and a branch with no endpoint has no fork point to fork from.
	proj, err := createClusterOnPool(ctx, cl, a.proj.poolID, "gc-pitr-"+a.f.suffix(), a.f.cfg.PgVersion, a.f.register)
	if err != nil {
		return fmt.Errorf("create the cluster for the pitr phase: %w", err)
	}
	return pitrCutoffSteps(ctx, cl, proj, "pitr-fork-"+a.f.suffix(), a.f.endpointSQL, a.f.endpointPageserver, a.f.logf, a.f.t.Skip)
}

func pitrCutoffSteps(ctx context.Context, cl ampCtl, proj ampCluster, forkName string, connect endpointConnector, findPS pageserverFinder, logf func(string, ...any), skip func(string)) error {
	ep, err := createEndpointReq(ctx, cl, proj.poolID, proj.projID, proj.branchID, controller.CreateEndpointRequest{
		Role:               roleReadWrite,
		PageserverSettings: pitrCutoffPageserverSettings,
	})
	if err != nil {
		return fmt.Errorf("create the pitr endpoint with a %s retention: %w", pitrRetention, err)
	}
	got, err := cl.GetEndpoint(ctx, proj.poolID, proj.projID, proj.branchID, ep.ID)
	if err != nil {
		return fmt.Errorf("read endpoint %s back: %w", ep.ID, err)
	}
	if len(got.PageserverSettings) == 0 {
		skip("this controller does not persist pageserver_settings, so the branch cannot be put under a test-sized pitr_interval")
		return nil
	}
	c, err := connect(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect the pitr endpoint: %w", err)
	}
	defer c.Close()
	ps, err := findPS(ctx, ep)
	if err != nil {
		if skipUnreachablePageserver(err, skip) {
			return nil
		}
		return err
	}
	branchID, err := c.QueryString(ctx, "SELECT current_setting('yb_amp.branch_id')")
	if err != nil {
		return fmt.Errorf("read the pitr endpoint's branch id: %w", err)
	}

	if _, err := c.Exec(ctx, "CREATE TABLE "+pitrTable+" (id int primary key, payload text)"); err != nil {
		return fmt.Errorf("create %s: %w", pitrTable, err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO "+pitrTable+" (id, payload) VALUES (1, 'anchor')"); err != nil {
		return fmt.Errorf("write the anchor row: %w", err)
	}
	// Inside the window for the first check, outside it for the second.
	protectedLSN, err := currentLSN(ctx, c)
	if err != nil {
		return err
	}
	protectedAt := time.Now()

	inside, err := pitrForcedCutoff(ctx, c, ps, branchID, pitrTable, pitrRounds)
	if err != nil {
		if skipUnreachablePageserver(err, skip) {
			return nil
		}
		return err
	}
	passed, err := cutoffPassed(inside.AppliedGCCutoffLSN, protectedLSN)
	if err != nil {
		return err
	}
	if passed {
		return fmt.Errorf("branch %s's applied GC cutoff is %s, past the LSN %s committed %s earlier, on a branch whose pitr_interval is %s: a resolved time cutoff is authoritative over the space horizon, so a forced GC at %d bytes must not reclaim inside the window. Point-in-time recovery to any moment in the last %s is already impossible on this branch (last record %s, min readable %s)",
			branchID, inside.AppliedGCCutoffLSN, protectedLSN, time.Since(protectedAt).Round(time.Second), pitrRetention,
			cutoffForceGCHorizonBytes, pitrRetention, inside.LastRecordLSN, inside.MinReadableLSN)
	}
	logf("pitr_cutoff: inside the %s window the applied cutoff is %s, still below the protected LSN %s (last record %s)",
		pitrRetention, inside.AppliedGCCutoffLSN, protectedLSN, inside.LastRecordLSN)

	// What the interval is FOR, and the sharpest check that GC has not
	// over-reclaimed: a cutoff that ran ahead surfaces here as a refusal. No
	// endpoint on it, since the fork phase covers the deploy-time consequence.
	if _, err := cl.CreateBranchAndWait(ctx, proj.poolID, proj.projID, controller.CreateBranchRequest{
		Name: forkName, ParentBranchID: proj.branchID, ForkPointLSN: protectedLSN,
	}); err != nil {
		return fmt.Errorf("forking branch %s at LSN %s, committed %s ago and so inside its %s pitr window, was refused. Time-based retention is what makes that fork point recoverable, so refusing it means the history it promises to keep is already gone (applied cutoff %s, min readable %s): %w",
			branchID, protectedLSN, time.Since(protectedAt).Round(time.Second), pitrRetention,
			inside.AppliedGCCutoffLSN, inside.MinReadableLSN, err)
	}
	if child, err := findBranchCtl(ctx, cl, proj.poolID, proj.projID, forkName); err == nil {
		if _, err := cl.DeleteBranchAndWait(ctx, proj.poolID, proj.projID, child.BranchID); err != nil {
			logf("pitr_cutoff: could not delete the in-window fork %s, leaving it to the cluster cascade: %v", forkName, err)
		}
	}

	// Waited from here, not from the protected commit: the churn above committed
	// after it, and leaving that churn inside the window pins the cutoff at the
	// protected LSN instead of past it.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(pitrRetention + pitrSlack):
	}

	outside, err := pitrForcedCutoff(ctx, c, ps, branchID, pitrTable, pitrRounds)
	if err != nil {
		if skipUnreachablePageserver(err, skip) {
			return nil
		}
		return err
	}
	// Reached, not passed: the cutoff is a commit LSN found by searching clog
	// page trailers, so it resolves to a boundary. Demanding one byte more failed
	// a run where retention worked (0/1516A28 -> 0/1554A98, captured 0/1554A98).
	advanced, err := cutoffPassed(outside.AppliedGCCutoffLSN, inside.AppliedGCCutoffLSN)
	if err != nil {
		return err
	}
	reached, err := cutoffReached(outside.AppliedGCCutoffLSN, protectedLSN)
	if err != nil {
		return err
	}
	if !advanced || !reached {
		// Same escape hatch the fork phase carries: a deployment that switched GC
		// off can never move a cutoff, and blaming wall-clock retention for that
		// sends whoever triages it after a bug that is not there.
		if samples, mErr := ps.Metrics(ctx); mErr == nil {
			if ticks, known := gcDrainDisabled(samples); known && ticks > 0 {
				skip(fmt.Sprintf("this deployment's pageserver is not running GC (%s{outcome=\"disabled\"} = %.0f), so no applied cutoff can move whatever the retention says",
					gcDrainTicksMetric, ticks))
				return nil
			}
		}
		return fmt.Errorf("branch %s's applied GC cutoff is %s: it was %s while the LSN %s was inside the window and that LSN was committed %s ago, past the branch's %s pitr_interval, with more WAL written and flushed since. The cutoff had to advance to at least it. A wall-clock retention that is not resolved to an LSN leaves the branch retaining history forever (last record %s, min readable %s)",
			branchID, outside.AppliedGCCutoffLSN, inside.AppliedGCCutoffLSN, protectedLSN,
			time.Since(protectedAt).Round(time.Second), pitrRetention,
			outside.LastRecordLSN, outside.MinReadableLSN)
	}
	logf("pitr_cutoff: %s after the commit the applied cutoff advanced %s -> %s, reaching the protected LSN %s",
		time.Since(protectedAt).Round(time.Second), inside.AppliedGCCutoffLSN, outside.AppliedGCCutoffLSN, protectedLSN)

	// The window moving does not license losing the live row.
	n, err := c.QueryInt(ctx, "SELECT count(*) FROM "+pitrTable+" WHERE id = 1")
	if err != nil {
		return fmt.Errorf("re-read the anchor row after the cutoff advanced: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("the anchor row is gone from %s after the GC cutoff advanced past it: reclaiming history below the cutoff must not touch the live page", pitrTable)
	}
	return nil
}

// pitrForcedCutoff churns, flushes and forces a GC, reporting the cutoff.
func pitrForcedCutoff(ctx context.Context, c sqlConn, ps pageserverAdmin, branchID, table string, rounds int) (pageserver.BranchInfo, error) {
	for round := range rounds {
		if _, err := c.Exec(ctx, "INSERT INTO "+table+" (id, payload) SELECT g, repeat('x', 512) FROM generate_series($1::int, $2::int) g",
			round*cutoffChurnRows+2, (round+1)*cutoffChurnRows+1); err != nil {
			return pageserver.BranchInfo{}, fmt.Errorf("pitr churn round %d insert: %w", round, err)
		}
		if _, err := c.Exec(ctx, "DELETE FROM "+table+" WHERE id > 1"); err != nil {
			return pageserver.BranchInfo{}, fmt.Errorf("pitr churn round %d delete: %w", round, err)
		}
		if _, err := c.Exec(ctx, "CHECKPOINT"); err != nil {
			return pageserver.BranchInfo{}, fmt.Errorf("pitr churn round %d checkpoint: %w", round, err)
		}
	}
	frontier, err := currentLSN(ctx, c)
	if err != nil {
		return pageserver.BranchInfo{}, err
	}
	if err := ps.FlushBranch(ctx, branchID, frontier); err != nil {
		return pageserver.BranchInfo{}, err
	}
	if _, err := ps.ForceGC(ctx, branchID, cutoffForceGCHorizonBytes); err != nil {
		return pageserver.BranchInfo{}, err
	}
	return ps.BranchInfo(ctx, branchID)
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

// cutoffReached reports whether the cutoff has reached the captured LSN.
func cutoffReached(cutoff, forkLSN string) (bool, error) {
	c, err := pageserver.ParseLSN(cutoff)
	if err != nil {
		return false, fmt.Errorf("read the applied GC cutoff: %w", err)
	}
	f, err := pageserver.ParseLSN(forkLSN)
	if err != nil {
		return false, fmt.Errorf("read the captured LSN: %w", err)
	}
	return c >= f, nil
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
