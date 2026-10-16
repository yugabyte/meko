package sched

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/fake"
)

// memSlotStore is a slotStore with the ConfigMap's version check.
type memSlotStore struct {
	mu        sync.Mutex
	entries   map[string]slotEntry
	version   int
	conflicts int // the next n stores report a conflict
	loadErr   error
}

func (m *memSlotStore) load(context.Context) (map[string]slotEntry, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return nil, "", m.loadErr
	}
	out := map[string]slotEntry{}
	for k, v := range m.entries {
		out[k] = v
	}
	return out, strconv.Itoa(m.version), nil
}

func (m *memSlotStore) store(_ context.Context, e map[string]slotEntry, v string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conflicts > 0 {
		m.conflicts--
		return true, nil
	}
	if v != strconv.Itoa(m.version) {
		return true, nil
	}
	m.entries, m.version = e, m.version+1
	return false, nil
}

func (m *memSlotStore) held() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return heldUnits(m.entries)
}

func TestAdmitIsStrictFIFO(t *testing.T) {
	now := time.Unix(1000, 0)
	live := now.Add(time.Minute).Unix()
	m := map[string]slotEntry{
		"held":  {Units: 6, Held: true, Since: 1, Expires: live},
		"big":   {Units: 6, Since: 2, Expires: live},
		"small": {Units: 2, Since: 3, Expires: live},
	}
	if _, ok, _ := admit(m, "small", 12, now); ok {
		t.Error("a small waiter must not jump the older six-slot one")
	}
	if _, ok, _ := admit(m, "big", 12, now); !ok {
		t.Error("the oldest waiter that fits is admitted")
	}
}

func TestAdmitDropsExpiredAndClamps(t *testing.T) {
	now := time.Unix(1000, 0)
	m := map[string]slotEntry{
		"dead": {Units: 12, Held: true, Since: 1, Expires: now.Add(-time.Second).Unix()},
		"me":   {Units: 30, Since: 2, Expires: now.Add(time.Minute).Unix()},
	}
	out, ok, _ := admit(m, "me", 12, now)
	if !ok {
		t.Error("a run that died holding slots frees them once its entry expires, and an oversized request runs alone")
	}
	if _, found := out["dead"]; found {
		t.Error("the expired entry must be dropped on the next write")
	}
}

func TestClusterSlotsBlocksUntilRelease(t *testing.T) {
	st := &memSlotStore{}
	c := newClusterSlots(st, 6, nil)
	c.poll = 5 * time.Millisecond
	relA, err := c.Acquire(context.Background(), "perf/pgbench", 6)
	if err != nil || st.held() != 6 {
		t.Fatalf("first acquire: %v, held %d", err, st.held())
	}
	got := make(chan func(), 1)
	go func() {
		rel, err := c.Acquire(context.Background(), "perf/tpcc", 6)
		if err == nil {
			got <- rel
		}
	}()
	select {
	case <-got:
		t.Fatal("the second benchmark must wait while the first holds the cluster")
	case <-time.After(50 * time.Millisecond):
	}
	relA()
	relA()
	select {
	case relB := <-got:
		if st.held() != 6 {
			t.Errorf("held = %d after hand-over, want 6", st.held())
		}
		relB()
		if st.held() != 0 {
			t.Errorf("held = %d after both released", st.held())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter never got the released slots")
	}
}

func TestClusterSlotsRetriesConflicts(t *testing.T) {
	st := &memSlotStore{conflicts: 3}
	c := newClusterSlots(st, 12, nil)
	rel, err := c.Acquire(context.Background(), "perf/sysbench", 6)
	if err != nil || st.held() != 6 {
		t.Fatalf("acquire through conflicts: %v, held %d", err, st.held())
	}
	rel()
}

func TestClusterSlotsCancelLeavesNoWaiter(t *testing.T) {
	st := &memSlotStore{}
	c := newClusterSlots(st, 6, nil)
	c.poll = 5 * time.Millisecond
	rel, _ := c.Acquire(context.Background(), "a", 6)
	defer rel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Acquire(ctx, "b", 6); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the context's error, got %v", err)
	}
	st.mu.Lock()
	n := len(st.entries)
	st.mu.Unlock()
	if n != 1 {
		t.Errorf("a cancelled waiter must remove its entry, %d entries left", n)
	}
}

func TestSlotKey(t *testing.T) {
	if got := slotKey("perf/pgbench_uncached"); got != "perf.pgbench_uncached" {
		t.Errorf("slotKey = %q", got)
	}
}

func TestClusterSlotsFromConfigNeedsANamedCluster(t *testing.T) {
	if ClusterSlotsFromConfig(core.NewConfig(), nil) != nil {
		t.Error("the ambient kube context must never be assumed")
	}
	c := core.NewConfig().Derive(map[string]string{"env.k8s.context": "itest-dev"})
	if ClusterSlotsFromConfig(c, nil) == nil {
		t.Error("a run naming a cluster takes part")
	}
	off := c.Derive(map[string]string{"sched.cluster_slots.capacity": "int:0"})
	if ClusterSlotsFromConfig(off, nil) != nil {
		t.Error("capacity 0 turns the cap off")
	}
}

func TestSchedulerHoldsClusterSlotsAroundTheBody(t *testing.T) {
	st := &memSlotStore{}
	slots := newClusterSlots(st, 6, nil)
	slots.poll = 5 * time.Millisecond
	var mu sync.Mutex
	live, peak := 0, 0
	body := func(*core.T) error {
		mu.Lock()
		live++
		peak = max(peak, live)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		live--
		mu.Unlock()
		return nil
	}
	var tests []core.Test
	for _, n := range []string{"perf/a", "perf/b", "perf/c"} {
		tests = append(tests, core.Test{Name: n, Suite: "s", Category: core.Perf, Run: body,
			Env: core.EnvSpec{Sharing: core.Exclusive, Shape: core.SingleStack, ClusterSlots: 4}})
	}
	summary, _ := Run(context.Background(), tests, Options{
		RunOptions:   core.RunOptions{Parallel: 3, Seed: core.NewRunSeed(1, true), Config: core.NewConfig(), Provider: &fake.Provider{}},
		Capacity:     3,
		ClusterSlots: slots,
	})
	if summary.Counts[core.StatusPassed] != 3 {
		t.Fatalf("counts = %v", summary.Counts)
	}
	if peak != 1 {
		t.Errorf("three 4-slot tests on a 6-slot cluster ran %d at once, want 1", peak)
	}
	if st.held() != 0 {
		t.Errorf("%d slots still held after the run", st.held())
	}
}

type gatedFake struct{ fake.Provider }

func (*gatedFake) SharesInfraAcrossTests() bool { return true }

// An exclusive test that held the infra gate while it queued for a lease could
// wait on a shared worker whose writer waits on that same gate.
func TestExclusiveWaitsForItsLeaseOutsideTheInfraGate(t *testing.T) {
	slots := newClusterSlots(&memSlotStore{}, 1, nil)
	slots.poll = 5 * time.Millisecond
	other, err := slots.Acquire(context.Background(), "another run", 1)
	if err != nil {
		t.Fatal(err)
	}
	p := &gatedFake{}
	var once sync.Once
	p.OnProvision = func(*fake.Env) {
		once.Do(func() {
			other()
			time.Sleep(200 * time.Millisecond)
		})
	}
	pass := func(*core.T) error { return nil }
	tests := []core.Test{
		{Name: "x", Suite: "s", Category: core.Functional, Run: pass,
			Env: core.EnvSpec{Sharing: core.Exclusive, Shape: core.ClusterOnly, ClusterSlots: 1}},
		{Name: "w", Suite: "s", Category: core.Functional, Run: pass,
			Env: core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack, FaultsSharedInfra: true}},
	}
	done := make(chan core.RunSummary, 1)
	go func() {
		summary, _ := Run(context.Background(), tests, Options{
			RunOptions:   core.RunOptions{Parallel: 4, Seed: core.NewRunSeed(1, true), Config: core.NewConfig(), Provider: p},
			Capacity:     1,
			ClusterSlots: slots,
		})
		done <- summary
	}()
	select {
	case summary := <-done:
		if summary.Counts[core.StatusPassed] != 2 {
			t.Errorf("counts = %v, want both passed", summary.Counts)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run deadlocked: the exclusive test holds the infra gate waiting for the lease the faulting test's worker holds")
	}
}
