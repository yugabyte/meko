package core

import (
	"context"
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
	projectID, branchID := uuid.New(), uuid.New()
	if err := b.EnsureProject(ctx, sp.ID, projectID, branchID, "projections", 18); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	endpointID := uuid.New()
	if err := b.EnsureEndpointRecord(ctx, endpointID, projectID, branchID, RoleReadWrite, "primary",
		[]byte(`{"cpu":"2","memory":"4Gi","extended":{"pg_port":55432}}`),
		[]byte(`{"work_mem":"64MB"}`)); err != nil {
		t.Fatalf("ensure endpoint: %v", err)
	}

	t.Run("branches", func(t *testing.T) {
		// GetBranch (branchSelect, pool-joined).
		got, err := b.GetBranch(ctx, sp.ID, projectID, branchID)
		if err != nil {
			t.Fatalf("GetBranch: %v", err)
		}
		if got.BranchID != branchID || got.Name != DefaultBranchName {
			t.Errorf("GetBranch = %+v", got)
		}

		// ListBranches (its own FROM, same column list).
		list, total, err := b.ListBranches(ctx, sp.ID, projectID, 100, 0)
		if err != nil {
			t.Fatalf("ListBranches: %v", err)
		}
		if total != 1 || len(list) != 1 || list[0].BranchID != branchID {
			t.Errorf("ListBranches = %d/%+v", total, list)
		}

		// GetProject resolves the default branch through getBranchByID, the third
		// caller of scanBranch and the one with no pool join.
		p, err := b.GetProject(ctx, sp.ID, projectID)
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		if p.DefaultBranch.BranchID != branchID {
			t.Errorf("GetProject default branch = %+v", p.DefaultBranch)
		}
	})

	t.Run("endpoints", func(t *testing.T) {
		got, err := b.GetEndpoint(ctx, sp.ID, projectID, branchID, endpointID)
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
		if got.PgSettings["work_mem"] != "64MB" {
			t.Errorf("pg_settings = %v, want work_mem 64MB", got.PgSettings)
		}
		if len(got.Extended) == 0 {
			t.Error("extended_config did not survive the round trip")
		}

		list, total, err := b.ListEndpoints(ctx, sp.ID, projectID, branchID, 100, 0)
		if err != nil {
			t.Fatalf("ListEndpoints: %v", err)
		}
		if total != 1 || len(list) != 1 || list[0].ID != endpointID {
			t.Errorf("ListEndpoints = %d/%+v", total, list)
		}
	})
}
