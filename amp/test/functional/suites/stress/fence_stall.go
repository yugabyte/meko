// Only fence_stall_supervisor asserts the fence FIRED. A frozen pod writes nothing its
// expired token is checked against. The fence deadline is asserted by both variants.
package stress

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/chaos"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	fenceStallDescription = "Stall the writer of a branch past its write lease (%s) while journaled writes run, attempt a second writer on the same branch while it is stalled, then resume it: no commit acknowledged more than stress.fence_stall.fence_deadline after the stall began, the endpoint serving and committing again on its own afterwards, and every acknowledged id present in storage read over a fresh connection."

	secondWriterTable = "fence_b"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/fence_stall",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamCompute,
		Description: fmt.Sprintf(fenceStallDescription, "the whole endpoint pod frozen through its cgroup v2 cgroup.freeze, postgres included"),
		Params:      []string{"stress.fence_stall", "chaos"},
		Timeout:     90 * time.Minute,
		Run:         func(t *core.T) error { return runFenceStall(t, chaos.ActionPodFreeze) },
	})
	core.Register(core.Test{
		Name:        "stress/fence_stall_supervisor",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamCompute,
		Description: fmt.Sprintf(fenceStallDescription, "the endpoint supervisor alone SIGSTOPped, postgres and the storage daemons left running"),
		Params:      []string{"stress.fence_stall", "chaos"},
		Timeout:     90 * time.Minute,
		Run:         func(t *core.T) error { return runFenceStall(t, chaos.ActionProcessStall) },
	})
}

type fenceStallParams struct {
	LeaseTTL   time.Duration
	SkewGrace  time.Duration
	Stall      time.Duration
	FenceAfter time.Duration

	BaselineDuration time.Duration
	ResumeDuration   time.Duration
	StallLimit       time.Duration
	SettleTimeout    time.Duration
	PayloadBytes     int
}

func fenceStallParamsFromConfig(c *core.Config) fenceStallParams {
	return fenceStallParams{
		LeaseTTL:         c.GetDuration("stress.fence_stall.lease_ttl", 15*time.Second),
		SkewGrace:        c.GetDuration("stress.fence_stall.skew_grace", 500*time.Millisecond),
		Stall:            c.GetDuration("stress.fence_stall.freeze", 90*time.Second),
		FenceAfter:       c.GetDuration("stress.fence_stall.fence_deadline", 60*time.Second),
		BaselineDuration: c.GetDuration("stress.fence_stall.baseline_duration", 20*time.Second),
		ResumeDuration:   c.GetDuration("stress.fence_stall.resume_duration", 30*time.Second),
		StallLimit:       c.GetDuration("stress.fence_stall.stall_limit", 60*time.Second),
		SettleTimeout:    c.GetDuration("stress.fence_stall.settle_timeout", 10*time.Minute),
		PayloadBytes:     c.GetInt("stress.fence_stall.payload_bytes", 256),
	}
}

func runFenceStall(t *core.T, action string) error {
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	ampEnv, ok := t.Env.(ampKubeEnv)
	if !ok || ampEnv.AmpKube() == nil {
		t.Skip("requires a kube handle for the control-plane namespace: the endpoint pod is what the stall selects")
		return nil
	}
	params := fenceStallParamsFromConfig(t.Options)
	if params.Stall <= params.LeaseTTL+params.SkewGrace {
		return fmt.Errorf("stress.fence_stall.freeze is %s, at or under the lease's %s TTL plus %s of skew grace: the lease would never lapse and the stall would prove nothing",
			params.Stall, params.LeaseTTL, params.SkewGrace)
	}
	if params.Stall <= params.FenceAfter {
		return fmt.Errorf("stress.fence_stall.freeze is %s, at or under the %s fence_deadline: the write window would close before a late acknowledgement could be seen and the deadline check would pass on every run",
			params.Stall, params.FenceAfter)
	}
	logf := t.Masker().Logf(log.Printf)
	kube := ampEnv.AmpKube()

	fleet, err := newTenantFleet(t, env, logf)
	if err != nil {
		return err
	}
	defer fleet.Close()

	journals := []*AckJournal{NewAckJournal()}
	if err := t.Step("create_tenant", func(s *core.StepHandle) error {
		if err := fleet.Create(t, 1); err != nil {
			return err
		}
		s.Set("cluster", fleet.Tenants[0].ClusterID)
		s.Set("branch", fleet.Tenants[0].BranchID)
		return nil
	}); err != nil {
		return err
	}
	ten := &fleet.Tenants[0]

	if err := t.Step("baseline_writes", func(s *core.StepHandle) error {
		st := partitionWriters(t.Context, env, fleet.Tenants, journals, params.BaselineDuration, params.StallLimit, params.PayloadBytes)
		s.Set("acked", st.acked)
		if st.acked == 0 {
			return fmt.Errorf("the writer was acknowledged for no commit in %s before the stall, so there is no promise to hold the product to (first error: %v)",
				params.BaselineDuration, st.firstErr.Load())
		}
		return nil
	}); err != nil {
		return err
	}

	var pod, podUID, restartsBefore string
	var sksBefore []int
	if err := t.Step("read_endpoint", func(s *core.StepHandle) error {
		pod, err = endpointPodFor(t, kube, ten.Endpoint)
		if err != nil {
			return err
		}
		podUID, restartsBefore, err = podIdentity(t.Context, kube, pod)
		if err != nil {
			return err
		}
		sksBefore, err = daemonPIDs(t.Context, kube.ForPod(pod, ""), "safekeeper")
		if err != nil {
			return err
		}
		if len(sksBefore) == 0 {
			return fmt.Errorf("endpoint pod %s runs no safekeeper before the stall, so nothing would be fenced", pod)
		}
		s.Set("pod", pod)
		s.Set("safekeeper_pids", joinPIDs(sksBefore, ","))
		return nil
	}); err != nil {
		return err
	}

	act, err := chaos.DefaultRegistry().Build(action, chaos.FromConfig(t.Options))
	if err != nil {
		return err
	}
	target := chaos.NewPodTarget(pod, kube, pod, "")
	// Armed before Inject, not after: a partial injection still needs the Recover.
	stalled := true
	defer func() {
		if !stalled {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context), 5*time.Minute)
		defer cancel()
		if err := act.Recover(ctx, target); err != nil {
			logf("fence_stall: lifting %s on %s at teardown: %v", action, pod, err)
		}
	}()

	var stallAt time.Time
	if err := t.Step("stall_writer", func(s *core.StepHandle) error {
		stallAt = time.Now()
		if err := act.Inject(t.Context, target); err != nil {
			return err
		}
		held, err := stallHeld(t, kube, pod, action, t.Options.GetString("chaos.stall.pattern", "endpoint-supervisor"))
		if err != nil {
			return err
		}
		s.Set("evidence", held)
		return nil
	}); err != nil {
		return err
	}

	var second *tenant
	defer func() {
		if second != nil && second.Conn != nil {
			second.Conn.Close()
		}
	}()
	secondJournal := NewAckJournal()
	var st *partitionWriteStats
	if err := t.Step("writes_under_stall", func(s *core.StepHandle) error {
		createCtx, cancelCreate := context.WithTimeout(t.Context, params.SettleTimeout)
		defer cancelCreate()
		var wg sync.WaitGroup
		var createErr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			cl, err := env.AmpController(createCtx)
			if err != nil {
				createErr = err
				return
			}
			_, createErr = cl.CreateEndpointAndWait(createCtx, env.AmpPoolID(), ten.ClusterID, ten.BranchID,
				controller.CreateEndpointRequest{Role: tenantRole, Resources: fleet.endpointResources()})
		}()
		windowStart := time.Now()
		st = partitionWriters(t.Context, env, fleet.Tenants, journals, params.Stall, params.StallLimit, params.PayloadBytes)
		wg.Wait()

		s.Set("acked_while_stalled", st.acked)
		s.Set("hung_while_stalled", st.hung)
		s.Set("second_writer_accepted", createErr == nil)
		if createErr != nil {
			s.Set("second_writer_refusal", createErr.Error())
		}
		if st.acked == 0 && st.hung == 0 {
			return fmt.Errorf("no write reached the stalled endpoint in %s, neither acknowledged nor hung, so the window proves nothing about the fence (first error: %v)",
				params.Stall, st.firstErr.Load())
		}
		// LastAckOffset is an offset into the write window, while the deadline runs from the stall.
		if st.LastAckOffset >= 0 {
			sinceStall := windowStart.Add(st.LastAckOffset).Sub(stallAt)
			s.Set("last_ack_after_stall", sinceStall.Round(time.Second).String())
			if sinceStall > params.FenceAfter {
				return fmt.Errorf("endpoint %s acknowledged a commit %s after it was stalled (%s), past the %s the write lease allows (fencing doc: a %s TTL plus %s of skew grace): the fence did not stop the writer",
					pod, sinceStall.Round(time.Second), action, params.FenceAfter, params.LeaseTTL, params.SkewGrace)
			}
		}
		if createErr == nil {
			second, err = attachSecondWriter(t, env, ten, params.SettleTimeout)
			if err != nil {
				return err
			}
			sst := partitionWriters(t.Context, env, []tenant{*second}, []*AckJournal{secondJournal},
				params.ResumeDuration, params.StallLimit, params.PayloadBytes)
			s.Set("acked_by_second_writer", sst.acked)
		}
		return nil
	}); err != nil {
		return err
	}

	var thawAt time.Time
	if err := t.Step("resume_writer", func(s *core.StepHandle) error {
		if err := act.Recover(t.Context, target); err != nil {
			return err
		}
		stalled = false
		thawAt = time.Now()
		s.Set("stall_lasted", thawAt.Sub(stallAt).Round(time.Second).String())
		return nil
	}); err != nil {
		return err
	}

	if action == chaos.ActionProcessStall {
		if err := t.Step("fence_fired", func(s *core.StepHandle) error {
			after, err := awaitSafekeeperReplaced(t, kube, ten, pod, sksBefore, params.FenceAfter)
			s.Set("safekeeper_pids_after", after)
			if err != nil {
				attachEndpointDiagnostics(t, kube, ten, "fence_fired", logf)
				return err
			}
			uid, restarts, err := podIdentity(t.Context, kube, pod)
			if err != nil {
				return err
			}
			s.Set("pod_replaced", uid != podUID)
			s.Set("container_restarts", fmt.Sprintf("%s -> %s", restartsBefore, restarts))
			return nil
		}); err != nil {
			return err
		}
	}

	if second != nil {
		return t.Step("two_writers_on_one_branch", func(s *core.StepHandle) error {
			post := partitionWriters(t.Context, env, fleet.Tenants, journals, params.ResumeDuration, params.StallLimit, params.PayloadBytes)
			s.Set("acked_after_resume", post.acked)
			if post.acked > 0 {
				return fmt.Errorf("endpoint %s was acknowledged for %d commit(s) after it resumed, while endpoint %s was admitted on the same branch and is writing to it: two writers committed to branch %s",
					pod, post.acked, second.Endpoint.ID, ten.BranchID)
			}
			if err := verifySecondWriter(t, s, env, second, secondJournal, params.PayloadBytes); err != nil {
				return err
			}
			return verifyExactSets(t, s, env, fleet.Tenants, journals, params.PayloadBytes)
		})
	}

	if err := t.Step("endpoint_back", func(s *core.StepHandle) error {
		start := time.Now()
		if err := assertEndpointsServe(t, env, fleet.Tenants, params.SettleTimeout); err != nil {
			attachEndpointDiagnostics(t, kube, ten, "endpoint_back", logf)
			return fmt.Errorf("the stalled endpoint does not serve SQL within %s of the stall being lifted: %w", params.SettleTimeout, err)
		}
		waited, err := awaitWritePath(t.Context, env, fleet.Tenants, journals, params.SettleTimeout, params.PayloadBytes)
		if err != nil {
			attachEndpointDiagnostics(t, kube, ten, "endpoint_back", logf)
			return fmt.Errorf("the stalled endpoint's write path never came back once it resumed: %w", err)
		}
		s.Set("serving_after", time.Since(start).Round(time.Second).String())
		s.Set("write_path_back_after", waited.Round(time.Second).String())
		t.Metric("write_path_back_seconds", waited.Seconds(), "s")
		return nil
	}); err != nil {
		return err
	}

	return t.Step("verify_acked_set", func(s *core.StepHandle) error {
		post := partitionWriters(t.Context, env, fleet.Tenants, journals, params.ResumeDuration, params.StallLimit, params.PayloadBytes)
		s.Set("acked_after_resume", post.acked)
		if post.acked == 0 {
			return fmt.Errorf("the endpoint serves SQL but was acknowledged for no commit in the %s after it resumed (first error: %v)",
				params.ResumeDuration, post.firstErr.Load())
		}
		return verifyExactSets(t, s, env, fleet.Tenants, journals, params.PayloadBytes)
	})
}

func stallHeld(t *core.T, kube *k8s.Kubectl, pod, action, pattern string) (string, error) {
	ex := kube.ForPod(pod, "")
	if action == chaos.ActionProcessStall {
		res, err := ex.Run(t.Context, stoppedStateScript(pattern))
		if err != nil {
			return "", fmt.Errorf("read the process state of %q in pod %s: %w", pattern, pod, err)
		}
		states := strings.Fields(res.Stdout)
		for _, st := range states {
			if st == "T" {
				return "process state T", nil
			}
		}
		return "", fmt.Errorf("no process matching %q in pod %s is in state T after the SIGSTOP (states %v): nothing was stalled", pattern, pod, states)
	}
	// A frozen cgroup freezes the exec before it runs, so a probe that ANSWERS is the failure.
	ctx, cancel := context.WithTimeout(t.Context, frozenProbeTimeout)
	defer cancel()
	if _, err := ex.Run(ctx, "echo alive"); err == nil {
		return "", fmt.Errorf("pod %s still answers an exec %s after its cgroup was frozen: the freeze did not take", pod, frozenProbeTimeout)
	}
	return fmt.Sprintf("no exec answer in %s", frozenProbeTimeout), nil
}

const frozenProbeTimeout = 20 * time.Second

func stoppedStateScript(pattern string) string {
	return fmt.Sprintf(`for d in /proc/[0-9]*; do case "$(readlink $d/exe 2>/dev/null)" in *%s) `+
		`sed -n "s/.*) \([A-Za-z]\).*/\1/p" $d/stat;; esac; done; exit 0`, pattern)
}

func awaitSafekeeperReplaced(t *core.T, kube *k8s.Kubectl, ten *tenant, pod string, before []int, within time.Duration) (string, error) {
	var last []int
	err := t.WaitFor(fmt.Sprintf("the safekeeper in pod %s to be replaced", pod), within, 3*time.Second, func() (bool, error) {
		now, err := endpointPodFor(t, kube, ten.Endpoint)
		if err != nil {
			return false, nil
		}
		if now != pod {
			last = nil
			return true, nil
		}
		pids, err := daemonPIDs(t.Context, kube.ForPod(pod, ""), "safekeeper")
		if err != nil {
			return false, nil
		}
		last = pids
		return len(intersectPIDs(before, pids)) == 0, nil
	})
	if err != nil {
		return joinPIDs(last, ","), fmt.Errorf("the safekeeper pid(s) %s that ran before the stall are still running %s after it was lifted: the lease lapsed over %s of stall and neither the supervisor nor the safekeeper's own deadline check stopped them (fencing doc, Enforcement): %w",
			joinPIDs(before, ","), within, within, err)
	}
	return joinPIDs(last, ","), nil
}

func attachSecondWriter(t *core.T, env ampTenantEnv, first *tenant, settle time.Duration) (*tenant, error) {
	cl, err := env.AmpController(t.Context)
	if err != nil {
		return nil, err
	}
	eps, err := cl.ListAllEndpoints(t.Context, env.AmpPoolID(), first.ClusterID, first.BranchID)
	if err != nil {
		return nil, err
	}
	var ep controller.Endpoint
	for _, e := range eps {
		if e.ID != first.Endpoint.ID {
			ep = e
		}
	}
	if ep.ID == "" {
		return nil, fmt.Errorf("the controller accepted a second writer on branch %s but the branch lists only endpoint %s", first.BranchID, first.Endpoint.ID)
	}
	second := &tenant{Index: first.Index + 1, Name: first.Name + "-b", ClusterID: first.ClusterID,
		BranchID: first.BranchID, Endpoint: ep, Table: secondWriterTable}
	deadline := time.Now().Add(settle)
	for {
		conn, err := env.ConnectEndpoint(t.Context, ep)
		if err == nil {
			_, err = conn.Exec(t.Context, fmt.Sprintf(
				"CREATE TABLE IF NOT EXISTS %s (id bigint PRIMARY KEY, v bigint NOT NULL, pad text)", second.Table))
			if err == nil {
				second.Conn = conn
				return second, nil
			}
			conn.Close()
		}
		if t.Context.Err() != nil {
			return nil, t.Context.Err()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the second writer %s admitted on branch %s never served SQL within %s: %w", ep.ID, first.BranchID, settle, err)
		}
		time.Sleep(5 * time.Second)
	}
}

func verifySecondWriter(t *core.T, s *core.StepHandle, env ampTenantEnv, second *tenant, j *AckJournal, payloadBytes int) error {
	present, badPad, err := readStoredRows(t, env, second, payloadBytes)
	if err != nil {
		return err
	}
	v := j.Verify(present)
	s.Set("second_writer_rows", v.Present)
	if len(v.Lost) > 0 {
		return fmt.Errorf("the second writer %s lost an ACKNOWLEDGED commit on branch %s: id %d is absent from %s read over a fresh connection. Journal: %s",
			second.Endpoint.ID, second.BranchID, v.Lost[0], second.Table, v.Summary())
	}
	if len(v.Mismatched) > 0 {
		m := v.Mismatched[0]
		return fmt.Errorf("%s holds id %d with v=%d, the second writer sent v=%d", second.Table, m.ID, m.Got, m.Want)
	}
	if badPad > 0 {
		return fmt.Errorf("%s holds %d row(s) whose pad is not %d bytes long", second.Table, badPad, payloadBytes)
	}
	return nil
}
