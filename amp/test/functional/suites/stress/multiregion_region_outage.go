// A whole region of the multi-region YugabyteDB under amp goes down while
// tenants write through their endpoints. The universe is this test's own
// (core.EnvSpec.BackingConfig): RF3 over three GCE regions, leaders preferred
// beside the control plane. Every node of the target region is stopped through
// YBA, the write path is held to a bounded stall, the region is started again,
// and every acknowledged row is read back.
package stress

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/multiregion"
	"github.com/yugabyte/amp/test/functional/framework/apps/yba"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/universe"
)

func init() {
	core.Register(core.Test{
		Name:     "stress/multiregion_region_outage",
		Suite:    "stress",
		Category: core.Stress,
		Env: core.EnvSpec{
			Sharing:           core.Exclusive,
			Shape:             core.DedicatedStack,
			FaultsSharedInfra: true,
			BackingConfig:     multiregion.BackingConfig,
		},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Stop every node of one region of the multi-region YugabyteDB under amp (the leader-preferred region by default) while tenants write through their endpoints, hold the write path to a bounded stall, start the region again, and assert every acknowledged row is stored and the control plane never answered a 500.",
		Params:      []string{"stress.multiregion_outage", "multiregion", "stress.consistency", "stress.settle"},
		Preflight:   multiregion.Preflight,
		Timeout:     3 * time.Hour,
		Run:         runMultiRegionOutage,
	})
}

type regionOutageParams struct {
	Tenants      int
	PayloadBytes int
	// Target is the region to take down: preferred, secondary or a region code.
	Target string
	// Duration is how long the region stays down with the writers running.
	Duration time.Duration
	// MaxWriteStall bounds the longest window in which no tenant committed a
	// row while the region was down.
	MaxWriteStall time.Duration
	SettleTimeout time.Duration
	// ResumeDuration is the throughput window after the region is back.
	ResumeDuration          time.Duration
	ControlPlaneRecoveryMax time.Duration
}

func regionOutageParamsFromConfig(c *core.Config) regionOutageParams {
	return regionOutageParams{
		Tenants:                 c.GetInt("stress.multiregion_outage.tenants", 3),
		PayloadBytes:            c.GetInt("stress.multiregion_outage.payload_bytes", 256),
		Target:                  c.GetString("stress.multiregion_outage.target", "preferred"),
		Duration:                c.GetDuration("stress.multiregion_outage.duration", 5*time.Minute),
		MaxWriteStall:           c.GetDuration("stress.multiregion_outage.max_write_stall", 90*time.Second),
		SettleTimeout:           c.GetDuration("stress.multiregion_outage.settle_timeout", 15*time.Minute),
		ResumeDuration:          c.GetDuration("stress.multiregion_outage.resume_duration", 30*time.Second),
		ControlPlaneRecoveryMax: c.GetDuration("stress.multiregion_outage.control_plane_recovery_max", 2*time.Minute),
	}
}

// nodeActor is the per-node control the outage needs from the YBA behind the
// backing; *yba.Client has it, the maintainer interface does not name it.
type nodeActor interface {
	NodesActionAndWait(ctx context.Context, universeID string, nodes []string, action yba.NodeAction, settle time.Duration) error
}

type universeEnv interface {
	YBAUniverse() (universe.YBAMaintainer, string)
}

func runMultiRegionOutage(t *core.T) error {
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane: the workload writes THROUGH amp")
		return nil
	}
	uenv, ok := t.Env.(universeEnv)
	if !ok {
		t.Skip("requires an env exposing its backing universe")
		return nil
	}
	yb, universeID := uenv.YBAUniverse()
	if yb == nil {
		t.Skip("the backing is not a YBA universe: the run moved this test off its own stack")
		return nil
	}
	actor, ok := yb.(nodeActor)
	if !ok {
		return fmt.Errorf("the YBA client %T cannot stop nodes", yb)
	}
	params := regionOutageParamsFromConfig(t.Options)
	shape := multiregion.FromConfig(t.Options)
	target, err := shape.ResolveTarget(params.Target)
	if err != nil {
		return fmt.Errorf("stress.multiregion_outage.target: %w", err)
	}
	logf := t.Masker().Logf(log.Printf)

	var nodes []string
	if err := t.Step("universe_shape", func(s *core.StepHandle) error {
		u, err := yb.GetUniverse(t.Context, universeID)
		if err != nil {
			return core.EnvFail("read universe %s: %w", universeID, err)
		}
		s.Set("universe", u.Name)
		s.Set("topology", u.DescribeTopology())
		s.Set("target_region", target)
		if err := shape.Check(u); err != nil {
			return err
		}
		nodes = u.RegionNodes()[target]
		if len(nodes) == 0 {
			return fmt.Errorf("region %s has no tserver to stop", target)
		}
		// Taking the target region out has to leave a write quorum, or the
		// universe stops serving for a legitimate reason and every assertion
		// below would blame amp for the shape this run was given.
		surviving := shape.Nodes() - len(nodes)
		if quorum := shape.ReplicationFactor/2 + 1; surviving < quorum {
			return fmt.Errorf("stopping region %s takes %d of %d node(s), leaving %d for an RF%d universe that needs %d to commit: pick a region holding a minority of the replicas (stress.multiregion_outage.target)",
				target, len(nodes), shape.Nodes(), surviving, shape.ReplicationFactor, quorum)
		}
		s.Set("target_nodes", strings.Join(nodes, ","))
		s.Set("surviving_nodes", surviving)
		return nil
	}); err != nil {
		return err
	}

	fleet, err := newTenantFleet(t, env, logf)
	if err != nil {
		return err
	}
	defer fleet.Close()

	if err := t.Step("create_tenants", func(s *core.StepHandle) error {
		if err := fleet.Create(t, params.Tenants); err != nil {
			return err
		}
		s.Set("tenants", len(fleet.Tenants))
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("write_baseline_data", func(s *core.StepHandle) error {
		runWriters(t.Context, env, fleet.Tenants, 20*time.Second, params.PayloadBytes)
		rows := totalWritten(fleet.Tenants)
		s.Set("baseline_rows", rows)
		if rows == 0 {
			return fmt.Errorf("no tenant committed a row before the outage")
		}
		return nil
	}); err != nil {
		return err
	}

	var (
		cpRecovery time.Duration
		cpMeasured bool
		gate       *settleGate
	)
	regionUp := false
	startRegion := func(ctx context.Context) error {
		if regionUp {
			return nil
		}
		if err := actor.NodesActionAndWait(ctx, universeID, nodes, yba.NodeStart, params.SettleTimeout); err != nil {
			return err
		}
		regionUp = true
		return nil
	}
	// The region comes back whatever happened in between: a universe left with
	// a stopped region would fail its own teardown's health checks.
	defer func() {
		if regionUp {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), params.SettleTimeout)
		defer cancel()
		if err := startRegion(ctx); err != nil {
			logf("multiregion outage: start region %s (best-effort): %v", target, err)
		}
	}()

	if err := t.Step("region_outage", func(s *core.StepHandle) error {
		var err error
		gate, err = awaitSteadyState(t, s, env, fleet.Tenants, nil, params.PayloadBytes, 0)
		if err != nil {
			return err
		}
		// The nodes taken down are GCE VMs, so no amp pod is a target of this
		// fault: amp riding out the loss of a minority region without its own
		// pods restarting IS the subject, which makes a restart here a product
		// failure rather than environment damage.
		gate.asProductFailure(t.Options)
		probe := newControlPlaneProbe(fleet.cl, fleet.poolID)
		probe.Start(t.Context)

		loadCtx, stopLoad := context.WithCancel(t.Context)
		defer stopLoad()
		loadDone := make(chan struct{})
		go func() {
			defer close(loadDone)
			runWriters(loadCtx, env, fleet.Tenants, params.Duration+params.SettleTimeout, params.PayloadBytes)
		}()
		stopStart := time.Now()
		if err := actor.NodesActionAndWait(t.Context, universeID, nodes, yba.NodeStop, params.SettleTimeout); err != nil {
			probe.Abort()
			return core.EnvFail("stop region %s through YBA: %w", target, err)
		}
		s.Set("stop_took", time.Since(stopStart).Round(time.Second).String())
		// Measured from here, not from before the stop: YBA's stop is a graceful
		// drain lasting over a minute, and writes pausing while a node is being
		// taken out on purpose is not the outage this test is about. Starting
		// the clock earlier charges that drain to recovery and makes the number
		// unattributable.
		stall := newStallTracker(fleet.Tenants)
		stall.Start(loadCtx)
		logf("multiregion outage: region %s down (%s); writers run for %s", target, strings.Join(nodes, ","), params.Duration)

		select {
		case <-time.After(params.Duration):
		case <-t.Context.Done():
			probe.Abort()
			return t.Context.Err()
		}
		downRows := stall.Written()
		stopLoad()
		<-loadDone
		maxStall, firstCommit, resumed := stall.Stop()

		s.Set("rows_during_outage", downRows)
		s.Set("write_stall_max", maxStall.Round(time.Second).String())
		t.Metric("multiregion_outage_write_stall_seconds", maxStall.Seconds(), "s")
		t.Metric("multiregion_outage_rows_committed", float64(downRows), "count")
		// Time to the FIRST acknowledged commit once the region was down is the
		// recovery number; the longest gap can also be a mid-run hiccup.
		if resumed {
			s.Set("first_commit_after_outage", firstCommit.Round(time.Second).String())
			t.Metric("multiregion_outage_first_commit_seconds", firstCommit.Seconds(), "s")
		} else {
			s.Set("first_commit_after_outage", "never")
		}

		startStart := time.Now()
		if err := startRegion(t.Context); err != nil {
			probe.Abort()
			return core.EnvFail("start region %s through YBA: %w", target, err)
		}
		s.Set("start_took", time.Since(startStart).Round(time.Second).String())

		cpRecovery, cpMeasured = probe.Stop(time.Now(), params.SettleTimeout)
		tally, bad := probe.Summary()
		s.Set("control_plane_probe", tally)
		s.Set("control_plane_recovery", cpRecovery.Round(time.Second).String())
		if bad > 0 {
			return fmt.Errorf("the controller answered %d non-503 5xx response(s) while region %s was down (%s); an unreachable store is a retryable 503, never a 500 (meko#886)", bad, target, tally)
		}
		if !resumed {
			return fmt.Errorf("no tenant committed a row in the %s after region %s finished stopping: with %d of %d nodes surviving the universe still has a write quorum",
				params.Duration, target, shape.Nodes()-len(nodes), shape.Nodes())
		}
		if firstCommit > params.MaxWriteStall {
			// Only the preferred region held the leaders; say so only when that
			// is the region this run actually stopped.
			why := "the surviving regions hold a quorum and should have carried the writes"
			if target == shape.PreferredRegion {
				why = "every tablet leader was in that region, so recovery is a leader move amp has to follow"
			}
			return fmt.Errorf("the write path took %s to commit again after region %s went down, over the %s allowed: %s",
				firstCommit.Round(time.Second), target, params.MaxWriteStall, why)
		}
		if maxStall > params.MaxWriteStall {
			return fmt.Errorf("writes resumed %s after region %s went down but then stalled %s later in the window, over the %s allowed",
				firstCommit.Round(time.Second), target, maxStall.Round(time.Second), params.MaxWriteStall)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("endpoints_serve_again", func(s *core.StepHandle) error {
		if err := assertEndpointsServe(t, env, fleet.Tenants, params.SettleTimeout); err != nil {
			return err
		}
		return gate.attestNonTarget(t, s)
	}); err != nil {
		return err
	}

	if err := t.Step("writes_flow_again", func(s *core.StepHandle) error {
		waited, err := awaitWritePath(t.Context, env, fleet.Tenants, nil, params.SettleTimeout, params.PayloadBytes)
		if err != nil {
			return fmt.Errorf("the write path never came back after region %s returned: %w", target, err)
		}
		s.Set("write_path_back_after", waited.Round(time.Second).String())
		before := totalWritten(fleet.Tenants)
		runWriters(t.Context, env, fleet.Tenants, params.ResumeDuration, params.PayloadBytes)
		delta := totalWritten(fleet.Tenants) - before
		s.Set("rows_after_outage", delta)
		if delta == 0 {
			return fmt.Errorf("every tenant committed a probe row, then none committed anything in the following %s", params.ResumeDuration)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("verify_data", func(s *core.StepHandle) error {
		return verifyTenantData(t, s, env, fleet.Tenants, params.PayloadBytes)
	}); err != nil {
		return err
	}

	if err := t.Step("control_plane_recovered", func(s *core.StepHandle) error {
		waited, err := awaitControlPlaneReady(t, fleet, params.SettleTimeout)
		if err != nil {
			return err
		}
		s.Set("ready_after", waited.Round(time.Second).String())
		if !cpMeasured {
			return fmt.Errorf("the controller never answered a probe 2xx in the %s after region %s came back", params.SettleTimeout, target)
		}
		if cpRecovery > params.ControlPlaneRecoveryMax {
			return fmt.Errorf("the controller kept answering errors for %s after region %s came back, over the %s allowed (meko#961)",
				cpRecovery.Round(time.Second), target, params.ControlPlaneRecoveryMax)
		}
		return nil
	}); err != nil {
		return err
	}

	return t.Step("amcheck", func(s *core.StepHandle) error {
		return verifyPhysicalConsistency(t, s, env, fleet.Tenants)
	})
}

// stallTracker samples the tenants' acknowledged-row counters and keeps the
// longest window in which none advanced.
type stallTracker struct {
	tenants []tenant
	base    int64
	start   time.Time
	done    chan struct{}
	stopped chan struct{}
	max     time.Duration
	// first is how long after Start the first row was acknowledged; zero until
	// one is, which resumed distinguishes from "immediately".
	first   time.Duration
	resumed bool
	written int64
}

func newStallTracker(tenants []tenant) *stallTracker {
	return &stallTracker{tenants: tenants, base: totalWritten(tenants), done: make(chan struct{}), stopped: make(chan struct{})}
}

func (s *stallTracker) Start(ctx context.Context) {
	s.start = time.Now()
	go func() {
		defer close(s.stopped)
		last, lastAt := totalWritten(s.tenants), time.Now()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-s.done:
				s.observe(last, lastAt)
				return
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			now := totalWritten(s.tenants)
			if now != last {
				if !s.resumed {
					s.resumed, s.first = true, time.Since(s.start)
				}
				last, lastAt = now, time.Now()
				atomic.StoreInt64(&s.written, now-s.base)
				continue
			}
			s.observe(last, lastAt)
		}
	}()
}

func (s *stallTracker) observe(last int64, lastAt time.Time) {
	if gap := time.Since(lastAt); gap > s.max {
		s.max = gap
	}
	atomic.StoreInt64(&s.written, last-s.base)
}

// Written is how many rows were acknowledged since the tracker started.
func (s *stallTracker) Written() int64 { return atomic.LoadInt64(&s.written) }

// Stop ends sampling and reports the longest window without an acknowledgement,
// how long the first one took, and whether any arrived at all.
func (s *stallTracker) Stop() (max, first time.Duration, resumed bool) {
	close(s.done)
	<-s.stopped
	return s.max, s.first, s.resumed
}
