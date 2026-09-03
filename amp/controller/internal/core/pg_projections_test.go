package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
)

// TestPgProjectionsMatchTheirScanners drives every read path that feeds a shared
// scanner against a real database.
//
// It exists because a projection and its scanner can drift silently: adding a
// column to one branch query and not the other still compiles, still passes every
// test with a fake backend, and only fails at runtime with
// "number of field descriptions must equal number of destinations" — which the
// API reports as a 500. That is exactly what happened when `pg_settings` was
// added: `branchSelect` got it, `ListBranches`'s own inline copy did not, and the
// launcher's first `GET .../branches` blew up in CI.
//
// The queries now share one column list per entity, and this test is what keeps
// them honest: every scanner is reached at least once over real SQL.
func TestPgProjectionsMatchTheirScanners(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AMP_TEST_DATABASE_URL (e.g. postgres://yugabyte@host:5433/yb_storage?sslmode=disable) to run")
	}
	const schema = "controller"
	provCtx, provCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer provCancel()
	scratchURL, drop, err := db.ScratchMigrated(provCtx, url, "amp_test_core_projections", schema)
	if err != nil {
		t.Fatalf("provision scratch database: %v", err)
	}
	t.Cleanup(drop)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, scratchURL, schema)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	b := NewPgBackend(pool)

	owner, err := b.CreateUser(ctx, false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	sp, err := b.CreateStoragePool(ctx, owner.ID, "projections-pool")
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	clusterID, branchID := uuid.New(), uuid.New()
	if err := b.EnsureCluster(ctx, sp.ID, clusterID, branchID, "projections", 18, "", nil); err != nil {
		t.Fatalf("ensure cluster: %v", err)
	}
	endpointID := uuid.New()
	if err := b.EnsureEndpointRecord(ctx, endpointID, clusterID, branchID, RoleReadWrite, "primary",
		[]byte(`{"cpu":"2","memory":"4Gi","extended":{"pg_port":55432}}`),
		[]byte(`{"work_mem":"64MB"}`),
		// Disjoint keys: the two daemon columns are adjacent in the projection, so
		// identical fixtures would let a transposed scan pass.
		[]byte(`{"tenant_config.enable_gc":true,"scale_factor":8}`),
		[]byte(`{"wal_cache_size_mib":512}`)); err != nil {
		t.Fatalf("ensure endpoint: %v", err)
	}
	// version is its own column, written after the insert the way the
	// create_endpoint step writes it (the deployer reports it only once the deploy
	// has landed).
	if err := b.SetEndpointVersion(ctx, endpointID, "ep-v7.7.7"); err != nil {
		t.Fatalf("set endpoint version: %v", err)
	}
	if err := b.SetEndpointTargetVersion(ctx, endpointID, "ep-v8.8.8"); err != nil {
		t.Fatalf("SetEndpointTargetVersion: %v", err)
	}

	// pg_settings and write_token_settings are adjacent jsonb columns in the
	// branch projection, so — as with the endpoint's two daemon columns above —
	// disjoint fixtures are what stops a transposed scan from passing.
	branchPg := map[string]string{"statement_timeout": "30s"}
	branchLease := map[string]any{"ttl_ms": 20000}
	if err := b.UpdateBranchSettings(ctx, branchID, &branchPg, &branchLease); err != nil {
		t.Fatalf("seed branch settings: %v", err)
	}

	t.Run("branches", func(t *testing.T) {
		// GetBranch (branchSelect, pool-joined).
		got, err := b.GetBranch(ctx, sp.ID, clusterID, branchID)
		if err != nil {
			t.Fatalf("GetBranch: %v", err)
		}
		if got.BranchID != branchID || got.Name != DefaultBranchName {
			t.Errorf("GetBranch = %+v", got)
		}
		if got.PgSettings["statement_timeout"] != "30s" {
			t.Errorf("branch pg_settings did not land: %+v", got.PgSettings)
		}
		// A transposed scan would put the lease object here, or the settings in
		// the lease — either way this comparison fails rather than the row
		// merely looking populated.
		if fmt.Sprint(got.WriteTokenSettings["ttl_ms"]) != "20000" {
			t.Errorf("branch write_token_settings did not land: %+v", got.WriteTokenSettings)
		}
		if _, crossed := got.WriteTokenSettings["statement_timeout"]; crossed {
			t.Errorf("pg_settings leaked into write_token_settings: %+v", got.WriteTokenSettings)
		}

		// ListBranches (its own FROM, same column list).
		list, total, err := b.ListBranches(ctx, sp.ID, clusterID, 100, 0)
		if err != nil {
			t.Fatalf("ListBranches: %v", err)
		}
		if total != 1 || len(list) != 1 || list[0].BranchID != branchID {
			t.Errorf("ListBranches = %d/%+v", total, list)
		}

		// GetCluster resolves the default branch through getBranchByID, the third
		// caller of scanBranch and the one with no pool join.
		p, err := b.GetCluster(ctx, sp.ID, clusterID)
		if err != nil {
			t.Fatalf("GetCluster: %v", err)
		}
		if p.DefaultBranch.BranchID != branchID {
			t.Errorf("GetCluster default branch = %+v", p.DefaultBranch)
		}
	})

	t.Run("endpoints", func(t *testing.T) {
		got, err := b.GetEndpoint(ctx, sp.ID, clusterID, branchID, endpointID)
		if err != nil {
			t.Fatalf("GetEndpoint: %v", err)
		}
		// Every column the projection added has to survive the round trip; a
		// scanner that reads them in the wrong order still returns a row.
		if got.Name != "primary" {
			t.Errorf("name = %q, want primary", got.Name)
		}
		if got.CPU != "2" || got.Memory != "4Gi" {
			t.Errorf("resources = (%q,%q), want (2,4Gi)", got.CPU, got.Memory)
		}
		// Distinct from every other text column in the projection: version sits
		// next to name/address/cpu/memory, so a transposed scan has to be
		// distinguishable from a merely populated row.
		if got.Version != "ep-v7.7.7" || got.TargetVersion != "ep-v8.8.8" {
			t.Errorf("version/target = %q/%q, want ep-v7.7.7/ep-v8.8.8", got.Version, got.TargetVersion)
		}
		if got.PgSettings["work_mem"] != "64MB" {
			t.Errorf("pg_settings = %v, want work_mem 64MB", got.PgSettings)
		}
		if len(got.Extended) == 0 {
			t.Error("extended_config did not survive the round trip")
		}
		// Each must land in its own field, and the dotted key must survive as a
		// literal key rather than being nested by the json round trip.
		if got.PageserverSettings["tenant_config.enable_gc"] != true {
			t.Errorf("pageserver_settings = %v, want tenant_config.enable_gc true", got.PageserverSettings)
		}
		if _, wrong := got.PageserverSettings["wal_cache_size_mib"]; wrong {
			t.Errorf("safekeeper settings leaked into pageserver_settings: %v", got.PageserverSettings)
		}
		if got.SafekeeperSettings["wal_cache_size_mib"] != float64(512) {
			t.Errorf("safekeeper_settings = %v, want wal_cache_size_mib 512", got.SafekeeperSettings)
		}
		if _, wrong := got.SafekeeperSettings["scale_factor"]; wrong {
			t.Errorf("pageserver settings leaked into safekeeper_settings: %v", got.SafekeeperSettings)
		}

		list, total, err := b.ListEndpoints(ctx, sp.ID, clusterID, branchID, 100, 0)
		if err != nil {
			t.Fatalf("ListEndpoints: %v", err)
		}
		if total != 1 || len(list) != 1 || list[0].ID != endpointID {
			t.Errorf("ListEndpoints = %d/%+v", total, list)
		}

		// BranchEndpointDeploys has its own projection and its own positional
		// scanner — the desired state every redeploy renders from. Its fixtures are
		// mutually distinguishable for the same reason as above.
		deploys, err := b.BranchEndpointDeploys(ctx, clusterID, branchID)
		if err != nil {
			t.Fatalf("BranchEndpointDeploys: %v", err)
		}
		if len(deploys) != 1 || deploys[0].ID != endpointID {
			t.Fatalf("BranchEndpointDeploys = %+v", deploys)
		}
		d := deploys[0]
		if d.PgVersion != 18 || d.CPU != "2" || d.Memory != "4Gi" || d.Version != "ep-v7.7.7" {
			t.Errorf("deploy state = pg%d %s/%s %q, want pg18 2/4Gi ep-v7.7.7", d.PgVersion, d.CPU, d.Memory, d.Version)
		}
		if d.BranchID != branchID {
			t.Errorf("deploy state branch = %s, want %s", d.BranchID, branchID)
		}

		// The by-id form shares that projection and scanner, and is what
		// upgrade_endpoint renders one endpoint from.
		one, err := b.EndpointDeploy(ctx, endpointID)
		if err != nil {
			t.Fatalf("EndpointDeploy: %v", err)
		}
		if one != d {
			t.Errorf("EndpointDeploy = %+v, want the same state the branch enumeration reports (%+v)", one, d)
		}
		if _, err := b.EndpointDeploy(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Errorf("EndpointDeploy on an unknown id = %v, want ErrNotFound", err)
		}
	})
}
