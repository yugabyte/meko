// The operations that move where the data or its leadership lives. Each runs
// under the tenant workloads and is judged by the longest span they went
// without an acknowledged commit: amp is meant to ride a leader change, a split
// or a node edit out, so a stall past resume_deadline is a product finding.
package stress

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ybadmin"
)

func (r *maintenanceRun) ensureAdmin(ctx context.Context) (*ybadmin.Client, error) {
	if r.admin != nil {
		return r.admin, nil
	}
	mover, ok := r.target.(dataMover)
	if !ok || !mover.SupportsDataMove() {
		return nil, fmt.Errorf("%s has nowhere in-cluster to run yb-admin", r.target.Describe())
	}
	admin, err := mover.NewAdmin(ctx, r.logf)
	if err != nil {
		return nil, err
	}
	r.admin = admin
	return admin, nil
}

// Sampling the acknowledged-commit counter rather than timing each INSERT keeps
// this off the writers' connections, which the writers own.
type stallWatch struct {
	stop    context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	longest time.Duration
}

func (r *maintenanceRun) watchStalls(ctx context.Context) *stallWatch {
	return newStallWatch(ctx, r.cfg.StallSampleInterval, func() int64 { return totalWritten(r.fleet.Tenants) })
}

func newStallWatch(ctx context.Context, interval time.Duration, written func() int64) *stallWatch {
	if interval <= 0 {
		interval = DefaultMaintenanceStallSample
	}
	wctx, cancel := context.WithCancel(ctx)
	w := &stallWatch{stop: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		last := time.Now()
		seen := written()
		for {
			if err := sleepCtx(wctx, interval); err != nil {
				w.note(time.Since(last))
				return
			}
			if now := written(); now > seen {
				seen = now
				w.note(time.Since(last))
				last = time.Now()
			}
		}
	}()
	return w
}

func (w *stallWatch) note(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if d > w.longest {
		w.longest = d
	}
}

func (w *stallWatch) Longest() time.Duration {
	w.stop()
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.longest
}

func (r *maintenanceRun) underStallWatch(ctx context.Context, op string, fn func(context.Context) error) error {
	w := r.watchStalls(ctx)
	err := fn(ctx)
	stall := w.Longest()
	r.t.Metric("write_stall_during_"+op+"_s", stall.Seconds(), "s")
	if err != nil {
		return err
	}
	if stall > r.cfg.ResumeDeadline {
		return fmt.Errorf("the tenants went %s without an acknowledged commit during %s, and %s is the deadline: amp did not ride the topology change out",
			stall.Round(time.Second), op, r.cfg.ResumeDeadline)
	}
	r.logf("%s: longest commit-free span %s (deadline %s)", op, stall.Round(time.Second), r.cfg.ResumeDeadline)
	return nil
}

type ampTablet struct {
	table  string
	tablet ybadmin.Tablet
}

func (r *maintenanceRun) ampTablets(ctx context.Context, admin *ybadmin.Client) ([]ampTablet, error) {
	var out []ampTablet
	for _, name := range r.cfg.TabletTables {
		tbl, err := admin.FindTable(ctx, ampStorageDB, name)
		if err != nil {
			return nil, err
		}
		tablets, err := admin.ListTablets(ctx, tbl.ID, ybadmin.DefaultMaxTablets)
		if err != nil {
			return nil, fmt.Errorf("list the tablets of %s.%s: %w", tbl.Keyspace, tbl.Name, err)
		}
		for _, tab := range tablets {
			out = append(out, ampTablet{table: name, tablet: tab})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the backing reports no tablets for %s in %s, so there is no leadership to move",
			strings.Join(r.cfg.TabletTables, ", "), ampStorageDB)
	}
	return out, nil
}

// From the run's seed, so a replay walks the same tablets in the same order.
func (r *maintenanceRun) seededOrder(label string, n int) []int {
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	rnd := r.t.Seed.Fork(label).Rand()
	rnd.Shuffle(n, func(i, j int) { order[i], order[j] = order[j], order[i] })
	return order
}

func (r *maintenanceRun) leaderStepdown(ctx context.Context) error {
	admin, err := r.ensureAdmin(ctx)
	if err != nil {
		return err
	}
	tablets, err := r.ampTablets(ctx, admin)
	if err != nil {
		return err
	}
	return r.underStallWatch(ctx, OpLeaderStepdown, func(ctx context.Context) error {
		wctx, cancel := context.WithTimeout(ctx, r.cfg.StepdownDuration)
		defer cancel()
		done := 0
		for _, i := range r.seededOrder("leader-stepdown", len(tablets)) {
			at := tablets[i]
			if err := admin.LeaderStepdown(wctx, at.tablet.ID); err != nil {
				// A tablet whose leader moved on its own between the listing
				// and the call is not a failure of the product under test.
				if wctx.Err() != nil {
					break
				}
				r.logf("%s: stepping down %s tablet %s: %v", OpLeaderStepdown, at.table, at.tablet.ID, err)
				continue
			}
			done++
			if err := sleepCtx(wctx, r.cfg.StepdownInterval); err != nil {
				break
			}
		}
		r.t.Metric("tablet_leader_stepdowns", float64(done), "count")
		if done == 0 {
			return fmt.Errorf("not one of the %d %s tablets accepted a leader_stepdown", len(tablets), ampStorageDB)
		}
		r.logf("%s: stepped %d of %d tablet leaders down", OpLeaderStepdown, done, len(tablets))
		return nil
	})
}

// The pageserver's direct-lane client has to re-discover the new tablets, which
// is the part that has broken before.
func (r *maintenanceRun) tabletSplit(ctx context.Context) error {
	admin, err := r.ensureAdmin(ctx)
	if err != nil {
		return err
	}
	tbl, err := admin.FindTable(ctx, ampStorageDB, r.splitTable())
	if err != nil {
		return err
	}
	before, err := admin.ListTablets(ctx, tbl.ID, ybadmin.DefaultMaxTablets)
	if err != nil {
		return fmt.Errorf("list the tablets of %s.%s: %w", tbl.Keyspace, tbl.Name, err)
	}
	return r.underStallWatch(ctx, OpTabletSplit, func(ctx context.Context) error {
		var refusals []string
		split := ""
		for _, i := range r.seededOrder("tablet-split", len(before)) {
			id := before[i].ID
			if err := admin.SplitTablet(ctx, id); err != nil {
				refusals = append(refusals, fmt.Sprintf("%s: %v", id, err))
				continue
			}
			split = id
			break
		}
		if split == "" {
			return fmt.Errorf("no tablet of %s.%s could be split under load (%d tried): %s",
				tbl.Keyspace, tbl.Name, len(before), strings.Join(refusals, "; "))
		}
		r.logf("%s: split %s.%s tablet %s", OpTabletSplit, tbl.Keyspace, tbl.Name, split)
		if err := r.t.WaitFor(fmt.Sprintf("%s.%s to report more than %d tablets", tbl.Keyspace, tbl.Name, len(before)),
			r.cfg.SplitTimeout, ybadmin.DefaultPoll, func() (bool, error) {
				now, err := admin.ListTablets(ctx, tbl.ID, ybadmin.DefaultMaxTablets)
				if err != nil {
					return false, err
				}
				return len(now) > len(before), nil
			}); err != nil {
			return fmt.Errorf("the split of %s never produced new tablets: %w", split, err)
		}
		if err := admin.WaitLoadBalancerIdle(ctx, r.cfg.SplitTimeout, ybadmin.DefaultPoll); err != nil {
			return fmt.Errorf("the cluster never rebalanced after splitting %s: %w", split, err)
		}
		return nil
	})
}

// The last configured table grows fastest under the workloads, so it is the one
// with enough data to be splittable.
func (r *maintenanceRun) splitTable() string {
	if len(r.cfg.TabletTables) == 0 {
		return "pages"
	}
	return r.cfg.TabletTables[len(r.cfg.TabletTables)-1]
}

func (r *maintenanceRun) masterLeaderChange(ctx context.Context) error {
	admin, err := r.ensureAdmin(ctx)
	if err != nil {
		return err
	}
	return r.underStallWatch(ctx, OpMasterLeaderChange, func(ctx context.Context) error {
		for i := 0; i < r.cfg.MasterStepdowns; i++ {
			if err := admin.StepDownMasterLeader(ctx); err != nil {
				return fmt.Errorf("step the master leader down (%d of %d): %w", i+1, r.cfg.MasterStepdowns, err)
			}
			if err := sleepCtx(ctx, r.cfg.StepdownInterval); err != nil {
				return err
			}
		}
		r.t.Metric("master_leader_stepdowns", float64(r.cfg.MasterStepdowns), "count")
		return r.target.Health(ctx)
	})
}

// The backing gains an address and then loses one while WAL flows, the window
// the stale-host-list class of bug (meko#811) lives in.
func (r *maintenanceRun) addRemoveTServer(ctx context.Context) error {
	yt, ok := r.target.(*ybaTarget)
	if !ok {
		return fmt.Errorf("%s is not a YBA universe, so there is no edit API to add a node through", r.target.Describe())
	}
	u, err := yt.client.GetUniverse(ctx, yt.universeID)
	if err != nil {
		return fmt.Errorf("read universe %s: %w", yt.universeID, err)
	}
	nodes := 0
	for _, n := range u.UniverseDetails.NodeDetailsSet {
		if n.IsTserver {
			nodes++
		}
	}
	if nodes < r.cfg.RF {
		return fmt.Errorf("the backing has %d tserver(s) and rf is %d: removing one would take the write quorum with it", nodes, r.cfg.RF)
	}
	return r.underStallWatch(ctx, OpAddRemoveTServer, func(ctx context.Context) error {
		r.logf("%s: growing the universe from %d to %d tservers", OpAddRemoveTServer, nodes, nodes+1)
		if err := yt.client.EditUniverseNodesAndWait(ctx, yt.universeID, nodes+1, r.cfg.EditTimeout); err != nil {
			return fmt.Errorf("add a tserver: %w", err)
		}
		if err := r.target.Health(ctx); err != nil {
			return err
		}
		r.logf("%s: draining the universe back to %d tservers", OpAddRemoveTServer, nodes)
		if err := yt.client.EditUniverseNodesAndWait(ctx, yt.universeID, nodes, r.cfg.EditTimeout); err != nil {
			return fmt.Errorf("remove a tserver: %w", err)
		}
		return r.target.Health(ctx)
	})
}

// The slow-replica case. tc cannot slow a disk from outside the pod, and the
// intra-backing replication path is where the slowness shows up anyway.
// network_slowdown in the chaos registry targets the whole pod network instead.
func (r *maintenanceRun) diskLatency(ctx context.Context) error {
	kube := r.kube()
	if kube == nil {
		return fmt.Errorf("%s is not a kubernetes universe, so there is no pod network namespace to slow", r.target.Describe())
	}
	pods, err := backingPods(r.t, kube)
	if err != nil {
		return err
	}
	if len(pods) == 0 {
		return fmt.Errorf("no backing pod in namespace %s to slow", kube.Namespace())
	}
	victim := pods[r.seededOrder("disk-latency", len(pods))[0]]
	slow, err := slowPodToPeers(r.t, kube, victim, r.t.ResourceSuffix()+"-slow", tserverRPCPort, r.cfg.DiskLatency)
	if err != nil {
		return err
	}
	defer func() {
		if err := slow.Heal(ctx); err != nil {
			r.logf("%s: removing the delay on %s (best-effort): %v", OpDiskLatency, victim, err)
		}
	}()
	r.logf("%s: %s answering its peers %s late for %s", OpDiskLatency, victim, r.cfg.DiskLatency, r.cfg.DiskLatencyDuration)
	return r.underStallWatch(ctx, OpDiskLatency, func(ctx context.Context) error {
		if err := sleepCtx(ctx, r.cfg.DiskLatencyDuration); err != nil {
			return err
		}
		if err := slow.Heal(ctx); err != nil {
			return fmt.Errorf("remove the delay on %s: %w", victim, err)
		}
		return r.target.Health(ctx)
	})
}
