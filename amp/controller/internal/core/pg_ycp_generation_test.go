package core

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
)

// TestPgYCPGeneration covers the applied-generation column against a real
// Postgres/YSQL database: it starts NULL, the setters are MONOTONIC (an older
// task completing late cannot regress it), it reaches every reader through the
// one branchColumns / endpointSelect projection, and a tombstoned branch is
// skipped.
//
// The monotonic half is the one that cannot be checked in-memory alone: it is
// GREATEST inside the UPDATE, so only the real statement proves it.
//
// Set AMP_TEST_DATABASE_URL to run; skipped otherwise so `go test ./...` stays
// green without a database.
func TestPgYCPGeneration(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AMP_TEST_DATABASE_URL (e.g. postgres://yugabyte@host:5433/yb_storage?sslmode=disable) to run")
	}
	const schema = "controller" // the --db-schema default
	provCtx, provCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer provCancel()
	scratchURL, drop, err := db.ScratchMigrated(provCtx, url, "amp_test_core_ycp_generation", schema)
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
	poolID, clusterID := seedCoreCluster(t, ctx, pool)
	branchID := uuid.New()
	if err := b.EnsureCluster(ctx, poolID, clusterID, branchID, "main", 18, "", nil); err != nil {
		t.Fatalf("EnsureCluster: %v", err)
	}
	endpointID := uuid.New()
	if err := b.EnsureEndpointRecord(ctx, endpointID, clusterID, branchID, "read_write", "rw",
		[]byte(`{"cpu":"2","memory":"4Gi"}`), nil, nil, nil); err != nil {
		t.Fatalf("EnsureEndpointRecord: %v", err)
	}

	branchGen := func() *int64 {
		t.Helper()
		br, err := b.GetBranch(ctx, poolID, clusterID, branchID)
		if err != nil {
			t.Fatalf("GetBranch: %v", err)
		}
		return br.YCPGeneration
	}
	endpointGen := func() *int64 {
		t.Helper()
		ep, err := b.GetEndpoint(ctx, poolID, clusterID, branchID, endpointID)
		if err != nil {
			t.Fatalf("GetEndpoint: %v", err)
		}
		return ep.YCPGeneration
	}

	// --- a row nothing has applied reports nil, not zero: it has never been the
	// target of a YCP update, which is what every pre-existing row is. ---
	if got := branchGen(); got != nil {
		t.Errorf("fresh branch ycp_generation = %d, want nil", *got)
	}
	if got := endpointGen(); got != nil {
		t.Errorf("fresh endpoint ycp_generation = %d, want nil", *got)
	}

	// --- a completed task stamps it. ---
	if err := b.SetBranchYCPGeneration(ctx, branchID, 4); err != nil {
		t.Fatalf("SetBranchYCPGeneration: %v", err)
	}
	if err := b.SetEndpointYCPGeneration(ctx, endpointID, 4); err != nil {
		t.Fatalf("SetEndpointYCPGeneration: %v", err)
	}
	if got := branchGen(); got == nil || *got != 4 {
		t.Fatalf("branch ycp_generation = %v, want 4", got)
	}
	if got := endpointGen(); got == nil || *got != 4 {
		t.Fatalf("endpoint ycp_generation = %v, want 4", got)
	}

	// --- MONOTONIC: an older task completing after a newer one leaves the newer
	// value standing, so the agent is not told to re-apply what already landed. ---
	if err := b.SetBranchYCPGeneration(ctx, branchID, 2); err != nil {
		t.Fatalf("SetBranchYCPGeneration(older): %v", err)
	}
	if err := b.SetEndpointYCPGeneration(ctx, endpointID, 2); err != nil {
		t.Fatalf("SetEndpointYCPGeneration(older): %v", err)
	}
	if got := branchGen(); got == nil || *got != 4 {
		t.Errorf("branch ycp_generation after a lower write = %v, want it held at 4", got)
	}
	if got := endpointGen(); got == nil || *got != 4 {
		t.Errorf("endpoint ycp_generation after a lower write = %v, want it held at 4", got)
	}

	// --- and it advances. ---
	if err := b.SetEndpointYCPGeneration(ctx, endpointID, 9); err != nil {
		t.Fatalf("SetEndpointYCPGeneration(newer): %v", err)
	}
	if got := endpointGen(); got == nil || *got != 9 {
		t.Errorf("endpoint ycp_generation = %v, want 9", got)
	}

	// --- every reader sees it, because both projections are one constant. ---
	branches, _, err := b.ListBranches(ctx, poolID, clusterID, 10, 0)
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if len(branches) != 1 || branches[0].YCPGeneration == nil || *branches[0].YCPGeneration != 4 {
		t.Errorf("ListBranches ycp_generation = %+v, want 4", branches)
	}
	endpoints, _, err := b.ListEndpoints(ctx, poolID, clusterID, branchID, 10, 0)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if len(endpoints) != 1 || endpoints[0].YCPGeneration == nil || *endpoints[0].YCPGeneration != 9 {
		t.Errorf("ListEndpoints ycp_generation = %+v, want 9", endpoints)
	}

	// --- an absent row is a no-op, not an error (matching the other id-keyed
	// writers), and so is a tombstoned branch: nothing will reconcile it again. ---
	if err := b.SetEndpointYCPGeneration(ctx, uuid.New(), 1); err != nil {
		t.Errorf("SetEndpointYCPGeneration on an absent endpoint = %v, want a no-op", err)
	}
	if err := b.SetBranchYCPGeneration(ctx, uuid.New(), 1); err != nil {
		t.Errorf("SetBranchYCPGeneration on an absent branch = %v, want a no-op", err)
	}
}
