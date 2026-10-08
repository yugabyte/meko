// A foundation node that is UP but unreachable. stress/backing_nemesis deletes
// backing pods, so every peer and client sees a connection refused within a
// round trip; a partitioned node answers nothing, and clients sit in RPC
// timeouts instead. With RF3 and one tserver cut off, YugabyteDB keeps a quorum
// and amp's storage daemons must find the new leaders: a COMMIT that hangs past
// the stall limit while a quorum serves is the finding (the meko#727 shape),
// as is any acknowledged row missing once the node is back.
//
// stress/backing_node_partition_multi_region is the same fault with the
// backing's tservers and the tenants' endpoints first placed the round trips
// of a multi-region deployment apart (apps/netshape, tc netem in the pods'
// own network namespaces): a region cut off while the surviving quorum is
// tens of milliseconds apart, which is the outage a real multi-region universe
// has and a co-located one cannot show.
package stress

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/netshape"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const partitionRestartIsFailKey = "stress.backing_node_partition.non_target_restart_is_failure"

func init() {
	core.Register(core.Test{
		Name:        "stress/backing_node_partition",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack, FaultsSharedInfra: true},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Cut one backing tserver pod off the network (iptables in its own network namespace dropping every packet, the pod stays up) while tenants write through amp, and hold the write path to never stalling past the stall limit: with a quorum left, writes must keep flowing. Lift the partition, demand the node rejoins, every acknowledged row reads back, writes flow, and the control plane still creates a tenant.",
		Params:      []string{"stress.backing_node_partition", "stress.consistency", "stress.fault_metrics"},
		Timeout:     90 * time.Minute,
		Run:         func(t *core.T) error { return runBackingNodePartition(t, "") },
	})
	core.Register(core.Test{
		Name:        "stress/backing_node_partition_multi_region",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack, FaultsSharedInfra: true},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamControlPlane,
		Description: "stress/backing_node_partition with the backing tservers and the tenants' endpoints first placed a multi-region deployment's round trips apart (tc netem in their own network namespaces, stress.backing_node_partition.multi_region_topology, every round trip verified with ping): one region's tserver cut off while tenants write through amp and the surviving quorum is tens of milliseconds apart, the write path held to the stall limit, the node rejoined, every acknowledged row read back.",
		Params:      []string{"stress.backing_node_partition", "stress.partition", "stress.consistency", "stress.fault_metrics"},
		Timeout:     2 * time.Hour,
		Run: func(t *core.T) error {
			return runBackingNodePartition(t, t.Options.GetString("stress.backing_node_partition.multi_region_topology", "multi_region: 0-1=12 0-2=47 1-2=60"))
		},
	})
}

type backingNodePartitionParams struct {
	Tenants          int
	Rounds           int
	BaselineDuration time.Duration
	// HoldDuration is how long each partition stays applied while tenants write.
	HoldDuration time.Duration
	// StallLimit is how long one INSERT may take before it counts as hung.
	StallLimit time.Duration
	// MinBackingPods is the smallest backing that holds the promise: below RF3
	// one node out legitimately stops the writes.
	MinBackingPods          int
	SettleTimeout           time.Duration
	ResumeDuration          time.Duration
	PayloadBytes            int
	ControlPlaneRecoveryMax time.Duration
}

func backingNodePartitionParamsFromConfig(c *core.Config) backingNodePartitionParams {
	return backingNodePartitionParams{
		Tenants:                 c.GetInt("stress.backing_node_partition.tenants", 3),
		Rounds:                  c.GetInt("stress.backing_node_partition.rounds", 2),
		BaselineDuration:        c.GetDuration("stress.backing_node_partition.baseline_duration", 20*time.Second),
		HoldDuration:            c.GetDuration("stress.backing_node_partition.hold_duration", 5*time.Minute),
		StallLimit:              c.GetDuration("stress.backing_node_partition.stall_limit", 2*time.Minute),
		MinBackingPods:          c.GetInt("stress.backing_node_partition.min_backing_pods", 3),
		SettleTimeout:           c.GetDuration("stress.backing_node_partition.settle_timeout", 10*time.Minute),
		ResumeDuration:          c.GetDuration("stress.backing_node_partition.resume_duration", 30*time.Second),
		PayloadBytes:            c.GetInt("stress.backing_node_partition.payload_bytes", 256),
		ControlPlaneRecoveryMax: c.GetDuration("stress.backing_node_partition.control_plane_recovery_max", 2*time.Minute),
	}
}

// topologySpec, when non-empty, is the netshape topology the backing and the
// endpoints are shaped into before any write; tserver N by name order is site
// N, the endpoints sit in the topology's endpoint site.
func runBackingNodePartition(t *core.T, topologySpec string) error {
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	backingEnv, ok := t.Env.(backingKubeEnv)
	if !ok || backingEnv.BackingKube() == nil {
		t.Skip("requires a kube handle for the backing's namespace: a partition has nothing to select without it")
		return nil
	}
	params := backingNodePartitionParamsFromConfig(t.Options)
	if params.Rounds < 1 {
		return fmt.Errorf("stress.backing_node_partition.rounds must be at least 1, got %d: no fault would be injected", params.Rounds)
	}
	logf := t.Masker().Logf(log.Printf)
	bkube := backingEnv.BackingKube()
	var ampKube *k8s.Kubectl
	if ae, ok := t.Env.(ampKubeEnv); ok {
		ampKube = ae.AmpKube()
	}
	var topo *netshape.Topology
	if topologySpec != "" {
		tp, err := netshape.ParseTopology(topologySpec)
		if err != nil {
			return err
		}
		if ampKube == nil {
			t.Skip("requires a kube handle for the control-plane namespace: the endpoint pods are shaped too")
			return nil
		}
		topo = &tp
	}

	ips, err := backingPodIPs(t, bkube)
	if err != nil {
		return err
	}
	if len(ips) < params.MinBackingPods {
		t.Skipf("the backing runs %d serving pod(s) in %q, under the %d a partition of one leaves a quorum in; below that a node out stops the writes for a legitimate reason",
			len(ips), bkube.Namespace(), params.MinBackingPods)
		return nil
	}

	fleet, err := newTenantFleet(t, env, logf)
	if err != nil {
		return err
	}
	defer fleet.Close()

	pssrc, psspecs := pageserverFaultMetrics()
	faults := startFaultMetrics(t, ampKube, pssrc, psspecs, logf)
	defer faults.stop()

	var journals []*AckJournal
	if err := t.Step("create_tenants", func(s *core.StepHandle) error {
		if err := fleet.Create(t, params.Tenants); err != nil {
			return err
		}
		journals = make([]*AckJournal, len(fleet.Tenants))
		for i := range journals {
			journals[i] = NewAckJournal()
		}
		s.Set("tenants", len(fleet.Tenants))
		s.Set("backing_pods", len(ips))
		return nil
	}); err != nil {
		return err
	}

	if topo != nil {
		var shaped *netshape.Fleet
		if err := t.Step("shape_"+topo.Name, func(s *core.StepHandle) error {
			var err error
			shaped, err = shapeBackingAndEndpoints(t, s, bkube, ampKube, fleet.Tenants, ips, *topo)
			return err
		}); err != nil {
			if shaped != nil {
				closeShaping(t.Context, shaped)
			}
			return err
		}
		defer closeShaping(t.Context, shaped)
	}

	if err := t.Step("seed_writes", func(s *core.StepHandle) error {
		st := partitionWriters(t.Context, env, fleet.Tenants, journals, params.BaselineDuration, params.StallLimit, params.PayloadBytes)
		s.Set("acked", st.acked)
		if st.hung > 0 {
			return fmt.Errorf("%d INSERT(s) exceeded %s on a healthy backing before any partition", st.hung, params.StallLimit)
		}
		if st.acked == 0 {
			return fmt.Errorf("no tenant was acknowledged for a single write in %s before the partitions, so there is no promise to hold the product to (first error: %v)",
				params.BaselineDuration, st.firstErr.Load())
		}
		return nil
	}); err != nil {
		return err
	}

	// The seed picks the order the nodes are cut in; each round takes the next.
	pods := make([]string, 0, len(ips))
	for pod := range ips {
		pods = append(pods, pod)
	}
	sort.Strings(pods)
	order := t.Seed.Fork("backing-partition").Rand().Perm(len(pods))

	var part *podPartition
	defer func() {
		if err := part.Heal(t.Context); err != nil {
			logf("backing_node_partition: healing the partition on exit: %v", err)
		}
	}()

	for r := 1; r <= params.Rounds; r++ {
		victim := pods[order[(r-1)%len(pods)]]
		var peer, peerIP string
		var peerAddr func() string
		for _, pod := range pods {
			if pod != victim {
				peer, peerIP = pod, ips[pod]
				break
			}
		}
		probe := newControlPlaneProbe(fleet.cl, fleet.poolID)

		var gate *settleGate
		if err := t.Step(fmt.Sprintf("partition_node_%d", r), func(s *core.StepHandle) error {
			s.Set("pod", victim)
			s.Set("peer", peer)
			gate, err = awaitSteadyState(t, s, env, fleet.Tenants, journals, params.PayloadBytes, 0)
			if err != nil {
				return err
			}
			// Quorum holds, so any pod but the victim going down is the product
			// failing to ride out one tserver (meko#1767), not env damage to retry.
			gate.asProductFailure(t.Options, partitionRestartIsFailKey)
			ex := bkube.ForPod(victim, "")
			peerAddr = podIPNow(t.Context, bkube, peer, peerIP)
			open, err := tcpOpen(t.Context, ex, peerAddr(), tserverRPCPort)
			if err != nil {
				return err
			}
			if !open {
				return fmt.Errorf("pod %s cannot reach its peer %s at %s:%d before any partition is applied, so the probe would prove nothing", victim, peer, peerAddr(), tserverRPCPort)
			}
			part, err = isolatePod(t, bkube, victim, fmt.Sprintf("%s-%d", t.ResourceSuffix(), r))
			if err != nil {
				return err
			}
			took, err := awaitLink(t, ex, victim, peerAddr, tserverRPCPort, false, partitionEnforceTimeout)
			if err != nil {
				return fmt.Errorf("the DROP rules in %s's network namespace never bit: no partition was injected: %w", victim, err)
			}
			s.Set("enforced_after", took.Round(time.Second).String())
			probe.Start(t.Context)
			faults.Open()
			return nil
		}); err != nil {
			return err
		}

		var partitionEnd time.Time
		if err := t.Step(fmt.Sprintf("writes_under_partition_%d", r), func(s *core.StepHandle) error {
			st := partitionWriters(t.Context, env, fleet.Tenants, journals, params.HoldDuration, params.StallLimit, params.PayloadBytes)
			partitionEnd = time.Now()
			s.Set("acked", st.acked)
			s.Set("errored", st.errored)
			s.Set("hung", st.hung)
			s.Set("longest_write_gap", st.LongestGap.Round(time.Second).String())
			s.Set("writes_resumed_within_hold", st.Resumed)
			if first := st.firstErr.Load(); first != nil {
				s.Set("first_error", first)
			}
			t.Metric(fmt.Sprintf("acked_under_partition_%d", r), float64(st.acked), "rows")
			t.Metric(fmt.Sprintf("longest_write_gap_seconds_%d", r), st.LongestGap.Seconds(), "s")
			if st.LongestGap > params.StallLimit {
				outcome := fmt.Sprintf("never resumed within the %s hold", params.HoldDuration)
				if st.Resumed {
					outcome = "then resumed"
				}
				return fmt.Errorf("writes through amp stalled for %s (%s) while backing pod %s was partitioned and %d peer(s) held a quorum, over the %s allowed: amp's storage daemons must fail over to the surviving leaders, not sit in RPC timeouts on the unreachable one (cf. meko#727); %d hung INSERT(s), %d error(s), %d acked",
					st.LongestGap.Round(time.Second), outcome, victim, len(ips)-1, params.StallLimit, st.hung, st.errored, st.acked)
			}
			return nil
		}); err != nil {
			probe.Abort()
			return err
		}

		if err := t.Step(fmt.Sprintf("heal_node_%d", r), func(s *core.StepHandle) error {
			if err := part.Heal(t.Context); err != nil {
				return fmt.Errorf("lift the partition of %s: %w", victim, err)
			}
			took, err := awaitLink(t, bkube.ForPod(victim, ""), victim, peerAddr, tserverRPCPort, true, params.SettleTimeout)
			if err != nil {
				return fmt.Errorf("pod %s still cannot reach peer %s at %s:%d after its DROP rules were deleted: %w", victim, peer, peerAddr(), tserverRPCPort, err)
			}
			s.Set("reachable_after", took.Round(time.Second).String())
			faults.Close()
			cpRecovery, measured := probe.Stop(partitionEnd, params.SettleTimeout)
			tally, bad := probe.Summary()
			s.Set("control_plane_probe", tally)
			s.Set("control_plane_recovery", cpRecovery.Round(time.Second).String())
			if bad > 0 {
				return fmt.Errorf("the controller answered %d non-503 5xx response(s) while backing pod %s was partitioned (%s); an unreachable store is a retryable 503, never a 500 (meko#886)", bad, victim, tally)
			}
			if !measured {
				return fmt.Errorf("the controller never answered a probe 2xx in the %s after the partition of %s was lifted (meko#961)", params.SettleTimeout, victim)
			}
			if cpRecovery > params.ControlPlaneRecoveryMax {
				return fmt.Errorf("the controller kept answering errors for %s after the partition of %s was lifted, over the %s allowed (meko#961)",
					cpRecovery.Round(time.Second), victim, params.ControlPlaneRecoveryMax)
			}
			return gate.attestNonTarget(t, s, victim)
		}); err != nil {
			return err
		}

		if err := t.Step(fmt.Sprintf("backing_recovered_%d", r), func(s *core.StepHandle) error {
			waited, err := awaitBackingReady(t, bkube, params.SettleTimeout)
			if err != nil {
				return err
			}
			s.Set("ready_after", waited.Round(time.Second).String())
			return nil
		}); err != nil {
			return err
		}

		if err := t.Step(fmt.Sprintf("endpoints_serve_%d", r), func(s *core.StepHandle) error {
			if err := assertEndpointsServe(t, env, fleet.Tenants, params.SettleTimeout); err != nil {
				return err
			}
			s.Set("endpoints_recovered", len(fleet.Tenants))
			return nil
		}); err != nil {
			return err
		}

		if err := t.Step(fmt.Sprintf("verify_acked_set_%d", r), func(s *core.StepHandle) error {
			return verifyExactSets(t, s, env, fleet.Tenants, journals, params.PayloadBytes)
		}); err != nil {
			return err
		}
	}

	if err := t.Step("writes_flow_again", func(s *core.StepHandle) error {
		st := partitionWriters(t.Context, env, fleet.Tenants, journals, params.ResumeDuration, params.StallLimit, params.PayloadBytes)
		s.Set("acked_after_recovery", st.acked)
		if st.hung > 0 {
			return fmt.Errorf("%d INSERT(s) exceeded %s after every partition was lifted and the backing reported ready", st.hung, params.StallLimit)
		}
		if st.acked == 0 {
			return fmt.Errorf("no tenant was acknowledged for a single write in %s after the partitions were lifted and every endpoint answered SQL: the endpoints serve reads but no COMMIT completes (cf. meko#727); first error: %v",
				params.ResumeDuration, st.firstErr.Load())
		}
		return verifyExactSets(t, s, env, fleet.Tenants, journals, params.PayloadBytes)
	}); err != nil {
		return err
	}

	if err := t.Step("control_plane_recovered", func(s *core.StepHandle) error {
		before := len(fleet.Tenants)
		if err := fleet.Create(t, 1); err != nil {
			return fmt.Errorf("the control plane cannot create a tenant after the partitions: %w", err)
		}
		if len(fleet.Tenants) <= before {
			return fmt.Errorf("creating a tenant after the partitions reported success but added none")
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("physical_consistency", func(s *core.StepHandle) error {
		return verifyPhysicalConsistency(t, s, env, fleet.Tenants)
	}); err != nil {
		return err
	}

	return faults.Assert(t)
}

// shapeBackingAndEndpoints places the backing pods (site = name order) and
// every tenant's endpoint pod (the topology's endpoint site) the topology's
// round trips apart, and measures each separated pair from inside the pods. The
// returned fleet is non-nil once any helper exists, so a caller can close it.
func shapeBackingAndEndpoints(t *core.T, s *core.StepHandle, bkube, ampKube *k8s.Kubectl, tenants []tenant, ips map[string]string, tp netshape.Topology) (*netshape.Fleet, error) {
	c := t.Options
	f := &netshape.Fleet{
		Device:    c.GetString("stress.partition.device", "eth0"),
		Pings:     c.GetInt("stress.partition.shape_verify_pings", 10),
		Tolerance: c.GetFloat("stress.partition.shape_tolerance", 0.3),
	}
	pods := make([]string, 0, len(ips))
	for pod := range ips {
		pods = append(pods, pod)
	}
	sort.Strings(pods)
	if tp.MaxSite() > len(pods) {
		return nil, fmt.Errorf("topology %q names site %d, but the backing has %d pod(s): sites 0..%d, plus %d for the endpoints", tp.Name, tp.MaxSite(), len(pods), len(pods)-1, len(pods))
	}
	type target struct {
		kube *k8s.Kubectl
		m    *netshape.Member
	}
	var targets []target
	for i, pod := range pods {
		targets = append(targets, target{bkube, &netshape.Member{Label: pod, Pod: pod, IP: ips[pod], Site: i}})
	}
	for i := range tenants {
		ten := &tenants[i]
		pod, err := endpointPodFor(t, ampKube, ten.Endpoint)
		if err != nil {
			return nil, err
		}
		ip, err := ampKube.Get(t.Context, "pod", pod, "{.status.podIP}")
		if err != nil || ip == "" {
			return nil, fmt.Errorf("read the IP of endpoint pod %s: %v", pod, err)
		}
		targets = append(targets, target{ampKube, &netshape.Member{Label: "endpoint-" + ten.Name, Pod: pod, IP: ip, Site: tp.EndpointSite}})
	}
	image := helperImage(c)
	for i, tg := range targets {
		f.Members = append(f.Members, tg.m)
		h, err := netshape.Attach(t.Context, tg.kube, tg.m.Pod, fmt.Sprintf("%s-s%d", t.ResourceSuffix(), i), image)
		if err != nil {
			return f, core.EnvFail("attach a netshape helper to %s: %w", tg.m.Pod, err)
		}
		tg.m.Helper = h
		s.Set(fmt.Sprintf("site_%d", tg.m.Site)+"_"+tg.m.Label, tg.m.IP)
	}
	native, err := f.Baseline(t.Context)
	if err != nil {
		return f, core.EnvFail("%w", err)
	}
	for _, m := range native {
		s.Set(fmt.Sprintf("native_rtt_%s_%s", m.From, m.To), m.Got.Round(10*time.Microsecond).String())
	}
	if err := f.Apply(t.Context, tp); err != nil {
		return f, core.EnvFail("%w", err)
	}
	ms, err := f.Verify(t.Context, tp)
	for _, m := range ms {
		s.Set(fmt.Sprintf("rtt_%s_%s", m.From, m.To), fmt.Sprintf("%s (want %s)", m.Got.Round(10*time.Microsecond), m.Want))
	}
	if err != nil {
		return f, core.EnvFail("the injected latency did not take, so nothing below would run under %q: %w", tp.Name, err)
	}
	return f, nil
}

// closeShaping is Fleet.Close on a context that survives the run's cancellation.
func closeShaping(ctx context.Context, f *netshape.Fleet) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	f.Close(ctx)
}
