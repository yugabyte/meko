// Synchronous refusals the spec promises and nothing asserted: disk_type,
// pg_version, an unknown pool owner; plus the two read paths every client
// touches first (GET /version, the users list) and what a repeated create does.
package features

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const unknownUUID = "00000000-0000-4000-8000-0000000000aa"

func init() {
	core.Register(core.Test{
		Name:        "features/amp_api_validation",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "gapcoverage"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Request validation the spec promises: disk_type=local is refused 400 as unsupported and an unknown disk_type 400, an unsupported pg_version 400, a pool for an unknown owner 404; GET /version carries a version and commit; the user the run created is listed and readable with its admin flag; GET of the default branch returns it with is_default and an unknown branch is 404; and a cluster or branch created twice under one name leaves exactly one of it (a 409 up front or a failed second task, never two rows).",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     35 * time.Minute,
		Run:         runAmpAPIValidation,
	})
}

func runAmpAPIValidation(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{
			{"request_refusals", a.requestRefusals},
			{"version_and_users", a.versionAndUsers},
			{"repeated_creates", a.repeatedCreates},
		}
	})
}

func (a *ampFeatureRun) requestRefusals(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	proj := a.proj
	_, _, err = cl.CreateEndpoint(ctx, proj.poolID, proj.projID, proj.branchID, controller.CreateEndpointRequest{Role: roleReadWrite, DiskType: "local"})
	if status := apiErrorStatus(err); status != http.StatusBadRequest {
		if err == nil {
			return fmt.Errorf("disk_type=local was accepted; the spec says it is not yet supported and refused 400 (an endpoint on a network volume was now created under a promise of local NVMe)")
		}
		return fmt.Errorf("disk_type=local returned HTTP %d (%v), want 400", status, err)
	}
	if !strings.Contains(err.Error(), "unsupported_disk_type") && !strings.Contains(err.Error(), "not yet supported") {
		return fmt.Errorf("the disk_type=local refusal does not say it is unsupported: %v", err)
	}
	_, _, err = cl.CreateEndpoint(ctx, proj.poolID, proj.projID, proj.branchID, controller.CreateEndpointRequest{Role: roleReadWrite, DiskType: "floppy"})
	if status := apiErrorStatus(err); status != http.StatusBadRequest {
		return fmt.Errorf("disk_type=floppy returned HTTP %d (%v), want 400", status, err)
	}
	if len(mustListEndpoints(ctx, cl, proj)) != 0 {
		return fmt.Errorf("a refused endpoint create left an endpoint row on the branch")
	}

	_, _, err = cl.CreateCluster(ctx, proj.poolID, controller.CreateClusterRequest{Name: "pgv-" + a.f.suffix(), PgVersion: 99})
	if status := apiErrorStatus(err); status != http.StatusBadRequest {
		if err == nil {
			return fmt.Errorf("pg_version=99 was accepted (a task was planned for a Postgres that does not exist)")
		}
		return fmt.Errorf("pg_version=99 returned HTTP %d (%v), want 400", status, err)
	}

	_, err = cl.CreateStoragePool(ctx, controller.CreateStoragePoolRequest{Name: "owner-" + a.f.suffix(), OwnerUserID: unknownUUID})
	if status := apiErrorStatus(err); status != http.StatusNotFound && status != http.StatusBadRequest {
		if err == nil {
			return fmt.Errorf("a pool for the unknown owner %s was created", unknownUUID)
		}
		return fmt.Errorf("a pool for an unknown owner returned HTTP %d (%v), want 404", status, err)
	}
	return nil
}

func (a *ampFeatureRun) versionAndUsers(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	v, err := cl.Version(ctx)
	if err != nil {
		return fmt.Errorf("GET /version: %w", err)
	}
	if strings.TrimSpace(v.Version) == "" {
		return fmt.Errorf("GET /version carries an empty version")
	}
	if strings.TrimSpace(v.GitCommit) == "" && v.Version != "dev" {
		return fmt.Errorf("GET /version %q carries no git_commit; a stamped build must say what it was built from", v.Version)
	}
	a.f.logf("controller %s (%s, %s)", v.Version, v.GitCommit, v.BuildDate)

	pool, err := cl.GetStoragePool(ctx, a.proj.poolID)
	if err != nil {
		return fmt.Errorf("read the run's pool: %w", err)
	}
	owner, err := cl.GetUser(ctx, pool.OwnerUserID)
	if err != nil {
		return fmt.Errorf("GET /users/%s (the pool's owner): %w", pool.OwnerUserID, err)
	}
	if owner.ID != pool.OwnerUserID {
		return fmt.Errorf("GET /users/%s returned user %s", pool.OwnerUserID, owner.ID)
	}
	found := false
	for offset := 0; ; offset += 100 {
		list, err := cl.ListUsers(ctx, 100, offset)
		if err != nil {
			return fmt.Errorf("list users: %w", err)
		}
		for _, u := range list.Users {
			if u.ID == owner.ID {
				found = true
				if u.IsAdmin != owner.IsAdmin {
					return fmt.Errorf("user %s is_admin=%v in the list but %v on get", u.ID, u.IsAdmin, owner.IsAdmin)
				}
			}
		}
		if found || list.Count < 100 || offset+len(list.Users) >= list.Total {
			break
		}
	}
	if !found {
		return fmt.Errorf("the pool owner %s is not in GET /users", owner.ID)
	}
	if _, err := cl.GetUser(ctx, unknownUUID); apiErrorStatus(err) != http.StatusNotFound {
		return fmt.Errorf("GET /users/%s returned %v, want 404", unknownUUID, err)
	}

	branches := "/storage-pools/" + a.proj.poolID + "/clusters/" + a.proj.projID + "/branches/"
	var b controller.Branch
	if _, err := cl.GetRaw(ctx, branches+a.proj.branchID, nil, &b); err != nil {
		return fmt.Errorf("GET the cluster's default branch: %w", err)
	}
	if b.BranchID != a.proj.branchID || b.ClusterID != a.proj.projID || !b.IsDefault {
		return fmt.Errorf("GET branch %s returned branch_id %q cluster_id %q is_default %v", a.proj.branchID, b.BranchID, b.ClusterID, b.IsDefault)
	}
	var none controller.Branch
	if _, err := cl.GetRaw(ctx, branches+unknownUUID, nil, &none); apiErrorStatus(err) != http.StatusNotFound {
		return fmt.Errorf("GET branch %s returned %v, want 404", unknownUUID, err)
	}
	return nil
}

// repeatedCreates: a duplicate name is refused up front or fails as a task;
// either way exactly one row exists afterwards. The cluster half needs no
// compute; the branch half forks at an explicit LSN, because a parent with no
// endpoint has no fork point the controller can resolve.
func (a *ampFeatureRun) repeatedCreates(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	proj := a.proj

	cluster := "dupc-" + a.f.suffix()
	if _, err := cl.CreateClusterAndWait(ctx, proj.poolID, controller.CreateClusterRequest{Name: cluster, PgVersion: a.f.cfg.PgVersion}); err != nil {
		return fmt.Errorf("first cluster %s: %w", cluster, err)
	}
	first, err := findClusterCtl(ctx, cl, proj.poolID, cluster)
	if err != nil {
		return err
	}
	a.f.register("delete duplicate-name cluster", func(ctx context.Context) error {
		_, e := cl.DeleteClusterAndWait(ctx, proj.poolID, first.ID)
		if apiErrorStatus(e) == http.StatusNotFound {
			return nil
		}
		return e
	})
	if _, err := cl.CreateClusterAndWait(ctx, proj.poolID, controller.CreateClusterRequest{Name: cluster, PgVersion: a.f.cfg.PgVersion}); err == nil {
		return fmt.Errorf("a second cluster named %s was created in the same pool", cluster)
	}
	if n := countClusters(ctx, cl, proj.poolID, cluster); n != 1 {
		return fmt.Errorf("%d clusters named %s exist (failed ones included) after a duplicate create, want 1", n, cluster)
	}

	ep, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create the writer the fork point comes from: %w", err)
	}
	conn, err := a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect the writer: %w", err)
	}
	lsn, err := currentLSN(ctx, conn)
	conn.Close()
	if err != nil {
		return err
	}
	branch := "dup-" + a.f.suffix()
	req := controller.CreateBranchRequest{Name: branch, ParentBranchID: proj.branchID, ForkPointLSN: lsn}
	if err := createBranchRetrying(ctx, cl, proj.poolID, proj.projID, req); err != nil {
		return fmt.Errorf("first fork %s: %w", branch, err)
	}
	if _, err := cl.CreateBranchAndWait(ctx, proj.poolID, proj.projID, req); err == nil {
		return fmt.Errorf("a second branch named %s was created on the same cluster", branch)
	}
	if n := countBranches(ctx, cl, proj, branch); n != 1 {
		return fmt.Errorf("%d branches named %s exist after a duplicate create, want 1", n, branch)
	}
	return nil
}

// countClusters counts the pool's clusters carrying name, failed ones included.
func countClusters(ctx context.Context, cl *controller.Client, poolID, name string) int {
	n := 0
	for offset := 0; ; offset += 100 {
		list, err := cl.ListClustersIncludingFailed(ctx, poolID, 100, offset)
		if err != nil {
			return -1
		}
		for _, c := range list.Clusters {
			if c.Name == name {
				n++
			}
		}
		if list.Count < 100 || offset+len(list.Clusters) >= list.Total {
			return n
		}
	}
}

func countBranches(ctx context.Context, cl ampCtl, proj ampCluster, name string) int {
	n := 0
	for offset := 0; ; offset += 100 {
		list, err := cl.ListBranches(ctx, proj.poolID, proj.projID, 100, offset)
		if err != nil {
			return -1
		}
		for _, b := range list.Branches {
			if b.Name == name {
				n++
			}
		}
		if list.Count < 100 || offset+len(list.Branches) >= list.Total {
			return n
		}
	}
}

func mustListEndpoints(ctx context.Context, cl ampCtl, proj ampCluster) []controller.Endpoint {
	eps, _ := cl.ListAllEndpoints(ctx, proj.poolID, proj.projID, proj.branchID)
	return eps
}
