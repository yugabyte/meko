package core

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
)

// UpdateEndpointScheduling's jsonb merge over real SQL: a nil list keeps its
// value, an empty one removes the key, and both read paths — GET's projection
// and the redeploy record — see the same thing.
func TestPgUpdateEndpointScheduling(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AMP_TEST_DATABASE_URL (e.g. postgres://yugabyte@host:5433/yb_storage?sslmode=disable) to run")
	}
	const schema = "controller"
	provCtx, provCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer provCancel()
	scratchURL, drop, err := db.ScratchMigrated(provCtx, url, "amp_test_core_scheduling", schema)
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
	sp, err := b.CreateStoragePool(ctx, owner.ID, "scheduling-pool")
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	clusterID, branchID, endpointID := uuid.New(), uuid.New(), uuid.New()
	if err := b.EnsureCluster(ctx, sp.ID, clusterID, branchID, "scheduling", 18, "", nil, nil); err != nil {
		t.Fatalf("ensure cluster: %v", err)
	}
	const tols = `[{"key": "dedicated", "operator": "Exists"}]`
	const tscs = `[{"maxSkew": 1, "topologyKey": "topology.kubernetes.io/zone"}]`
	if err := b.EnsureEndpointRecord(ctx, endpointID, clusterID, branchID, RoleReadWrite, "",
		[]byte(`{"cpu":"2","memory":"4Gi","tolerations":`+tols+`,"topology_spread_constraints":`+tscs+`}`),
		nil, nil, nil, nil); err != nil {
		t.Fatalf("ensure endpoint: %v", err)
	}

	check := func(step, wantTols, wantTscs string) {
		t.Helper()
		ep, err := b.GetEndpoint(ctx, sp.ID, clusterID, branchID, endpointID)
		if err != nil {
			t.Fatalf("%s: GetEndpoint: %v", step, err)
		}
		d, err := b.EndpointDeploy(ctx, endpointID)
		if err != nil {
			t.Fatalf("%s: EndpointDeploy: %v", step, err)
		}
		for _, got := range []Scheduling{ep.Scheduling, d.Scheduling} {
			if string(got.Tolerations) != wantTols || string(got.TopologySpreadConstraints) != wantTscs {
				t.Errorf("%s: scheduling = %s / %s, want %s / %s", step,
					got.Tolerations, got.TopologySpreadConstraints, wantTols, wantTscs)
			}
		}
		if ep.CPU != "2" {
			t.Errorf("%s: cpu = %q, want the rest of config untouched", step, ep.CPU)
		}
	}
	check("created", tols, tscs)

	const newTols = `[{"key": "gpu", "operator": "Exists"}]`
	if err := b.UpdateEndpointScheduling(ctx, endpointID, Scheduling{Tolerations: []byte(newTols)}); err != nil {
		t.Fatal(err)
	}
	check("tolerations replaced", newTols, tscs)

	if err := b.UpdateEndpointScheduling(ctx, endpointID, Scheduling{TopologySpreadConstraints: []byte(`[]`)}); err != nil {
		t.Fatal(err)
	}
	check("spread cleared", newTols, "")
}
