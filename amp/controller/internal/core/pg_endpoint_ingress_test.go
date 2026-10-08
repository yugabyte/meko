package core

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
)

// TestPgEndpointIngressDNSName pins that an endpoint read carries the name on
// its LIVE ingress allocation and nothing else: the API reports an ingress
// endpoint's documentdb_address only for a published name, so a released row
// read back as live would advertise a gateway route the endpoint no longer has.
//
// Set AMP_TEST_DATABASE_URL to run; skipped otherwise.
func TestPgEndpointIngressDNSName(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AMP_TEST_DATABASE_URL (e.g. postgres://yugabyte@host:5433/yb_storage?sslmode=disable) to run")
	}
	const schema = "controller"
	provCtx, provCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer provCancel()
	scratchURL, drop, err := db.ScratchMigrated(provCtx, url, "amp_test_core_endpoint_ingress", schema)
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
	if err := b.EnsureCluster(ctx, poolID, clusterID, branchID, "main", 18, "", nil, nil); err != nil {
		t.Fatalf("EnsureCluster: %v", err)
	}
	endpointID := uuid.New()
	if err := b.EnsureEndpointRecord(ctx, endpointID, clusterID, branchID, "read_write", "rw",
		[]byte(`{"connectivity":"ingress_internal"}`), nil, nil, nil, nil); err != nil {
		t.Fatalf("EnsureEndpointRecord: %v", err)
	}

	names := func() (get, list string) {
		t.Helper()
		ep, err := b.GetEndpoint(ctx, poolID, clusterID, branchID, endpointID)
		if err != nil {
			t.Fatalf("GetEndpoint: %v", err)
		}
		eps, _, err := b.ListEndpoints(ctx, poolID, clusterID, branchID, 10, 0)
		if err != nil || len(eps) != 1 {
			t.Fatalf("ListEndpoints: %v (%d rows)", err, len(eps))
		}
		return ep.IngressDNSName, eps[0].IngressDNSName
	}

	if g, l := names(); g != "" || l != "" {
		t.Errorf("no allocation: IngressDNSName = %q (get) / %q (list), want empty", g, l)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO amp_registry.ingress_shards (name, family, state, hostname, port_lo, port_hi)
		 VALUES ('pgin-0', 'internal', 'ready', 'lb-0.example', 20000, 20009)`); err != nil {
		t.Fatalf("seed shard: %v", err)
	}
	// A released allocation first, as a retired-then-recreated slot would leave.
	const stale, live = "ep-00000000.old.example", "ep-11111111.int.example"
	if _, err := pool.Exec(ctx,
		`INSERT INTO amp_registry.ingress_allocations (endpoint_id, shard, port, dns_name, released_at)
		 VALUES ($1::uuid, 'pgin-0', 20000, $2, now())`, endpointID.String(), stale); err != nil {
		t.Fatalf("seed released allocation: %v", err)
	}
	if g, l := names(); g != "" || l != "" {
		t.Errorf("released allocation only: IngressDNSName = %q (get) / %q (list), want empty", g, l)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO amp_registry.ingress_allocations (endpoint_id, shard, port, dns_name)
		 VALUES ($1::uuid, 'pgin-0', 20001, $2)`, endpointID.String(), live); err != nil {
		t.Fatalf("seed live allocation: %v", err)
	}
	if g, l := names(); g != live || l != live {
		t.Errorf("live allocation: IngressDNSName = %q (get) / %q (list), want %q", g, l, live)
	}
}
