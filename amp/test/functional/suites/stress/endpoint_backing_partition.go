// The compute up and reachable, its storage gone quiet. stress/storage_proc_nemesis
// kills the daemons inside the endpoint pod and stress/wal_tail_nemesis takes a
// backing pod away, so the daemons see a dead peer at once. A partition between
// the endpoint and the backing leaves every connection open and every RPC
// waiting: the shape a real network fault has.
//
// The product's answer to it is the write lease (endpoint_supervisor/src/fence.rs):
// the supervisor renews a token in the store every few seconds, and once the
// token lapses it stops every service and waits for the store to hand the lease
// back. So a cut endpoint must stop acknowledging commits within the lease's
// TTL, must never have acknowledged one that storage does not hold, must leave
// its neighbours committing, and must come back on its own once the link does.
package stress

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/endpoint_backing_partition",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Cut one tenant's endpoint pod off the backing YugabyteDB (iptables in its own network namespace dropping its egress to the store, the pod up and reachable) while journaled writes run, and hold the endpoint to its write lease: no commit acknowledged past the lease's TTL, every acknowledged id in storage once the link is back, the other tenants committing throughout, and the cut endpoint serving and committing again on its own after the heal.",
		Params:      []string{"stress.endpoint_backing_partition", "stress.consistency"},
		Timeout:     90 * time.Minute,
		Run:         runEndpointBackingPartition,
	})
}

type endpointBackingPartitionParams struct {
	Tenants          int
	Rounds           int
	BaselineDuration time.Duration
	// HoldDuration is how long the link stays cut while writers run against it.
	HoldDuration time.Duration
	// StallLimit bounds one INSERT on an UNCUT endpoint; on the cut one a hang
	// is the expected outcome and only journaled.
	StallLimit time.Duration
	// FenceWithin is how long after the cut the endpoint may still acknowledge
	// a commit: the write lease's TTL plus the renewal that was in flight.
	FenceWithin    time.Duration
	SettleTimeout  time.Duration
	ResumeDuration time.Duration
	PayloadBytes   int
}

func endpointBackingPartitionParamsFromConfig(c *core.Config) endpointBackingPartitionParams {
	return endpointBackingPartitionParams{
		Tenants:          c.GetInt("stress.endpoint_backing_partition.tenants", 2),
		Rounds:           c.GetInt("stress.endpoint_backing_partition.rounds", 2),
		BaselineDuration: c.GetDuration("stress.endpoint_backing_partition.baseline_duration", 20*time.Second),
		HoldDuration:     c.GetDuration("stress.endpoint_backing_partition.hold_duration", 90*time.Second),
		StallLimit:       c.GetDuration("stress.endpoint_backing_partition.stall_limit", 60*time.Second),
		FenceWithin:      c.GetDuration("stress.endpoint_backing_partition.fence_within", 45*time.Second),
		SettleTimeout:    c.GetDuration("stress.endpoint_backing_partition.settle_timeout", 10*time.Minute),
		ResumeDuration:   c.GetDuration("stress.endpoint_backing_partition.resume_duration", 30*time.Second),
		PayloadBytes:     c.GetInt("stress.endpoint_backing_partition.payload_bytes", 256),
	}
}

func runEndpointBackingPartition(t *core.T) error {
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	ampEnv, ok := t.Env.(ampKubeEnv)
	if !ok || ampEnv.AmpKube() == nil {
		t.Skip("requires a kube handle for the control-plane namespace: the endpoint pod is what the partition selects")
		return nil
	}
	backingEnv, ok := t.Env.(backingKubeEnv)
	if !ok || backingEnv.BackingKube() == nil {
		t.Skip("requires a kube handle for the backing's namespace: the cut is defined by where the backing lives")
		return nil
	}
	params := endpointBackingPartitionParamsFromConfig(t.Options)
	if params.Rounds < 1 {
		return fmt.Errorf("stress.endpoint_backing_partition.rounds must be at least 1, got %d: no fault would be injected", params.Rounds)
	}
	logf := t.Masker().Logf(log.Printf)
	kube := ampEnv.AmpKube()
	bkube := backingEnv.BackingKube()

	ips, err := backingPodIPs(t, bkube)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return fmt.Errorf("no backing pod matched any known selector in namespace %q", bkube.Namespace())
	}
	var anyBackingIP string
	for _, ip := range ips {
		anyBackingIP = ip
		break
	}

	fleet, err := newTenantFleet(t, env, logf)
	if err != nil {
		return err
	}
	defer fleet.Close()

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
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("seed_writes", func(s *core.StepHandle) error {
		st := partitionWriters(t.Context, env, fleet.Tenants, journals, params.BaselineDuration, params.StallLimit, params.PayloadBytes)
		s.Set("acked", st.acked)
		if st.hung > 0 {
			return fmt.Errorf("%d INSERT(s) exceeded %s before any link was cut", st.hung, params.StallLimit)
		}
		if st.acked == 0 {
			return fmt.Errorf("no tenant was acknowledged for a single write in %s before the cuts, so there is no promise to hold the product to (first error: %v)",
				params.BaselineDuration, st.firstErr.Load())
		}
		return nil
	}); err != nil {
		return err
	}

	order := t.Seed.Fork("endpoint-partition").Rand().Perm(len(fleet.Tenants))

	var part *podPartition
	defer func() {
		if err := part.Heal(t.Context); err != nil {
			logf("endpoint_backing_partition: healing the partition on exit: %v", err)
		}
	}()

	for r := 1; r <= params.Rounds; r++ {
		k := order[(r-1)%len(fleet.Tenants)]
		ten := &fleet.Tenants[k]
		var pod, podUID, restartsBefore string
		var ex *k8s.Kubectl

		if err := t.Step(fmt.Sprintf("cut_endpoint_%d", r), func(s *core.StepHandle) error {
			pod, err = endpointPodFor(t, kube, ten.Endpoint)
			if err != nil {
				return err
			}
			ex = kube.ForPod(pod, "")
			s.Set("tenant", ten.Name)
			s.Set("pod", pod)
			podUID, restartsBefore, err = podIdentity(t.Context, kube, pod)
			if err != nil {
				return err
			}
			open, err := tcpOpen(t.Context, ex, anyBackingIP, ysqlPort)
			if err != nil {
				return err
			}
			if !open {
				return fmt.Errorf("endpoint pod %s cannot reach the backing at %s:%d before any cut, so the probe would prove nothing", pod, anyBackingIP, ysqlPort)
			}
			part, err = cutPodFromBacking(t, kube, pod, fmt.Sprintf("%s-%d", t.ResourceSuffix(), r), bkube)
			if err != nil {
				return err
			}
			took, err := awaitLink(t, ex, pod, anyBackingIP, ysqlPort, false, partitionEnforceTimeout)
			if err != nil {
				return fmt.Errorf("the DROP rules cutting %s off the backing never bit: no partition was injected: %w", pod, err)
			}
			s.Set("enforced_after", took.Round(time.Second).String())
			return nil
		}); err != nil {
			return err
		}
		cutAt := time.Now()

		if err := t.Step(fmt.Sprintf("writes_under_cut_%d", r), func(s *core.StepHandle) error {
			// One writer group per tenant, each a view onto the fleet's own
			// storage, so the cut tenant's numbers stay separable from its
			// neighbours'.
			results := make([]*partitionWriteStats, len(fleet.Tenants))
			var wg sync.WaitGroup
			for i := range fleet.Tenants {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					results[i] = partitionWriters(t.Context, env, fleet.Tenants[i:i+1], journals[i:i+1], params.HoldDuration, params.StallLimit, params.PayloadBytes)
				}(i)
			}
			wg.Wait()

			cut := results[k]
			var othersAcked int64
			var othersGap time.Duration
			for i, st := range results {
				if i != k {
					othersAcked += st.acked
					if st.LongestGap > othersGap {
						othersGap = st.LongestGap
					}
				}
			}
			s.Set("cut_acked", cut.acked)
			s.Set("cut_errored", cut.errored)
			s.Set("cut_hung", cut.hung)
			if first := cut.firstErr.Load(); first != nil {
				s.Set("cut_first_error", first)
			}
			lastAck := "none"
			if cut.LastAckOffset >= 0 {
				lastAck = cut.LastAckOffset.Round(time.Second).String()
			}
			s.Set("cut_last_ack_after_cut", lastAck)
			s.Set("others_acked", othersAcked)
			s.Set("others_longest_gap", othersGap.Round(time.Second).String())
			t.Metric(fmt.Sprintf("acked_on_cut_endpoint_%d", r), float64(cut.acked), "rows")

			if cut.LastAckOffset > params.FenceWithin {
				return fmt.Errorf("endpoint %s acknowledged a commit %s after its store became unreachable, past the %s the write lease allows (fence.rs: TTL 15s, renewed every 5s): the fence did not stop the writer",
					pod, cut.LastAckOffset.Round(time.Second), params.FenceWithin)
			}
			if len(fleet.Tenants) > 1 {
				if othersGap > params.StallLimit {
					return fmt.Errorf("writes on an endpoint whose link to the backing was never cut stalled for %s (limit %s) while only %s's was",
						othersGap.Round(time.Second), params.StallLimit, pod)
				}
				if othersAcked == 0 {
					return fmt.Errorf("no uncut tenant was acknowledged for a single write in the %s %s was cut off the backing: one tenant's partition stopped its neighbours",
						params.HoldDuration, pod)
				}
			}
			return nil
		}); err != nil {
			return err
		}

		if err := t.Step(fmt.Sprintf("heal_endpoint_%d", r), func(s *core.StepHandle) error {
			if err := part.Heal(t.Context); err != nil {
				return fmt.Errorf("lift the cut on %s: %w", pod, err)
			}
			took, err := awaitLink(t, ex, pod, anyBackingIP, ysqlPort, true, params.SettleTimeout)
			if err != nil {
				return fmt.Errorf("endpoint pod %s still cannot reach %s:%d after its DROP rules were deleted: %w", pod, anyBackingIP, ysqlPort, err)
			}
			s.Set("reachable_after", took.Round(time.Second).String())
			s.Set("cut_lasted", time.Since(cutAt).Round(time.Second).String())
			return nil
		}); err != nil {
			return err
		}

		// A lapsed lease stops every service in place (supervise.rs poll_once,
		// Outcome::FenceLost) and the supervisor waits for the store to hand the
		// lease back; nothing restarts the container for it.
		if err := t.Step(fmt.Sprintf("endpoint_back_%d", r), func(s *core.StepHandle) error {
			start := time.Now()
			if err := assertEndpointsServe(t, env, fleet.Tenants[k:k+1], params.SettleTimeout); err != nil {
				attachEndpointDiagnostics(t, kube, ten, fmt.Sprintf("endpoint_back_%d", r), logf)
				return fmt.Errorf("the cut endpoint does not serve SQL within %s of its link to the backing coming back: %w", params.SettleTimeout, err)
			}
			s.Set("serving_after", time.Since(start).Round(time.Second).String())
			t.Metric(fmt.Sprintf("endpoint_back_seconds_%d", r), time.Since(start).Seconds(), "s")
			now, err := endpointPodFor(t, kube, ten.Endpoint)
			if err != nil {
				return err
			}
			uid, restarts, err := podIdentity(t.Context, kube, now)
			if err != nil {
				return err
			}
			s.Set("pod_replaced", now != pod || uid != podUID)
			s.Set("container_restarts", fmt.Sprintf("%s -> %s", restartsBefore, restarts))
			pids, err := daemonPIDs(t.Context, kube.ForPod(now, ""), "safekeeper")
			if err != nil {
				return err
			}
			if len(pids) == 0 {
				return fmt.Errorf("endpoint pod %s answers SQL but runs no safekeeper: the supervisor brought postgres back without its WAL service", now)
			}
			pod = now
			return nil
		}); err != nil {
			return err
		}

		// The oracle: an id the client was told committed before the fence fell
		// has to be in storage now.
		if err := t.Step(fmt.Sprintf("verify_acked_set_%d", r), func(s *core.StepHandle) error {
			if err := verifyExactSets(t, s, env, fleet.Tenants, journals, params.PayloadBytes); err != nil {
				attachEndpointDiagnostics(t, kube, ten, fmt.Sprintf("verify_acked_set_%d", r), logf)
				return err
			}
			return nil
		}); err != nil {
			return err
		}

		if err := t.Step(fmt.Sprintf("writes_flow_%d", r), func(s *core.StepHandle) error {
			waited, err := awaitWritePath(t.Context, env, fleet.Tenants[k:k+1], journals, params.SettleTimeout, params.PayloadBytes)
			if err != nil {
				attachEndpointDiagnostics(t, kube, ten, fmt.Sprintf("writes_flow_%d", r), logf)
				return fmt.Errorf("the cut endpoint's write path never came back once the link was (cf. meko#727): %w", err)
			}
			s.Set("write_path_back_after", waited.Round(time.Second).String())
			s.Set("write_errors", atomic.LoadInt64(&ten.WriteErrors))
			s.Set("reconnects", atomic.LoadInt64(&ten.Reconnects))
			return nil
		}); err != nil {
			return err
		}
	}

	if err := t.Step("writes_flow_again", func(s *core.StepHandle) error {
		st := partitionWriters(t.Context, env, fleet.Tenants, journals, params.ResumeDuration, params.StallLimit, params.PayloadBytes)
		s.Set("acked_after_recovery", st.acked)
		if st.hung > 0 {
			return fmt.Errorf("%d INSERT(s) exceeded %s after every link was back and every endpoint answered SQL", st.hung, params.StallLimit)
		}
		if st.acked == 0 {
			return fmt.Errorf("no tenant was acknowledged for a single write in %s after the links were back: the endpoints serve reads but no COMMIT completes (cf. meko#727); first error: %v",
				params.ResumeDuration, st.firstErr.Load())
		}
		return verifyExactSets(t, s, env, fleet.Tenants, journals, params.PayloadBytes)
	}); err != nil {
		return err
	}

	return t.Step("physical_consistency", func(s *core.StepHandle) error {
		return verifyPhysicalConsistency(t, s, env, fleet.Tenants)
	})
}
