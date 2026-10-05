package sched

import (
	"context"

	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
)

// Cluster slots cap how many self-provisioned stacks run on one k8s cluster at
// once, across every run pointed at it. The lease Manager only sees its own
// run: eight perf runs triggered side by side each admitted their stacks, the
// cluster autoscaler hit its CPU limit, and endpoint pods sat unschedulable
// until their 35m deploy timeout. A test declares its stacks in
// EnvSpec.ClusterSlots and waits here, before its body and its timeout start.
//
// The holders live in one store (a ConfigMap, see kubeSlotStore) updated
// read-modify-write with a version check, so two processes cannot both see room
// that only one of them fits in. Every entry carries an expiry its owner
// renews, so a run that died holding slots frees them after slotTTL. Waiters
// queue too, and admission is strict FIFO: a six-stack benchmark is not starved
// by a stream of two-stack tests that fit around it.

const (
	slotTTL          = 10 * time.Minute
	slotRenewEvery   = slotTTL / 4
	slotPollEvery    = 30 * time.Second
	slotReleaseTries = 20
)

// slotEntry is one holder or waiter.
type slotEntry struct {
	Units    int    `json:"units"`
	Held     bool   `json:"held"`
	Since    int64  `json:"since"`   // unix nanos the entry first queued; FIFO order
	Expires  int64  `json:"expires"` // unix seconds
	Holder   string `json:"holder"`  // the test, for whoever reads the ConfigMap
	Capacity int    `json:"capacity,omitempty"`
}

// slotStore is the shared state: load returns the entries and an opaque
// version, store writes them back only if the version is still current.
type slotStore interface {
	load(ctx context.Context) (map[string]slotEntry, string, error)
	store(ctx context.Context, entries map[string]slotEntry, version string) (conflict bool, err error)
}

// ClusterSlots is a counting semaphore over one cluster.
type ClusterSlots struct {
	st       slotStore
	capacity int
	now      func() time.Time
	logf     func(string, ...any)
	poll     time.Duration
}

func newClusterSlots(st slotStore, capacity int, logf func(string, ...any)) *ClusterSlots {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &ClusterSlots{st: st, capacity: max(capacity, 1), now: time.Now, logf: logf, poll: slotPollEvery}
}

// admit is the pure decision: drop expired entries, then let id take its units
// when it is the oldest waiter and they fit. A request larger than capacity is
// clamped so it can run alone rather than never.
func admit(entries map[string]slotEntry, id string, capacity int, now time.Time) (map[string]slotEntry, bool, int) {
	out := map[string]slotEntry{}
	for k, e := range entries {
		if e.Expires >= now.Unix() {
			out[k] = e
		}
	}
	me, ok := out[id]
	if !ok || me.Held {
		return out, ok && me.Held, heldUnits(out)
	}
	used := heldUnits(out)
	for k, e := range out {
		if k != id && !e.Held && (e.Since < me.Since || (e.Since == me.Since && k < id)) {
			return out, false, used
		}
	}
	if used+min(me.Units, capacity) > capacity {
		return out, false, used
	}
	me.Held = true
	out[id] = me
	return out, true, used
}

func heldUnits(entries map[string]slotEntry) int {
	n := 0
	for _, e := range entries {
		if e.Held {
			n += e.Units
		}
	}
	return n
}

// Acquire blocks until units are held for holder, or ctx ends. The returned
// release frees them; it is safe to call more than once.
func (c *ClusterSlots) Acquire(ctx context.Context, holder string, units int) (func(), error) {
	id := fmt.Sprintf("%s-%d", slotKey(holder), rand.Int63())
	units = min(max(units, 1), c.capacity)
	since := c.now().UnixNano()
	lastLog := time.Time{}
	for {
		held, used, err := c.update(ctx, func(m map[string]slotEntry) (map[string]slotEntry, bool, int) {
			e, ok := m[id]
			if !ok {
				e = slotEntry{Units: units, Since: since, Holder: holder, Capacity: c.capacity}
			}
			e.Expires = c.now().Add(slotTTL).Unix()
			m[id] = e
			return admit(m, id, c.capacity, c.now())
		})
		if err != nil {
			c.leave(id)
			return nil, err
		}
		if held {
			c.logf("sched: %s holds %d/%d cluster slots", holder, units, c.capacity)
			return c.keep(id), nil
		}
		if time.Since(lastLog) > 5*time.Minute {
			c.logf("sched: %s waits for %d cluster slots (%d/%d held)", holder, units, used, c.capacity)
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			c.leave(id)
			return nil, ctx.Err()
		case <-time.After(c.poll + time.Duration(rand.Int63n(int64(c.poll/3)+1))):
		}
	}
}

// slotKey maps a test name onto a ConfigMap data key ([-._a-zA-Z0-9]).
func slotKey(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c == '-' || c == '.' || c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '.'
		}
	}
	return string(b)
}

// DefaultClusterSlots is the stack budget one cluster gives self-provisioned
// tests. A benchmark stack with its live arm reserves ~11 CPU,
// and itest-dev's autoscaler stops at 512 CPU shared with everything else on it,
// so 12 is two benchmarks, or one and three two-stack tests.
const DefaultClusterSlots = 12

// ClusterSlotsFromConfig builds the cap from sched.cluster_slots.capacity on
// the cluster the run names (apps.ybk8s.kubecontext / kubeconfig, else
// env.k8s.context). nil when the capacity is 0
// or the run names no cluster: the ambient kube context is never assumed.
func ClusterSlotsFromConfig(c *core.Config, logf func(string, ...any)) *ClusterSlots {
	capacity := c.GetInt("sched.cluster_slots.capacity", DefaultClusterSlots)
	if capacity <= 0 {
		return nil
	}
	kubeconfig := c.GetString("apps.ybk8s.kubeconfig", "")
	kctx := ""
	for _, k := range []string{"apps.ybk8s.kubecontext", "env.k8s.context"} {
		if kctx = c.GetString(k, ""); kctx != "" {
			break
		}
	}
	if kctx == "" && kubeconfig == "" {
		return nil
	}
	kube := k8s.New(k8s.Config{Context: kctx})
	if kubeconfig != "" {
		kube = kube.WithRunner(k8s.KubeconfigRunner(kubeconfig))
	}
	return newClusterSlots(newKubeSlotStore(kube), capacity, logf)
}

// update applies fn under the store's version check, retrying on conflict.
func (c *ClusterSlots) update(ctx context.Context, fn func(map[string]slotEntry) (map[string]slotEntry, bool, int)) (bool, int, error) {
	for {
		m, v, err := c.st.load(ctx)
		if err != nil {
			return false, 0, fmt.Errorf("cluster slots: %w", err)
		}
		if m == nil {
			m = map[string]slotEntry{}
		}
		next, ok, used := fn(m)
		conflict, err := c.st.store(ctx, next, v)
		if err != nil {
			return false, 0, fmt.Errorf("cluster slots: %w", err)
		}
		if !conflict {
			return ok, used, nil
		}
		select {
		case <-ctx.Done():
			return false, 0, ctx.Err()
		case <-time.After(time.Duration(rand.Int63n(int64(time.Second)))):
		}
	}
}

// keep renews id's expiry until the returned release runs.
func (c *ClusterSlots) keep(id string) func() {
	stop := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(slotRenewEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				_, _, err := c.update(ctx, func(m map[string]slotEntry) (map[string]slotEntry, bool, int) {
					if e, ok := m[id]; ok {
						e.Expires = c.now().Add(slotTTL).Unix()
						m[id] = e
					}
					return m, true, 0
				})
				cancel()
				if err != nil {
					c.logf("sched: renew cluster slots %s: %v (expires in %s if it keeps failing)", id, err, slotTTL)
				}
			}
		}
	}()
	return func() {
		once.Do(func() {
			close(stop)
			c.leave(id)
		})
	}
}

// leave removes id, best-effort: an entry left behind expires on its own.
func (c *ClusterSlots) leave(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for range slotReleaseTries {
		_, _, err := c.update(ctx, func(m map[string]slotEntry) (map[string]slotEntry, bool, int) {
			delete(m, id)
			return m, true, 0
		})
		if err == nil {
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			break
		}
	}
	c.logf("sched: could not release cluster slots %s; they expire within %s", id, slotTTL)
}
