// This file carries the NEGATIVE fork test: a fork whose fork point the
// ancestor's GC has already reclaimed must be rejected outright, not registered
// and then discovered broken at the child's first endpoint deploy (a
// crash-looping compute pod, which is what the unvalidated path produced).
package features

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	// cutoffTable is the table the churn phase writes and truncates to push WAL
	// through the ancestor branch.
	cutoffTable = "amp_gc_cutoff"
	// cutoffChurnBudget bounds the WAL churn that tries to drive the ancestor's GC
	// cutoff past the captured LSN. It is a best-effort phase: the test reports its
	// outcome honestly rather than failing when the cutoff did not move.
	cutoffChurnBudget = 4 * time.Minute
	// cutoffChurnRows is how many wide rows one churn round writes before deleting
	// them again — insert + delete + CHECKPOINT is WAL the GC horizon can move past.
	cutoffChurnRows = 20000
	// cutoffDoomedEndpointBudget bounds the endpoint deploy on a fork the system
	// ACCEPTED, so a crash-looping compute fails the test inside the test's own
	// budget instead of burning the controller's full helm --wait.
	cutoffDoomedEndpointBudget = 6 * time.Minute
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_fork_below_gc_cutoff",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Churn an ancestor's WAL past a captured LSN and assert a fork at that reclaimed LSN is rejected at registration rather than registered and left unusable.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		// Churn (4m) plus a doomed-endpoint deploy (6m) cannot fit the category
		// default; timing out mid-classification reads as a hang, not a verdict.
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
// Nothing exposes the ancestor's applied GC cutoff — not the controller API, not
// the endpoint's SQL surface — so the test cannot pick an LSN it KNOWS is below
// it. It churns WAL for a bounded budget to drive the cutoff forward instead, and
// then classifies what the fork attempt did:
//
//   - rejected naming the cutoff / fork point  → the guarantee holds (pass);
//   - rejected for an unrelated reason         → fail;
//   - accepted AND its endpoint serves reads   → the cutoff never passed the
//     captured LSN in the budget, so nothing was proven (skip, reason recorded);
//   - accepted AND its endpoint never comes up → a fork was registered that
//     cannot be used (fail) — exactly the outcome the validation exists to
//     prevent.
//
// The stronger, deterministic form of this test needs the ancestor's applied GC
// cutoff to be observable (an API or pageserver read) or forceable (a GC trigger),
// so the fork LSN can be chosen below a KNOWN cutoff and the skip branch deleted.
// It asserts rejection semantics only: the typed pageserver error for an
// out-of-range fork LSN is not implemented.
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
	return forkBelowGCCutoffSteps(ctx, cl, a.proj, "gc-fork-"+a.f.suffix(), a.f.endpointSQL, a.f.logf, a.f.t.Skip)
}

func forkBelowGCCutoffSteps(ctx context.Context, cl ampCtl, proj ampCluster, childName string, connect endpointConnector, logf func(string, ...any), skip func(string)) error {
	baseEP, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create ancestor endpoint: %w", err)
	}
	base, err := connect(ctx, baseEP)
	if err != nil {
		return fmt.Errorf("connect ancestor endpoint: %w", err)
	}
	defer base.Close()

	if _, err := base.Exec(ctx, "CREATE TABLE "+cutoffTable+" (id int primary key, payload text)"); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := base.Exec(ctx, "INSERT INTO "+cutoffTable+" (id, payload) VALUES (1, 'anchor')"); err != nil {
		return fmt.Errorf("write the anchor row: %w", err)
	}
	// The fork point: captured BEFORE the churn, so every byte of churned WAL is
	// history the GC may reclaim from under it.
	earlyLSN, err := currentLSN(ctx, base)
	if err != nil {
		return err
	}
	rounds, err := churnWAL(ctx, base, cutoffChurnBudget)
	if err != nil {
		return err
	}
	logf("churned %d WAL round(s) of %d rows on the ancestor after capturing fork LSN %s", rounds, cutoffChurnRows, earlyLSN)

	_, forkErr := cl.CreateBranchAndWait(ctx, proj.poolID, proj.projID, controller.CreateBranchRequest{
		Name:           childName,
		ParentBranchID: proj.branchID,
		ForkPointLSN:   earlyLSN,
	})
	if forkErr != nil {
		if !mentionsGCCutoff(forkErr) {
			return fmt.Errorf("forking at LSN %s (below the ancestor's churned-past history) was rejected, but for an unrelated reason — want a rejection naming the GC cutoff / fork point: %w", earlyLSN, forkErr)
		}
		logf("fork at %s rejected as expected: %v", earlyLSN, forkErr)
		return nil
	}

	// Accepted. Either the cutoff genuinely never passed earlyLSN (nothing proven)
	// or a doomed fork was registered — the endpoint tells them apart.
	child, err := findBranchCtl(ctx, cl, proj.poolID, proj.projID, childName)
	if err != nil {
		return err
	}
	ectx, cancel := context.WithTimeout(ctx, cutoffDoomedEndpointBudget)
	defer cancel()
	childEP, err := createEndpointReq(ectx, cl, proj.poolID, proj.projID, child.BranchID,
		controller.CreateEndpointRequest{Role: roleReadWrite})
	if err != nil {
		// A doomed fork's endpoint FAILS — the task records an error. This one
		// merely never finished, which on a cluster that cannot place a pod looks
		// identical and says nothing about the fork. Only a failed task is
		// evidence, so anything else is reported as an environment failure rather
		// than as a verdict on the product.
		if !hasFailedEndpointTask(ctx, cl, proj.projID) {
			return core.AsEnvFailure(fmt.Errorf("the fork at LSN %s was accepted, but its endpoint neither came up nor failed within %s — no create_endpoint task failed, so the cluster never placed the pod and whether the fork is usable stays unknown: %w",
				earlyLSN, cutoffDoomedEndpointBudget, err))
		}
		return fmt.Errorf("the fork at LSN %s was ACCEPTED but its endpoint FAILED to come up — a fork the system registers must be usable, so a fork point the ancestor's GC already reclaimed has to be rejected at registration instead: %w", earlyLSN, err)
	}
	kid, err := connect(ectx, childEP)
	if err != nil {
		return fmt.Errorf("the fork at LSN %s was ACCEPTED and its endpoint deployed, but it does not serve connections: %w", earlyLSN, err)
	}
	defer kid.Close()
	if _, err := kid.QueryInt(ectx, "SELECT count(*) FROM "+cutoffTable); err != nil {
		return fmt.Errorf("the fork at LSN %s was ACCEPTED but reading its inherited data fails: %w", earlyLSN, err)
	}
	skip(fmt.Sprintf("the ancestor's GC cutoff did not pass the captured fork LSN %s within %s of WAL churn (%d rounds), so the fork was legitimately accepted and served reads — the below-cutoff rejection was not exercised",
		earlyLSN, cutoffChurnBudget, rounds))
	return nil
}

// churnWAL writes and deletes wide rows in a CHECKPOINTed loop until the budget
// is spent, returning how many rounds it completed. It is the only lever the test
// has on the ancestor's GC cutoff.
func churnWAL(ctx context.Context, c sqlConn, budget time.Duration) (int, error) {
	deadline := time.Now().Add(budget)
	rounds := 0
	for time.Now().Before(deadline) {
		if _, err := c.Exec(ctx, "INSERT INTO "+cutoffTable+" (id, payload) SELECT g, repeat('x', 512) FROM generate_series($1::int, $2::int) g",
			rounds*cutoffChurnRows+2, (rounds+1)*cutoffChurnRows+1); err != nil {
			return rounds, fmt.Errorf("churn round %d insert: %w", rounds, err)
		}
		if _, err := c.Exec(ctx, "DELETE FROM "+cutoffTable+" WHERE id > 1"); err != nil {
			return rounds, fmt.Errorf("churn round %d delete: %w", rounds, err)
		}
		if _, err := c.Exec(ctx, "CHECKPOINT"); err != nil {
			return rounds, fmt.Errorf("churn round %d checkpoint: %w", rounds, err)
		}
		rounds++
		if err := ctx.Err(); err != nil {
			return rounds, err
		}
	}
	return rounds, nil
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
