// This file carries the REAL-FEATURE functional tests: end-to-end tests that
// exercise yb-amp's actual product features through the controller REST API —
// branch copy-on-write isolation, the endpoint lifecycle, and (the headline)
// compute restart across shared storage. Each stands up a co-located
// YugabyteDB-in-k8s backing plus the amp control plane (the cheapest fully-k8s
// mode, reusing the same provisionYBK8s + deployAmp the amp_on_yb_k8s foundation
// test uses), then drives the resource ladder user → pool → project → branch →
// endpoint and asserts a specific product guarantee.
//
// The stand-up + guaranteed LIFO teardown (endpoints/branches retired by the
// project-delete cascade, then the amp namespace, then the backing namespace +
// PVCs) is shared here; the three tests differ only in their feature phases. They
// SKIP cleanly unless functional.enabled AND a k8s cluster are configured — the
// same gate the foundation tests use.
package features

import (
	"context"
	"fmt"
	"log"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/yb-amp/test/functional/framework/core"
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
	CreateProjectAndWait(context.Context, string, controller.CreateProjectRequest) (controller.Task, error)
	ListProjects(context.Context, string, int, int) (controller.ProjectList, error)
	DeleteProjectAndWait(context.Context, string, string) (controller.Task, error)
	CreateBranchAndWait(context.Context, string, string, controller.CreateBranchRequest) (controller.Task, error)
	ListBranches(context.Context, string, string, int, int) (controller.BranchList, error)
	CreateEndpointAndWait(context.Context, string, string, string, controller.CreateEndpointRequest) (controller.Task, error)
	ListAllEndpoints(context.Context, string, string, string) ([]controller.Endpoint, error)
	DeleteEndpointAndWait(context.Context, string, string, string, string) (controller.Task, error)
}

// endpointConnector opens a SQL connection to an amp compute endpoint. The live
// wiring resolves the endpoint's address (or a fresh pod IP) into a pg.ConnInfo
// and connects; a fake returns a scripted sqlConn in unit tests.
type endpointConnector func(ctx context.Context, ep controller.Endpoint) (sqlConn, error)

// ampProject holds the control-plane handles a feature test creates on top of a
// stood-up amp control plane.
type ampProject struct {
	poolID   string
	projID   string
	branchID string // the project's default branch id
}

// ampFeatureRun is the shared state a feature test builds on the flow (backing +
// amp control plane already stood up).
type ampFeatureRun struct {
	f    *flow
	proj ampProject
}

// runAmpFeature is the shared lifecycle behind the three feature tests: gate →
// provision the co-located YB backing → wait YSQL ready → deploy the amp control
// plane → create the project the feature builds on → the feature's own phases,
// with a guaranteed LIFO teardown (the project-delete cascade retires branches +
// endpoints, then the amp namespace, then the backing namespace + PVCs). The
// feature builder is handed the shared run so its phases can read the created
// project handles.
func runAmpFeature(t *core.T, feature func(a *ampFeatureRun) []phase) error {
	cfg := FromConfig(t.Options)
	f := &flow{t: t, cfg: cfg, td: &teardownStack{}, logf: runFlowLogf(t.Masker(), log.Printf)}
	a := &ampFeatureRun{f: f}
	step := func(name string, fn func() error) error {
		return t.Step(name, func(*core.StepHandle) error { return fn() })
	}

	var phases []phase
	if cp, ok := ampEnvFrom(t.Env); ok {
		// ATTACH: the scheduler stood a standing amp control plane up (e.g.
		// sharedk8s, via `run --env <id>`). Source the amp handle from it and skip
		// self-provisioning a backing + amp — the feature builds its project-per-test
		// straight on the standing control plane's pool.
		f.ampEnv = cp
		phases = []phase{{"create_project", a.createProject}}
	} else {
		// SELF-PROVISION (today's path): a fake/none/universe env exposes no amp
		// control plane, so stand our own up. Gated exactly as the foundation tests
		// are — this gate MUST NOT skip an attached run (handled above, before the
		// gate), only a run that would have to self-provision without an amp env.
		if reason, ok := cfg.gateYBK8s(t.Options); !ok {
			t.Skip(reason)
			return nil
		}
		if reason, ok := gateNoDoubleProvision(t.Env); !ok {
			t.Skip(reason)
			return nil
		}
		f.prov = cfg.provisionYBK8s
		f.reg = openRunReapRegistry(f.logf)
		phases = []phase{
			{"provision_backing", f.provisionBacking},
			{"wait_backing_ready", f.waitBackingReady},
			{"deploy_amp", f.deployAmp},
			{"create_project", a.createProject},
		}
	}
	phases = append(phases, feature(a)...)
	return runLifecycle(t.Context, step, f.td, phases, f.logf, f.collectDiagnostics, f.onTeardownDone)
}

// createProject creates the user → pool → project the feature builds on, and
// registers the project-delete cascade cleanup (which retires the feature's
// branches and endpoints at teardown, before the amp namespace is deleted).
func (a *ampFeatureRun) createProject(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	name := "feat-" + a.f.suffix()
	// Attach mode: the standing control plane already has a pool (created by the
	// provider); create the project-per-test straight on it. Self-provision mode:
	// run the full user → pool → project ladder on our own fresh amp.
	var proj ampProject
	if a.f.ampEnv != nil {
		proj, err = createProjectOnPool(ctx, cl, a.f.ampEnv.AmpPoolID(), name, a.f.cfg.PgVersion, a.f.register)
	} else {
		proj, err = setupAmpProject(ctx, cl, name, a.f.cfg.PgVersion, a.f.register)
	}
	if err != nil {
		return err
	}
	a.proj = proj
	return nil
}

// setupAmpProject runs the user → pool → project ladder and registers the
// project-delete cascade cleanup, returning the project handles. It is seam-pure
// (an ampCtl and a register callback), so the ladder and its teardown
// registration are unit-testable with fakes.
func setupAmpProject(ctx context.Context, cl ampCtl, name string, pgVersion int, register func(string, func(context.Context) error)) (ampProject, error) {
	user, err := cl.CreateUser(ctx, controller.CreateUserRequest{IsAdmin: true})
	if err != nil {
		return ampProject{}, fmt.Errorf("create amp user: %w", err)
	}
	pool, err := cl.CreateStoragePool(ctx, controller.CreateStoragePoolRequest{Name: name, OwnerUserID: user.ID})
	if err != nil {
		return ampProject{}, fmt.Errorf("create amp storage pool: %w", err)
	}
	return createProjectOnPool(ctx, cl, pool.ID, name, pgVersion, register)
}

// createProjectOnPool creates a project on an EXISTING pool and registers its
// project-delete cascade cleanup, returning the project handles. It is the tail of
// setupAmpProject, split out so attach mode (which reuses the standing control
// plane's pool) and self-provision mode (which creates its own pool first) share
// the identical project-create + teardown-registration logic. Seam-pure (an ampCtl
// + register callback), so both paths are unit-testable with fakes.
func createProjectOnPool(ctx context.Context, cl ampCtl, poolID, name string, pgVersion int, register func(string, func(context.Context) error)) (ampProject, error) {
	if _, err := cl.CreateProjectAndWait(ctx, poolID, controller.CreateProjectRequest{Name: name, PgVersion: pgVersion}); err != nil {
		return ampProject{}, fmt.Errorf("create amp project: %w", err)
	}
	proj, err := findProjectCtl(ctx, cl, poolID, name)
	if err != nil {
		return ampProject{}, err
	}
	register("delete amp project", func(ctx context.Context) error {
		_, e := cl.DeleteProjectAndWait(ctx, poolID, proj.ID)
		return e
	})
	return ampProject{poolID: poolID, projID: proj.ID, branchID: proj.DefaultBranch.BranchID}, nil
}

// createEndpoint deploys a read_write endpoint on the addressed branch and
// returns it. The endpoint create is async (202 + Task); the *AndWait helper polls
// it to success, then the endpoint (with its resolved address) is read from the
// branch's endpoint list.
func createEndpoint(ctx context.Context, cl ampCtl, poolID, projID, branchID string) (controller.Endpoint, error) {
	if _, err := cl.CreateEndpointAndWait(ctx, poolID, projID, branchID, controller.CreateEndpointRequest{Role: roleReadWrite}); err != nil {
		return controller.Endpoint{}, fmt.Errorf("create endpoint on branch %s: %w", branchID, err)
	}
	eps, err := cl.ListAllEndpoints(ctx, poolID, projID, branchID)
	if err != nil {
		return controller.Endpoint{}, fmt.Errorf("list endpoints on branch %s: %w", branchID, err)
	}
	if len(eps) == 0 {
		return controller.Endpoint{}, fmt.Errorf("endpoint create returned no endpoint on branch %s", branchID)
	}
	// The newest endpoint (a branch admits one read_write endpoint; after a
	// delete+recreate the retired one is already gone from the list).
	return eps[len(eps)-1], nil
}

// findProjectCtl resolves an asynchronously-created project by name (its id is not
// known until the create task succeeds). Mirrors findProject over the ampCtl seam.
func findProjectCtl(ctx context.Context, cl ampCtl, poolID, name string) (controller.Project, error) {
	const page = 100
	for offset := 0; ; offset += page {
		list, err := cl.ListProjects(ctx, poolID, page, offset)
		if err != nil {
			return controller.Project{}, fmt.Errorf("list projects: %w", err)
		}
		for _, p := range list.Projects {
			if p.Name == name {
				return p, nil
			}
		}
		if list.Count < page || offset+len(list.Projects) >= list.Total {
			return controller.Project{}, fmt.Errorf("project %q not found after create", name)
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
