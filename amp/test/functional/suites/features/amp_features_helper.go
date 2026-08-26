// This file carries the REAL-FEATURE functional tests: end-to-end tests that
// exercise yb-amp's actual product features through the controller REST API —
// branch copy-on-write isolation, the endpoint lifecycle, and (the headline)
// compute restart across shared storage. Each stands up a co-located
// YugabyteDB-in-k8s backing plus the amp control plane (the cheapest fully-k8s
// mode, reusing the same provisionYBK8s + deployAmp the amp_on_yb_k8s foundation
// test uses), then drives the resource ladder user → pool → cluster → branch →
// endpoint and asserts a specific product guarantee.
//
// The stand-up + guaranteed LIFO teardown (endpoints/branches retired by the
// cluster-delete cascade, then the amp namespace, then the backing namespace +
// PVCs) is shared here; the three tests differ only in their feature phases. They
// SKIP cleanly unless a k8s cluster is configured — the
// same gate the foundation tests use.
package features

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// roleReadWrite is the endpoint role the feature tests create (the API's
// single-writer role; a branch admits at most one).
const roleReadWrite = "read_write"

// sqlConn is the pooled-SQL surface the feature assertions drive against an amp
// compute endpoint. *pg.Client satisfies it, and a fake satisfies it in unit
// tests (no real Postgres).
type sqlConn interface {
	Exec(ctx context.Context, sql string, args ...any) (int64, error)
	QueryInt(ctx context.Context, sql string, args ...any) (int64, error)
	QueryString(ctx context.Context, sql string, args ...any) (string, error)
	Close()
}

// ampCtl is the controller-API subset the feature tests drive. *controller.Client
// satisfies it; a scripted fake satisfies it in unit tests, so the orchestration
// and teardown ordering are testable with no controller running.
type ampCtl interface {
	CreateUser(context.Context, controller.CreateUserRequest) (controller.User, error)
	CreateStoragePool(context.Context, controller.CreateStoragePoolRequest) (controller.StoragePool, error)
	CreateClusterAndWait(context.Context, string, controller.CreateClusterRequest) (controller.Task, error)
	AddBranchExtensionsAndWait(context.Context, string, string, string, controller.AddBranchExtensionsRequest) (controller.Task, error)
	ListClusters(context.Context, string, int, int) (controller.ClusterList, error)
	DeleteClusterAndWait(context.Context, string, string) (controller.Task, error)
	CreateBranchAndWait(context.Context, string, string, controller.CreateBranchRequest) (controller.Task, error)
	DeleteBranchAndWait(context.Context, string, string, string) (controller.Task, error)
	ListBranches(context.Context, string, string, int, int) (controller.BranchList, error)
	ListTasks(context.Context, controller.ListTasksOptions) (controller.TaskList, error)
	CreateEndpointAndWait(context.Context, string, string, string, controller.CreateEndpointRequest) (controller.Task, error)
	GetEndpoint(context.Context, string, string, string, string) (controller.Endpoint, error)
	ListAllEndpoints(context.Context, string, string, string) ([]controller.Endpoint, error)
	DeleteEndpointAndWait(context.Context, string, string, string, string) (controller.Task, error)

	// The settings PATCHes. Synchronous, unlike everything above: the row is
	// written and the endpoints are told before the call returns, and the reply
	// is each endpoint's account of what it did with the change.
	UpdateBranchSettings(context.Context, string, string, string, controller.UpdateSettingsRequest) (controller.SettingsUpdateResult, error)
	UpdateEndpointSettings(context.Context, string, string, string, string, controller.UpdateSettingsRequest) (controller.SettingsUpdateResult, error)

	// The async trio. Every method above waits out its task, which is what a
	// feature test wants — but a CONCURRENCY test has to hold two tasks in flight
	// at once, so it plans them itself (the 202 + Task) and joins on WaitTask
	// afterwards. See amp_endpoint_drop_race.go.
	DeleteBranch(context.Context, string, string, string) (controller.Task, string, error)
	CreateEndpoint(context.Context, string, string, string, controller.CreateEndpointRequest) (controller.Task, string, error)
	WaitTask(context.Context, string) (controller.Task, error)
}

// endpointConnector opens a SQL connection to an amp compute endpoint. The live
// wiring resolves the endpoint's address (or a fresh pod IP) into a pg.ConnInfo
// and connects; a fake returns a scripted sqlConn in unit tests.
type endpointConnector func(ctx context.Context, ep controller.Endpoint) (sqlConn, error)

// ampCluster holds the control-plane handles a feature test creates on top of a
// stood-up amp control plane.
type ampCluster struct {
	poolID   string
	projID   string
	branchID string // the cluster's default branch id
}

// ampFeatureRun is the shared state a feature test builds on the flow (backing +
// amp control plane already stood up).
type ampFeatureRun struct {
	f    *flow
	proj ampCluster
}

// runAmpFeature is the shared lifecycle behind the three feature tests: gate →
// provision the co-located YB backing → wait YSQL ready → deploy the amp control
// plane → create the cluster the feature builds on → the feature's own phases,
// with a guaranteed LIFO teardown (the cluster-delete cascade retires branches +
// endpoints, then the amp namespace, then the backing namespace + PVCs). The
// feature builder is handed the shared run so its phases can read the created
// cluster handles.
func runAmpFeature(t *core.T, feature func(a *ampFeatureRun) []phase) error {
	cfg := FromConfig(t.Options)
	flowLogf, rlog := newFlowLog(t.Masker())
	f := &flow{t: t, cfg: cfg, td: &teardownStack{}, logf: flowLogf, rlog: rlog}
	a := &ampFeatureRun{f: f}
	step := func(name string, fn func() error) error {
		return t.Step(name, func(*core.StepHandle) error { return fn() })
	}

	var phases []phase
	if cp, ok := ampEnvFrom(t.Env); ok {
		// ATTACH: the scheduler stood a standing amp control plane up (e.g.
		// sharedk8s, via `run --env <id>`). Source the amp handle from it and skip
		// self-provisioning a backing + amp — the feature builds its cluster-per-test
		// straight on the standing control plane's pool.
		f.ampEnv = cp
		phases = []phase{{"create_cluster", a.createCluster}}
	} else {
		// SELF-PROVISION (today's path): a fake/none/universe env exposes no amp
		// control plane, so stand our own up. Default to a YBA-provisioned
		// universe; fall back to the co-located ybk8s backing where YBA can't be
		// stood up (no quay/provider creds). Skip only
		// when neither backing is available. This gate MUST NOT skip an attached
		// run (handled above, before the gate).
		ybaReason, ybaOK := cfg.gateYBAProvisioned(t.Options)
		k8sReason, k8sOK := cfg.gateYBK8s(t.Options)
		switch {
		case ybaOK:
			f.prov = cfg.provisionYBAProvisioned
		case k8sOK:
			f.logf("YBA-provisioned backing unavailable (%s); using co-located ybk8s backing", ybaReason)
			f.prov = cfg.provisionYBK8s
		default:
			t.Skip(ybaReason + "; ybk8s fallback also unavailable: " + k8sReason)
			return nil
		}
		if reason, ok := gateNoDoubleProvision(t.Env); !ok {
			t.Skip(reason)
			return nil
		}
		f.reg = openRunReapRegistry(f.logf)
		phases = []phase{
			{"provision_backing", f.provisionBacking},
			{"wait_backing_ready", f.waitBackingReady},
			{"deploy_amp", f.deployAmp},
			{"create_cluster", a.createCluster},
		}
	}
	phases = append(phases, feature(a)...)
	return runLifecycle(t.Context, step, f.td, phases, f.logf, f.collectDiagnostics, f.onTeardownDone)
}

// createCluster creates the user → pool → cluster the feature builds on, and
// registers the cluster-delete cascade cleanup (which retires the feature's
// branches and endpoints at teardown, before the amp namespace is deleted).
func (a *ampFeatureRun) createCluster(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	name := "feat-" + a.f.suffix()
	// Attach mode: the standing control plane already has a pool (created by the
	// provider); create the cluster-per-test straight on it. Self-provision mode:
	// run the full user → pool → cluster ladder on our own fresh amp.
	var proj ampCluster
	if a.f.ampEnv != nil {
		proj, err = createClusterOnPool(ctx, cl, a.f.ampEnv.AmpPoolID(), name, a.f.cfg.PgVersion, a.f.register)
	} else {
		proj, err = setupAmpCluster(ctx, cl, name, a.f.cfg.PgVersion, a.f.register)
	}
	if err != nil {
		return err
	}
	a.proj = proj
	return nil
}

// setupAmpCluster runs the user → pool → cluster ladder and registers the
// cluster-delete cascade cleanup, returning the cluster handles. It is seam-pure
// (an ampCtl and a register callback), so the ladder and its teardown
// registration are unit-testable with fakes.
func setupAmpCluster(ctx context.Context, cl ampCtl, name string, pgVersion int, register func(string, func(context.Context) error)) (ampCluster, error) {
	user, err := cl.CreateUser(ctx, controller.CreateUserRequest{IsAdmin: true})
	if err != nil {
		return ampCluster{}, fmt.Errorf("create amp user: %w", err)
	}
	pool, err := cl.CreateStoragePool(ctx, controller.CreateStoragePoolRequest{Name: name, OwnerUserID: user.ID})
	if err != nil {
		return ampCluster{}, fmt.Errorf("create amp storage pool: %w", err)
	}
	return createClusterOnPool(ctx, cl, pool.ID, name, pgVersion, register)
}

// createClusterOnPool creates a cluster on an EXISTING pool and registers its
// cluster-delete cascade cleanup, returning the cluster handles. It is the tail of
// setupAmpCluster, split out so attach mode (which reuses the standing control
// plane's pool) and self-provision mode (which creates its own pool first) share
// the identical cluster-create + teardown-registration logic. Seam-pure (an ampCtl
// + register callback), so both paths are unit-testable with fakes.
func createClusterOnPool(ctx context.Context, cl ampCtl, poolID, name string, pgVersion int, register func(string, func(context.Context) error)) (ampCluster, error) {
	if _, err := cl.CreateClusterAndWait(ctx, poolID, controller.CreateClusterRequest{Name: name, PgVersion: pgVersion}); err != nil {
		return ampCluster{}, fmt.Errorf("create amp cluster: %w", err)
	}
	proj, err := findClusterCtl(ctx, cl, poolID, name)
	if err != nil {
		return ampCluster{}, err
	}
	register("delete amp cluster", func(ctx context.Context) error {
		_, e := cl.DeleteClusterAndWait(ctx, poolID, proj.ID)
		return e
	})
	return ampCluster{poolID: poolID, projID: proj.ID, branchID: proj.DefaultBranch.BranchID}, nil
}

// classifyEndpointCreate marks an abandoned wait as an environment failure
// rather than a product one.
//
// A create_endpoint task that FAILS is the product refusing to bring an endpoint
// up. A wait that runs out of context is not: on this cluster an endpoint pod
// requests 40Gi ephemeral against ~43.8Gi allocatable, so exactly one fits per
// node and a cold endpoint waits on a freshly autoscaled node — 10 to 19 minutes
// observed, against a warm median of 18s. Four tests in nightly 0.260807.1
// reported "endpoint never came up" for endpoints the controller log shows were
// created and deployed minutes after the test stopped watching.
//
// The two arrive at the call site as the same error, so the task's own status is
// the only thing that separates them.
func classifyEndpointCreate(ctx context.Context, cl ampCtl, clusterID string, err error) error {
	if ctx.Err() == nil || hasFailedEndpointTask(ctx, cl, clusterID) {
		return err
	}
	return core.AsEnvFailure(fmt.Errorf("%w — no create_endpoint task failed, so the endpoint was still being provisioned when this wait ran out (a cold endpoint needs a fresh node); this is the cluster being slow, not the product refusing", err))
}

// hasFailedEndpointTask reports whether any create_endpoint task for the cluster
// actually failed, as opposed to still being in flight.
func hasFailedEndpointTask(ctx context.Context, cl ampCtl, clusterID string) bool {
	// The caller's ctx is typically already expired, which is the whole reason we
	// are here — so this lookup gets its own budget.
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	failed, err := cl.ListTasks(lctx, controller.ListTasksOptions{
		ClusterID: clusterID, Kind: "create_endpoint", Status: controller.TaskFailed, Limit: 10,
	})
	if err != nil {
		return false
	}
	return failed.Total > 0
}

// createEndpoint deploys a read_write endpoint on the addressed branch and
// returns it, converging on the controller's non-idempotent create: a failed
// helm deploy ("services yb-ep-… not found") can leave a partial record whose
// deploy finishes moments later, so on that transient we back off and REUSE the
// branch's now-present endpoint instead of re-creating (the same pattern
// sharedk8s.createEndpoint uses).
func createEndpoint(ctx context.Context, cl ampCtl, poolID, projID, branchID string) (controller.Endpoint, error) {
	return createEndpointReq(ctx, cl, poolID, projID, branchID, controller.CreateEndpointRequest{Role: roleReadWrite})
}

// createEndpointReq is createEndpoint with the caller's own create request (an
// extension opt-in, an explicit reservation), carrying the same convergence.
func createEndpointReq(ctx context.Context, cl ampCtl, poolID, projID, branchID string, req controller.CreateEndpointRequest) (controller.Endpoint, error) {
	var lastErr error
	for attempt := 0; attempt <= 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return controller.Endpoint{}, ctx.Err()
			case <-time.After(time.Duration(attempt) * 3 * time.Second):
			}
			if ep, ok := lastEndpoint(ctx, cl, poolID, projID, branchID); ok {
				return ep, nil
			}
		}
		if _, err := cl.CreateEndpointAndWait(ctx, poolID, projID, branchID, req); err != nil {
			lastErr = fmt.Errorf("create endpoint on branch %s: %w", branchID, err)
			if transientEndpointDeploy(err) {
				continue
			}
			return controller.Endpoint{}, classifyEndpointCreate(ctx, cl, projID, lastErr)
		}
		if ep, ok := lastEndpoint(ctx, cl, poolID, projID, branchID); ok {
			return ep, nil
		}
		return controller.Endpoint{}, fmt.Errorf("endpoint create returned no endpoint on branch %s", branchID)
	}
	return controller.Endpoint{}, fmt.Errorf("create endpoint failed after retries: %w", lastErr)
}

// lastEndpoint returns the branch's newest endpoint (a branch admits one
// read_write endpoint; after a delete+recreate the retired one is gone).
func lastEndpoint(ctx context.Context, cl ampCtl, poolID, projID, branchID string) (controller.Endpoint, bool) {
	eps, err := cl.ListAllEndpoints(ctx, poolID, projID, branchID)
	if err != nil || len(eps) == 0 {
		return controller.Endpoint{}, false
	}
	return eps[len(eps)-1], true
}

// transientEndpointDeploy reports the non-idempotent helm-deploy race that
// clears once the racing deploy finishes.
func transientEndpointDeploy(err error) bool {
	s := err.Error()
	return strings.Contains(s, "services") && strings.Contains(s, "not found")
}

// findClusterCtl resolves an asynchronously-created cluster by name (its id is not
// known until the create task succeeds). Mirrors findCluster over the ampCtl seam.
func findClusterCtl(ctx context.Context, cl ampCtl, poolID, name string) (controller.Cluster, error) {
	const page = 100
	for offset := 0; ; offset += page {
		list, err := cl.ListClusters(ctx, poolID, page, offset)
		if err != nil {
			return controller.Cluster{}, fmt.Errorf("list clusters: %w", err)
		}
		for _, p := range list.Clusters {
			if p.Name == name {
				return p, nil
			}
		}
		if list.Count < page || offset+len(list.Clusters) >= list.Total {
			return controller.Cluster{}, fmt.Errorf("cluster %q not found after create", name)
		}
	}
}

// findBranchCtl resolves an asynchronously-created branch by name.
func findBranchCtl(ctx context.Context, cl ampCtl, poolID, projID, name string) (controller.Branch, error) {
	const page = 100
	for offset := 0; ; offset += page {
		list, err := cl.ListBranches(ctx, poolID, projID, page, offset)
		if err != nil {
			return controller.Branch{}, fmt.Errorf("list branches: %w", err)
		}
		for _, b := range list.Branches {
			if b.Name == name {
				return b, nil
			}
		}
		if list.Count < page || offset+len(list.Branches) >= list.Total {
			return controller.Branch{}, fmt.Errorf("branch %q not found after create", name)
		}
	}
}
