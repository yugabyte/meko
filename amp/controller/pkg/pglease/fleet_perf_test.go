package pglease

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// What one process holding N leases costs, measured rather than reasoned about.
//
// The question is not throughput. It is MARGIN: how close the worst renewal came
// to the abort deadline, because that is what predicts an unplanned exit. A lease
// is lost at TTL-SafetyMargin with no confirmed renewal, and losing one exits the
// process — so the number to watch is the smallest remaining lease across the
// fleet, not the average latency.
//
// What this DOES measure: renewal latency and margin for N leases sharing one pool
// sized as internal/db.Connect leaves it (max(4, NumCPU), no MaxConns set), with
// AMP_PGLEASE_PERF_POOL to shrink it.
//
// What it does NOT: the pool contention that matters in production. Nothing here
// generates competing task work, so the pool serves renewals only — the feedback
// loop (task work delays renewals, delayed renewals lose the lease, losing it exits
// the process) needs a second load source this test has not got. Nor does it
// reproduce a renewal spike: the acquire loop is SERIAL, so each lease's ticker
// starts at its own acquire and the renewals come out smeared across the acquire
// window rather than phase-locked. A concurrent burst would be needed for that.
//
//	AMP_TEST_DATABASE_URL=postgres://... AMP_PGLEASE_PERF_LEASES=1000 \
//	  go test ./pkg/pglease -run TestPgFleetRenewalMargin -v -timeout 20m
func TestPgFleetRenewalMargin(t *testing.T) {
	n := perfLeaseCount(t)
	// TestMain's migrated scratch database, so amp_registry.token_acquire exists.
	if leaseITURL == "" {
		t.Skipf("%s is not set; skipping live-database test", leaseITURLEnv)
	}
	url := leaseITURL

	// Sized as production is, deliberately: a generous pool would measure a
	// configuration nothing runs.
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	// Defaults to what internal/db.Connect gets from pgx; overridable, because the
	// interesting question is where the margin goes when the pool is small — a
	// 2-vCPU pod gets a pool of 4, not of this box's core count.
	poolCfg.MaxConns = int32(max(4, runtime.NumCPU()))
	if v := os.Getenv("AMP_PGLEASE_PERF_POOL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("AMP_PGLEASE_PERF_POOL=%q: want a positive integer", v)
		}
		poolCfg.MaxConns = int32(n)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), poolCfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	store, err := NewStore(pool, StoreConfig{})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rec := &recordingBackend{inner: store}

	var aborts atomic.Int64
	cfg := Config{
		TTL: DefaultTTL, RenewEvery: DefaultRenewEvery,
		RenewTimeout: DefaultRenewTimeout, SafetyMargin: DefaultSafetyMargin,
		// Record instead of exiting: an abort here would take the test binary with
		// it, and "how many leases would have died" is the result we want.
		Abort: func(string, error) { aborts.Add(1) },
	}
	client, err := New(rec, NewOwner("perf"), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	prefix := fmt.Sprintf("perf:%s:", NewOwner("k")[:12])
	held := make([]*Held, 0, n)
	defer func() {
		ctx := context.WithoutCancel(t.Context())
		for _, h := range held {
			_ = h.Release(ctx)
		}
	}()

	acquireStart := time.Now()
	for i := range n {
		h, err := client.Acquire(t.Context(), fmt.Sprintf("%s%d", prefix, i))
		if err != nil {
			t.Fatalf("acquire %d of %d: %v", i, n, err)
		}
		held = append(held, h)
	}
	acquireTook := time.Since(acquireStart)

	// Hold across several renewal intervals, sampling the fleet's worst remaining
	// lease as we go. Renewals were all acquired in a burst here, which is exactly
	// the post-restart reclaim shape — and RenewEvery has no jitter, so they stay
	// phase-locked.
	const hold = 35 * time.Second
	worst := time.Duration(1 << 62)
	ended := 0
	deadline := time.Now().Add(hold)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		for _, h := range held {
			if rem := time.Until(h.ExpiresAt()); rem < worst {
				worst = rem
			}
			// Counted, not reported per sample: at N=1000 a systemic loss would emit
			// six figures of t.Errorf and bury the result this test exists to give.
			if h.Err() != nil {
				ended++
			}
		}
	}

	lat := rec.latencies()
	st := pool.Stat()
	t.Logf("leases=%d pool_max=%d", n, poolCfg.MaxConns)
	t.Logf("  acquire all:        %s (%s/lease)", acquireTook.Round(time.Millisecond), (acquireTook / time.Duration(n)).Round(time.Microsecond))
	t.Logf("  renewals observed:  %d over %s (%.0f/s)", len(lat), hold, float64(len(lat))/hold.Seconds())
	t.Logf("  renewal latency:    p50=%s p99=%s max=%s", pct(lat, 50), pct(lat, 99), pct(lat, 100))
	t.Logf("  MARGIN (worst remaining lease): %s   [abort at %s]", worst.Round(time.Millisecond), cfg.SafetyMargin)
	t.Logf("  pool: acquires=%d empty_acquires=%d total_wait=%s",
		st.AcquireCount(), st.EmptyAcquireCount(), st.AcquireDuration().Round(time.Millisecond))
	t.Logf("  goroutines=%d  aborts=%d", runtime.NumGoroutine(), aborts.Load())

	if got := aborts.Load(); got != 0 {
		t.Errorf("%d lease(s) lost — a real process would have exited", got)
	}
	if ended > 0 {
		t.Errorf("%d lease-sample(s) observed a lease already ended", ended)
	}
	// The fence is at SafetyMargin. This threshold is not generous: a healthy worst
	// is TTL-RenewEvery, so tripping it means renewals are already late by most of a
	// cycle, with about one more cycle before the abort.
	if worst < 2*cfg.SafetyMargin {
		t.Errorf("worst remaining lease %s is inside 2x the safety margin (%s): no slack left at %d leases",
			worst, 2*cfg.SafetyMargin, n)
	}
}

func perfLeaseCount(t *testing.T) int {
	t.Helper()
	v := os.Getenv("AMP_PGLEASE_PERF_LEASES")
	if v == "" {
		t.Skip("AMP_PGLEASE_PERF_LEASES not set; this test is opt-in (it holds N leases for ~35s)")
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		t.Fatalf("AMP_PGLEASE_PERF_LEASES=%q: want a positive integer", v)
	}
	return n
}

// recordingBackend times every Acquire, which is every renewal: Backend is the
// seam that makes this measurable without instrumenting the library.
type recordingBackend struct {
	inner Backend
	mu    sync.Mutex
	lat   []time.Duration
}

func (r *recordingBackend) Acquire(ctx context.Context, key, owner string, ttl time.Duration) (Grant, error) {
	start := time.Now()
	g, err := r.inner.Acquire(ctx, key, owner, ttl)
	r.mu.Lock()
	r.lat = append(r.lat, time.Since(start))
	r.mu.Unlock()
	return g, err
}

func (r *recordingBackend) Release(ctx context.Context, key, owner string) (bool, error) {
	return r.inner.Release(ctx, key, owner)
}

func (r *recordingBackend) latencies() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]time.Duration(nil), r.lat...)
	slices.Sort(out)
	return out
}

// pct is the p-th percentile by nearest rank. Note p99 == max whenever there are
// fewer than ~100 samples, which is a property of the estimator and not of the data.
func pct(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := (len(sorted)*p + 99) / 100 // ceil, so p99 of 200 samples is the 198th
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i].Round(time.Microsecond)
}
