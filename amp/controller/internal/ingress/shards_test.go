package ingress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/yugabyte/amp/controller/internal/db"
)

// The shard state machine is enum-and-cast heavy SQL whose interesting
// properties are all about what a statement does to an EXISTING row — a second
// registration, a promotion of something already promoted — so it is checked
// against a real engine rather than modelled.
//
// Set AMP_TEST_DATABASE_URL to run.
func TestShards(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AMP_TEST_DATABASE_URL (e.g. postgres://yugabyte@host:5433/yb_storage?sslmode=disable) to run")
	}
	const schema = "controller"
	provCtx, provCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer provCancel()
	scratchURL, drop, err := db.ScratchMigrated(provCtx, url, "amp_test_ingress_shards", schema)
	if err != nil {
		t.Fatalf("provision scratch database: %v", err)
	}
	t.Cleanup(drop)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pool, err := db.Connect(ctx, scratchURL, schema)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	zones := Zones{Internal: "int.pg.example", External: "ext.pg.example"}
	t.Run("EnsureShardConverges", func(t *testing.T) { testEnsureShardConverges(ctx, t, pool, zones) })
	t.Run("EnsureShardRefusesRangeChange", func(t *testing.T) { testEnsureShardRefusesRangeChange(ctx, t, pool, zones) })
	t.Run("MarkReadyOnlyPromotesBuilding", func(t *testing.T) { testMarkReadyOnlyPromotesBuilding(ctx, t, pool, zones) })
	t.Run("ReconcileShards", func(t *testing.T) { testReconcileShards(ctx, t, pool, zones) })
}

// The property a restart depends on: registering the same shard again must not
// disturb the row. Overwriting state here would knock a ready shard back to
// building on every controller start and stall every allocation until the next
// reconcile pass.
func testEnsureShardConverges(ctx context.Context, t *testing.T, pool *db.Pool, zones Zones) {
	reset(ctx, t, pool)
	a := New(pool, zones, time.Hour, PlacementSpread)

	s := Shard{Name: FirstShardName, Family: FamilyInternal, PortLo: 20000, PortHi: 20049}
	if err := a.EnsureShard(ctx, s); err != nil {
		t.Fatalf("first EnsureShard: %v", err)
	}
	shards, err := a.ListShards(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(shards) != 1 || shards[0].State != StateBuilding {
		t.Fatalf("after register: %+v, want exactly one building shard", shards)
	}

	if err := a.MarkReady(ctx, s.Name, "lb-1.example", "CNAME"); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	// The restart.
	if err := a.EnsureShard(ctx, s); err != nil {
		t.Fatalf("second EnsureShard: %v", err)
	}
	shards, err = a.ListShards(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(shards) != 1 {
		t.Fatalf("shards = %d, want 1: a restart registered a second row", len(shards))
	}
	if shards[0].State != StateReady {
		t.Errorf("state = %q after re-registering, want ready: a restart knocked the shard back to "+
			"building and every allocation stalls until the next reconcile", shards[0].State)
	}
	if shards[0].Hostname != "lb-1.example" {
		t.Errorf("hostname = %q after re-registering, want it preserved", shards[0].Hostname)
	}
}

// The range describes the listeners the shard's installed release actually
// serves. Rewriting it from config would hand out ports nothing listens on
// (widened) or make Capacity under-report and go negative (narrowed), so a
// mismatch is refused rather than applied.
func testEnsureShardRefusesRangeChange(ctx context.Context, t *testing.T, pool *db.Pool, zones Zones) {
	reset(ctx, t, pool)
	a := New(pool, zones, time.Hour, PlacementSpread)

	if err := a.EnsureShard(ctx, Shard{
		Name: FirstShardName, Family: FamilyInternal, PortLo: 20000, PortHi: 20049,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, tc := range []struct {
		name   string
		lo, hi int
	}{
		{"widened", 20000, 20099},
		{"narrowed", 20000, 20009},
		{"shifted", 21000, 21049},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := a.EnsureShard(ctx, Shard{
				Name: FirstShardName, Family: FamilyInternal, PortLo: tc.lo, PortHi: tc.hi,
			})
			if !errors.Is(err, ErrShardPortRangeChanged) {
				t.Fatalf("EnsureShard with a %s range = %v, want ErrShardPortRangeChanged", tc.name, err)
			}
			// And the stored range must be untouched.
			shards, err := a.ListShards(ctx)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if shards[0].PortLo != 20000 || shards[0].PortHi != 20049 {
				t.Errorf("stored range = %d-%d, want the original 20000-20049",
					shards[0].PortLo, shards[0].PortHi)
			}
		})
	}
}

// MarkReady is filtered on `building` so a reconcile pass cannot overwrite the
// address of a shard that is already serving, or resurrect a draining one.
func testMarkReadyOnlyPromotesBuilding(ctx context.Context, t *testing.T, pool *db.Pool, zones Zones) {
	reset(ctx, t, pool)
	seedShard(ctx, t, pool, "pgin-ready", FamilyInternal, "ready", 20000, 20049)
	seedShard(ctx, t, pool, "pgin-drain", FamilyInternal, "draining", 21000, 21049)
	a := New(pool, zones, time.Hour, PlacementSpread)

	if err := a.MarkReady(ctx, "pgin-ready", "moved.example", "CNAME"); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	if err := a.MarkReady(ctx, "pgin-drain", "resurrected.example", "CNAME"); err != nil {
		t.Fatalf("mark draining shard: %v", err)
	}
	byName := map[string]Shard{}
	shards, err := a.ListShards(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, s := range shards {
		byName[s.Name] = s
	}
	if got := byName["pgin-ready"].Hostname; got == "moved.example" {
		t.Error("MarkReady rewrote a ready shard's address; a reconcile pass would move a live " +
			"shard's published target out from under its endpoints")
	}
	if got := byName["pgin-drain"].State; got != StateDraining {
		t.Errorf("draining shard became %q; MarkReady must not resurrect one", got)
	}
}

// fakeLookup answers per shard so one shard's outcome cannot mask another's.
type fakeLookup map[string]lookupResult

type lookupResult struct {
	host       string
	isHostname bool
	addressed  bool
	err        error
}

func (f fakeLookup) ShardAddress(_ context.Context, shard string) (string, bool, bool, error) {
	r, ok := f[shard]
	if !ok {
		return "", false, false, fmt.Errorf("no such shard %q", shard)
	}
	return r.host, r.isHostname, r.addressed, r.err
}

// A sweep must promote what it can and leave the rest, and it must not let one
// shard's failure stop another's promotion — a cloud mid-provision is the
// ordinary case, and a missing Service or denied RBAC never clears on its own.
func testReconcileShards(ctx context.Context, t *testing.T, pool *db.Pool, zones Zones) {
	reset(ctx, t, pool)
	seedShard(ctx, t, pool, "pgin-addressed", FamilyInternal, "building", 20000, 20049)
	seedShard(ctx, t, pool, "pgin-waiting", FamilyInternal, "building", 21000, 21049)
	seedShard(ctx, t, pool, "pgin-broken", FamilyInternal, "building", 22000, 22049)
	seedShard(ctx, t, pool, "pgin-ip", FamilyExternal, "building", 23000, 23049)
	a := New(pool, zones, time.Hour, PlacementSpread)

	look := fakeLookup{
		"pgin-addressed": {host: "lb.example", isHostname: true, addressed: true},
		"pgin-waiting":   {},                                  // exists, no address yet
		"pgin-broken":    {err: errors.New("403 Forbidden")},  // never clears on its own
		"pgin-ip":        {host: "10.1.2.3", addressed: true}, // an A record, not CNAME
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := a.ReconcileShards(ctx, look, quiet); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := map[string]Shard{}
	shards, err := a.ListShards(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, s := range shards {
		got[s.Name] = s
	}
	if s := got["pgin-addressed"]; s.State != StateReady || s.Hostname != "lb.example" || s.RecordType != "CNAME" {
		t.Errorf("addressed shard = %+v, want ready/lb.example/CNAME", s)
	}
	// A bare IP must publish as an A record; a CNAME to an IP does not resolve.
	if s := got["pgin-ip"]; s.State != StateReady || s.RecordType != "A" {
		t.Errorf("IP-addressed shard = %+v, want ready with record type A", s)
	}
	if s := got["pgin-waiting"]; s.State != StateBuilding {
		t.Errorf("unaddressed shard = %q, want it left building", s.State)
	}
	if s := got["pgin-broken"]; s.State != StateBuilding {
		t.Errorf("failed-lookup shard = %q, want it left building", s.State)
	}
	// The failure must not have aborted the sweep before the shards after it.
	if got["pgin-ip"].State != StateReady {
		t.Error("a failed lookup stopped a later shard from being promoted")
	}
}
