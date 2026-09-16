package ingress

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
)

// The allocator is the one place where a bug hands two customers the same
// address, and every property worth having is a property of a SQL statement
// running against a real engine: the partial unique indexes that arbitrate a
// race, the quarantine that keeps a released port out of the pool, the ORDER BY
// that decides placement. None of that survives being faked, so these run
// against YugabyteDB and skip without one.
//
// Set AMP_TEST_DATABASE_URL to run.
func TestAllocator(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AMP_TEST_DATABASE_URL (e.g. postgres://yugabyte@host:5433/yb_storage?sslmode=disable) to run")
	}
	// Provisioning gets a budget of its own: ScratchMigrated queues behind a lock
	// that serializes DDL across test packages, which says nothing about this
	// test's own latency.
	const schema = "controller" // the --db-schema default
	provCtx, provCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer provCancel()
	scratchURL, drop, err := db.ScratchMigrated(provCtx, url, "amp_test_ingress_allocator", schema)
	if err != nil {
		t.Fatalf("provision scratch database: %v", err)
	}
	t.Cleanup(drop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := db.Connect(ctx, scratchURL, schema)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// Deliberately different per family: a name minted in the wrong family's zone
	// is the failure Zones exists to prevent, and one shared value would hide it.
	zones := Zones{Internal: "int.pg.example", External: "ext.pg.example"}
	// Subtests share one scratch database and reset between them, so they must not
	// run in parallel.
	t.Run("RoundTrip", func(t *testing.T) { testRoundTrip(ctx, t, pool, zones) })
	t.Run("AllocateIsIdempotent", func(t *testing.T) { testAllocateIsIdempotent(ctx, t, pool, zones) })
	t.Run("Quarantine", func(t *testing.T) { testQuarantine(ctx, t, pool, zones) })
	t.Run("Capacity", func(t *testing.T) { testCapacity(ctx, t, pool, zones) })
	t.Run("Placement", func(t *testing.T) { testPlacement(ctx, t, pool, zones) })
	t.Run("NoCapacity", func(t *testing.T) { testNoCapacity(ctx, t, pool, zones) })
	t.Run("ZonePerFamily", func(t *testing.T) { testZonePerFamily(ctx, t, pool, zones) })
	t.Run("NameCollisionWidens", func(t *testing.T) { testNameCollisionWidens(ctx, t, pool, zones) })
	t.Run("ConcurrentAllocation", func(t *testing.T) { testConcurrentAllocation(ctx, t, pool, zones) })
	t.Run("EnabledWithoutZone", func(t *testing.T) { testEnabledWithoutZone(ctx, t, pool) })
}

func testRoundTrip(ctx context.Context, t *testing.T, pool *db.Pool, zones Zones) {
	reset(ctx, t, pool)
	seedShard(ctx, t, pool, "pgin-0", FamilyInternal, "ready", 20000, 20009)
	a := New(pool, true, zones, time.Hour, PlacementSpread)

	ep := uuid.New()
	alloc, err := a.Allocate(ctx, ep, FamilyInternal)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if alloc.Shard != "pgin-0" || alloc.Port != 20000 {
		t.Errorf("allocated %s:%d, want pgin-0:20000 — the lowest free port on the only shard",
			alloc.Shard, alloc.Port)
	}
	if want := shortName(ep, zones.Internal); alloc.DNSName != want {
		t.Errorf("dns name = %q, want %q", alloc.DNSName, want)
	}
	if !alloc.Publish {
		t.Error("Publish = false with a zone configured; clients would be handed the shard's address")
	}
	if want := fmt.Sprintf("%s:%d", alloc.DNSName, alloc.Port); alloc.Address() != want {
		t.Errorf("Address() = %q, want %q", alloc.Address(), want)
	}
	got, err := a.Get(ctx, ep)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != alloc.ID || got.Port != alloc.Port || got.DNSName != alloc.DNSName {
		t.Errorf("Get returned %+v, want the row Allocate issued (%+v)", got, alloc)
	}
	// ShardHost and Publish are carried on the allocation so the caller renders
	// DNS from one read; both have to survive Get, not just INSERT ... RETURNING.
	if got.ShardHost != "lb-pgin-0.example" {
		t.Errorf("Get ShardHost = %q, want the shard's hostname", got.ShardHost)
	}
	if got.Publish != alloc.Publish {
		t.Errorf("Get Publish = %v, want %v", got.Publish, alloc.Publish)
	}

	if err := a.Release(ctx, ep); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := a.Get(ctx, ep); !errors.Is(err, ErrNotAllocated) {
		t.Errorf("Get after Release = %v, want ErrNotAllocated", err)
	}
	// Idempotent: retiring an endpoint twice is a task step replaying, not a fault.
	if err := a.Release(ctx, ep); err != nil {
		t.Errorf("second Release = %v, want nil — the step must be replayable", err)
	}
}

// Allocate has to survive being run twice, because a task step is the unit of
// re-run — on a 40001 abort and on resume after a crash. A second call must
// therefore hand back the SAME slot: a new one would issue two addresses for one
// database and hold the first until its quarantine expired, and an error would
// make any step containing this fail permanently the second time it ran.
func testAllocateIsIdempotent(ctx context.Context, t *testing.T, pool *db.Pool, zones Zones) {
	reset(ctx, t, pool)
	seedShard(ctx, t, pool, "pgin-0", FamilyInternal, "ready", 20000, 20009)
	a := New(pool, true, zones, time.Hour, PlacementSpread)

	ep := uuid.New()
	first, err := a.Allocate(ctx, ep, FamilyInternal)
	if err != nil {
		t.Fatalf("first allocate: %v", err)
	}
	again, err := a.Allocate(ctx, ep, FamilyInternal)
	if err != nil {
		t.Fatalf("second allocate = %v, want the first allocation back", err)
	}
	if again.ID != first.ID || again.Port != first.Port || again.DNSName != first.DNSName {
		t.Errorf("replay returned %+v, want the slot already issued (%+v): the address an "+
			"endpoint answers on must not change under a re-run", again, first)
	}
	// And the replay must not have consumed a second slot.
	var live int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM amp_registry.ingress_allocations WHERE released_at IS NULL`).Scan(&live); err != nil {
		t.Fatalf("count live allocations: %v", err)
	}
	if live != 1 {
		t.Errorf("live allocations = %d, want 1: the replay issued a second address", live)
	}
}

// A name must be minted in its OWN family's zone. dns_name is durable, so an
// internal endpoint published into the public zone is not something later wiring
// can correct without reissuing an address the customer already holds.
func testZonePerFamily(ctx context.Context, t *testing.T, pool *db.Pool, zones Zones) {
	reset(ctx, t, pool)
	seedShard(ctx, t, pool, "pgin-int", FamilyInternal, "ready", 20000, 20009)
	seedShard(ctx, t, pool, "pgin-ext", FamilyExternal, "ready", 21000, 21009)
	a := New(pool, true, zones, time.Hour, PlacementSpread)

	in, err := a.Allocate(ctx, uuid.New(), FamilyInternal)
	if err != nil {
		t.Fatalf("allocate internal: %v", err)
	}
	if !strings.HasSuffix(in.DNSName, "."+zones.Internal) {
		t.Errorf("internal name %q is not in the internal zone %q", in.DNSName, zones.Internal)
	}
	ext, err := a.Allocate(ctx, uuid.New(), FamilyExternal)
	if err != nil {
		t.Fatalf("allocate external: %v", err)
	}
	if !strings.HasSuffix(ext.DNSName, "."+zones.External) {
		t.Errorf("external name %q is not in the external zone %q", ext.DNSName, zones.External)
	}

	// A family with no zone still allocates — the name is minted unqualified and
	// simply not published, which is what a cluster with no external-dns gets.
	// The other family's zone must not leak into it.
	half := New(pool, true, Zones{Internal: zones.Internal}, time.Hour, PlacementSpread)
	if half.PublishesDNS(FamilyExternal) {
		t.Error("PublishesDNS(external) = true with no external zone")
	}
	unpublished, err := half.Allocate(ctx, uuid.New(), FamilyExternal)
	if err != nil {
		t.Fatalf("allocate external with no external zone = %v, want an unpublished name", err)
	}
	if strings.Contains(unpublished.DNSName, ".") {
		t.Errorf("external name %q is qualified; with no external zone it must be bare, and it "+
			"must never borrow the internal zone", unpublished.DNSName)
	}
	if unpublished.Publish {
		t.Error("Publish = true for a family with no zone")
	}
}

// The quarantine is the allocator's job, not the schema's — the moment
// released_at is set the row leaves every unique index and the database will
// happily reissue the port. This is the only thing standing between a client
// with a cached address and whichever database takes the slot next.
func testQuarantine(ctx context.Context, t *testing.T, pool *db.Pool, zones Zones) {
	reset(ctx, t, pool)
	// Exactly one port, so a reissue is the only way a second allocate succeeds.
	seedShard(ctx, t, pool, "pgin-0", FamilyInternal, "ready", 20000, 20000)

	held := New(pool, true, zones, time.Hour, PlacementSpread)
	first := uuid.New()
	if _, err := held.Allocate(ctx, first, FamilyInternal); err != nil {
		t.Fatalf("first allocate: %v", err)
	}
	if err := held.Release(ctx, first); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := held.Allocate(ctx, uuid.New(), FamilyInternal); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("allocate into a quarantined slot = %v, want ErrNoCapacity: a client holding the "+
			"old address would reach a different customer's database", err)
	}

	// Same row, same state — only the window differs, which is what proves the
	// quarantine is what refused above rather than some other emptiness.
	expired := New(pool, true, zones, 0, PlacementSpread)
	alloc, err := expired.Allocate(ctx, uuid.New(), FamilyInternal)
	if err != nil {
		t.Fatalf("allocate after the window = %v, want success: the port must come back", err)
	}
	if alloc.Port != 20000 {
		t.Errorf("reissued port = %d, want 20000", alloc.Port)
	}
}

// Capacity counts a quarantined slot as taken, because it is: the headroom
// watermark is measured against this, and building a shard early is cheaper than
// finding at allocation time that the free slots were all cooling off.
func testCapacity(ctx context.Context, t *testing.T, pool *db.Pool, zones Zones) {
	reset(ctx, t, pool)
	seedShard(ctx, t, pool, "pgin-0", FamilyInternal, "ready", 20000, 20003)
	a := New(pool, true, zones, time.Hour, PlacementSpread)

	assertCapacity := func(when string, wantFree, wantTotal int) {
		t.Helper()
		free, total, err := a.Capacity(ctx, FamilyInternal)
		if err != nil {
			t.Fatalf("capacity %s: %v", when, err)
		}
		if free != wantFree || total != wantTotal {
			t.Errorf("capacity %s = (free %d, total %d), want (%d, %d)", when, free, total, wantFree, wantTotal)
		}
	}
	assertCapacity("empty", 4, 4)

	ep := uuid.New()
	if _, err := a.Allocate(ctx, ep, FamilyInternal); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	assertCapacity("with one live", 3, 4)

	if err := a.Release(ctx, ep); err != nil {
		t.Fatalf("release: %v", err)
	}
	assertCapacity("with one quarantined", 3, 4)

	free, _, err := New(pool, true, zones, 0, PlacementSpread).Capacity(ctx, FamilyInternal)
	if err != nil {
		t.Fatalf("capacity after the window: %v", err)
	}
	if free != 4 {
		t.Errorf("free after the quarantine expires = %d, want 4", free)
	}
}

// Spread and pack are a real trade, not a tuning knob: spread keeps the blast
// radius small but means no shard ever empties on its own, so this pins that the
// ORDER BY actually differs rather than both rendering the same plan.
func testPlacement(ctx context.Context, t *testing.T, pool *db.Pool, zones Zones) {
	reset(ctx, t, pool)
	seedShard(ctx, t, pool, "pgin-a", FamilyInternal, "ready", 20000, 20009)
	seedShard(ctx, t, pool, "pgin-b", FamilyInternal, "ready", 21000, 21009)
	// pgin-a is the fuller of the two.
	seedAllocation(ctx, t, pool, "pgin-a", 20000)
	seedAllocation(ctx, t, pool, "pgin-a", 20001)

	spread, err := New(pool, true, zones, time.Hour, PlacementSpread).Allocate(ctx, uuid.New(), FamilyInternal)
	if err != nil {
		t.Fatalf("spread allocate: %v", err)
	}
	if spread.Shard != "pgin-b" {
		t.Errorf("spread landed on %s, want pgin-b — the emptiest shard", spread.Shard)
	}

	pack, err := New(pool, true, zones, time.Hour, PlacementPack).Allocate(ctx, uuid.New(), FamilyInternal)
	if err != nil {
		t.Fatalf("pack allocate: %v", err)
	}
	if pack.Shard != "pgin-a" {
		t.Errorf("pack landed on %s, want pgin-a — the fullest shard with room", pack.Shard)
	}

	// An unset placement must not render an empty ORDER BY.
	if _, err := New(pool, true, zones, time.Hour, "").Allocate(ctx, uuid.New(), FamilyInternal); err != nil {
		t.Errorf("allocate with placement unset = %v, want the spread default", err)
	}
}

// Running out is a refusal, never a quiet fallback to a dedicated load balancer:
// that is how a deployment ends up back at the per-endpoint bill this design
// exists to remove, and it would do so invisibly.
func testNoCapacity(ctx context.Context, t *testing.T, pool *db.Pool, zones Zones) {
	reset(ctx, t, pool)
	seedShard(ctx, t, pool, "pgin-0", FamilyInternal, "ready", 20000, 20000)
	seedShard(ctx, t, pool, "pgin-drain", FamilyInternal, "draining", 20100, 20199)
	seedShard(ctx, t, pool, "pgin-ext", FamilyExternal, "ready", 20200, 20299)
	a := New(pool, true, zones, time.Hour, PlacementSpread)

	if _, err := a.Allocate(ctx, uuid.New(), FamilyInternal); err != nil {
		t.Fatalf("allocate the only internal slot: %v", err)
	}
	// A draining shard has free ports and must still be passed over.
	if _, err := a.Allocate(ctx, uuid.New(), FamilyInternal); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("allocate with only a draining shard left = %v, want ErrNoCapacity", err)
	}
	// The external shard is ready and empty, and must not rescue an internal
	// request: family is a property of the load balancer, so landing on the wrong
	// one exposes a private database to the internet.
	ext, err := a.Allocate(ctx, uuid.New(), FamilyExternal)
	if err != nil {
		t.Fatalf("allocate external: %v", err)
	}
	if ext.Shard != "pgin-ext" {
		t.Errorf("external allocation landed on %s, want pgin-ext", ext.Shard)
	}

	// A deployment with no zone has no ingress at all, and says so up front rather
	// than inventing a name nothing resolves — so the API can answer 400 instead of
	// deploying an endpoint that cannot be reached.
	off := New(pool, false, zones, time.Hour, PlacementSpread)
	if off.Enabled() {
		t.Error("Enabled() = true with ingress disabled")
	}
	// A misconfiguration, not exhaustion: the two want opposite handling, so they
	// must not share a sentinel.
	if _, err := off.Allocate(ctx, uuid.New(), FamilyInternal); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("allocate with ingress disabled = %v, want ErrNotConfigured", err)
	}
}

// Two endpoints whose ids share their first 32 bits collide on the short name,
// and the second must widen to the full id rather than fail.
func testNameCollisionWidens(ctx context.Context, t *testing.T, pool *db.Pool, zones Zones) {
	reset(ctx, t, pool)
	seedShard(ctx, t, pool, "pgin-0", FamilyInternal, "ready", 20000, 20009)
	a := New(pool, true, zones, time.Hour, PlacementSpread)

	first := uuid.New()
	twin := twinOf(t, first)
	if shortName(first, zones.Internal) != shortName(twin, zones.Internal) {
		t.Fatalf("test bug: %s and %s do not share a short name", first, twin)
	}

	if _, err := a.Allocate(ctx, first, FamilyInternal); err != nil {
		t.Fatalf("allocate first: %v", err)
	}
	alloc, err := a.Allocate(ctx, twin, FamilyInternal)
	if err != nil {
		t.Fatalf("allocate the colliding twin = %v, want success on the widened name", err)
	}
	if want := longName(twin, zones.Internal); alloc.DNSName != want {
		t.Errorf("twin dns name = %q, want the full-id form %q", alloc.DNSName, want)
	}
}

// The property this pins is not just that concurrent creates get distinct ports
// — the partial unique index guarantees that — but that losing the race does NOT
// cost the loser its short name.
//
// The ORDER BY makes every concurrent Allocate in a family choose the SAME
// lowest free port, so overlapping creates collide on the slot by construction.
// Widening the name on any unique violation (rather than on a probe that says
// the name is taken) therefore hands an ordinary race's loser the 32-hex name
// for the life of the endpoint, with nothing having collided on its name.
func testConcurrentAllocation(ctx context.Context, t *testing.T, pool *db.Pool, zones Zones) {
	reset(ctx, t, pool)
	const n = 8
	seedShard(ctx, t, pool, "pgin-0", FamilyInternal, "ready", 20000, 20000+n-1)
	a := New(pool, true, zones, time.Hour, PlacementSpread)

	type result struct {
		endpoint uuid.UUID
		alloc    Allocation
		err      error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ep := uuid.New()
			<-start
			alloc, err := a.Allocate(ctx, ep, FamilyInternal)
			results[i] = result{endpoint: ep, alloc: alloc, err: err}
		}()
	}
	close(start)
	wg.Wait()

	ports := map[int]uuid.UUID{}
	succeeded := 0
	for _, r := range results {
		if r.err != nil {
			// A loser that exhausts its retries is a legitimate outcome under
			// contention — the create_endpoint step retries. A wrong ADDRESS is not,
			// which is what the rest of this asserts.
			t.Logf("allocate for %s: %v", r.endpoint, r.err)
			continue
		}
		succeeded++
		if other, dup := ports[r.alloc.Port]; dup {
			t.Fatalf("port %d issued to both %s and %s: two databases on one address",
				r.alloc.Port, other, r.endpoint)
		}
		ports[r.alloc.Port] = r.endpoint
		if want := shortName(r.endpoint, zones.Internal); r.alloc.DNSName != want {
			t.Errorf("%s got dns name %q, want the short form %q: losing a SLOT race must not "+
				"widen the name, which nothing collided on", r.endpoint, r.alloc.DNSName, want)
		}
	}
	if succeeded == 0 {
		t.Fatal("every concurrent allocation failed; the retry does not converge")
	}
}

// Enabled and zones are different questions, and a kind or CI cluster answers
// yes and no: it routes through shards but has no external-dns. The name is
// still allocated and still unique — the column and its index do not change
// shape just because there is no DNS — but clients are handed the shard's own
// address rather than a name nothing resolves.
func testEnabledWithoutZone(ctx context.Context, t *testing.T, pool *db.Pool) {
	reset(ctx, t, pool)
	seedShard(ctx, t, pool, "pgin-0", FamilyInternal, "ready", 20000, 20009)
	a := New(pool, true, Zones{}, time.Hour, PlacementSpread)

	if !a.Enabled() {
		t.Error("Enabled() = false; a deployment with no zone still routes through shards")
	}
	if a.PublishesDNS(FamilyInternal) {
		t.Error("PublishesDNS(internal) = true with no zone")
	}
	ep := uuid.New()
	alloc, err := a.Allocate(ctx, ep, FamilyInternal)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if want := fmt.Sprintf("ep-%s", hexOf(ep)[:8]); alloc.DNSName != want {
		t.Errorf("dns name = %q, want the unqualified %q", alloc.DNSName, want)
	}
	if alloc.Publish {
		t.Error("Publish = true with no zone; clients would be given a name nothing resolves")
	}
	if want := fmt.Sprintf("lb-pgin-0.example:%d", alloc.Port); alloc.Address() != want {
		t.Errorf("Address() = %q, want the shard's own address %q", alloc.Address(), want)
	}
}

// --- helpers ---

func reset(ctx context.Context, t *testing.T, pool *db.Pool) {
	t.Helper()
	// Allocations first: shard carries a foreign key, deliberately.
	for _, stmt := range []string{
		`DELETE FROM amp_registry.ingress_allocations`,
		`DELETE FROM amp_registry.ingress_shards`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("reset (%s): %v", stmt, err)
		}
	}
}

func seedShard(ctx context.Context, t *testing.T, pool *db.Pool, name string, family Family, state string, lo, hi int) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO amp_registry.ingress_shards (name, family, state, hostname, port_lo, port_hi)
		 VALUES ($1, $2::amp_registry.ingress_family, $3::amp_registry.ingress_shard_state, $4, $5, $6)`,
		name, string(family), state, "lb-"+name+".example", lo, hi); err != nil {
		t.Fatalf("seed shard %s: %v", name, err)
	}
}

// seedAllocation occupies a slot without going through the allocator, so a test
// can set up a shard's fill level independently of the thing under test.
func seedAllocation(ctx context.Context, t *testing.T, pool *db.Pool, shard string, port int) {
	t.Helper()
	ep := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO amp_registry.ingress_allocations (endpoint_id, shard, port, dns_name)
		 VALUES ($1::uuid, $2, $3, $4)`,
		ep.String(), shard, port, fmt.Sprintf("seed-%s-%d.example", shard, port)); err != nil {
		t.Fatalf("seed allocation %s:%d: %v", shard, port, err)
	}
}

func hexOf(id uuid.UUID) string { return strings.ReplaceAll(id.String(), "-", "") }

func shortName(id uuid.UUID, zone string) string {
	return fmt.Sprintf("ep-%s.%s", hexOf(id)[:8], zone)
}

func longName(id uuid.UUID, zone string) string {
	return fmt.Sprintf("ep-%s.%s", hexOf(id), zone)
}

// twinOf builds a different uuid sharing the first 8 hex digits, which is the
// collision dnsNameFor widens for.
func twinOf(t *testing.T, id uuid.UUID) uuid.UUID {
	t.Helper()
	h := hexOf(id)[:8] + hexOf(uuid.New())[8:]
	twin, err := uuid.Parse(fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]))
	if err != nil {
		t.Fatalf("build twin uuid: %v", err)
	}
	if twin == id {
		t.Fatal("twin equals the original")
	}
	return twin
}
