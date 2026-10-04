// Kills the amp controller pod while branch and endpoint tasks are in flight:
// the task contract (meko#578, meko#971) promises a task that loses its worker
// is re-claimed after the restart and finishes.
package stress

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/control_plane_nemesis",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack, FaultsSharedInfra: true},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Force-delete the amp controller pod while branch and endpoint tasks are in flight, then hold the control plane to the task contract (meko#578, meko#971): every fired task finishes after the restart, no task stays non-terminal, no orphaned compute pod survives, tenants keep committing through the outage, and the control plane still creates a tenant.",
		Params:      []string{"stress.control_plane_nemesis", "stress.consistency", "stress.fault_metrics"},
		Timeout:     90 * time.Minute,
		Run:         runControlPlaneNemesis,
	})
}

type controlPlaneNemesisParams struct {
	Tenants        int
	Rounds         int
	InFlightBudget time.Duration
	SettleTimeout  time.Duration
	PayloadBytes   int
	ResumeDuration time.Duration
}

func controlPlaneNemesisParamsFromConfig(c *core.Config) controlPlaneNemesisParams {
	return controlPlaneNemesisParams{
		Tenants:        c.GetInt("stress.control_plane_nemesis.tenants", 2),
		Rounds:         c.GetInt("stress.control_plane_nemesis.rounds", 2),
		InFlightBudget: c.GetDuration("stress.control_plane_nemesis.in_flight_budget", 60*time.Second),
		SettleTimeout:  c.GetDuration("stress.control_plane_nemesis.settle_timeout", 15*time.Minute),
		PayloadBytes:   c.GetInt("stress.control_plane_nemesis.payload_bytes", 256),
		ResumeDuration: c.GetDuration("stress.control_plane_nemesis.resume_duration", 30*time.Second),
	}
}

type churnBranch struct {
	Tenant     int
	Round      int
	Name       string
	BranchID   string
	EndpointID string
}

type firedTask struct {
	ID   string
	Kind string
	Desc string
}

func runControlPlaneNemesis(t *core.T) error {
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	ampEnv, ok := t.Env.(ampKubeEnv)
	if !ok || ampEnv.AmpKube() == nil {
		t.Skip("requires a kube handle for the control-plane namespace: the controller pod is the fault target")
		return nil
	}
	params := controlPlaneNemesisParamsFromConfig(t.Options)
	if params.Rounds < 1 {
		return fmt.Errorf("stress.control_plane_nemesis.rounds must be at least 1, got %d: no fault would be injected", params.Rounds)
	}
	logf := t.Masker().Logf(log.Printf)
	kube := ampEnv.AmpKube()
	controllerSelector := "app=" + ampstack.FromConfig(t.Options).ControllerName

	fleet, err := newTenantFleet(t, env, logf)
	if err != nil {
		return err
	}
	defer fleet.Close()

	cpsrc, cpspecs := controllerFaultMetrics(t)
	faults := startFaultMetrics(t, kube, cpsrc, cpspecs, logf)
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
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("seed_writes", func(s *core.StepHandle) error {
		runJournaledWriters(t.Context, env, fleet.Tenants, journals, t.Seed.Fork("seed"),
			20*time.Second, commitDurabilityParams{PayloadBytes: params.PayloadBytes}, nil)
		acked := totalAcked(journals)
		s.Set("acked", acked)
		if acked == 0 {
			return fmt.Errorf("no tenant was acknowledged for a single write before the chaos, so there is no baseline to hold the data plane to")
		}
		return nil
	}); err != nil {
		return err
	}

	var live []churnBranch
	for r := 1; r <= params.Rounds; r++ {
		var fired []firedTask
		var gate *settleGate

		if err := t.Step(fmt.Sprintf("tasks_in_flight_%d", r), func(s *core.StepHandle) error {
			// Settle before firing: a 20s settle between firing and the kill let every
			// task finish first, so the kill interrupted nothing and no reclaim happened.
			gate, err = awaitSteadyState(t, s, env, fleet.Tenants, journals, params.PayloadBytes, 0)
			if err != nil {
				return err
			}
			cl, err := env.AmpController(t.Context)
			if err != nil {
				return err
			}
			for i := range fleet.Tenants {
				ten := &fleet.Tenants[i]
				name := fmt.Sprintf("cpn-%s-r%d-t%d", t.ResourceSuffix(), r, i)
				task, _, err := cl.CreateBranch(t.Context, fleet.poolID, ten.ClusterID, controller.CreateBranchRequest{Name: name})
				if err != nil {
					return fmt.Errorf("tenant %d: fire branch create %s: %w", i, name, err)
				}
				fired = append(fired, firedTask{ID: task.ID, Kind: task.Kind, Desc: "create branch " + name})
				live = append(live, churnBranch{Tenant: i, Round: r, Name: name})
			}
			for bi := range live {
				b := &live[bi]
				ten := &fleet.Tenants[b.Tenant]
				// Last round's branch is ready, so this is a live endpoint task to interrupt.
				if b.Round == r-1 && b.EndpointID == "" && b.BranchID != "" {
					task, _, err := cl.CreateEndpoint(t.Context, fleet.poolID, ten.ClusterID, b.BranchID,
						controller.CreateEndpointRequest{Role: tenantRole, Resources: fleet.endpointResources()})
					if err != nil {
						return fmt.Errorf("tenant %d: fire endpoint create on branch %s: %w", b.Tenant, b.Name, err)
					}
					fired = append(fired, firedTask{ID: task.ID, Kind: task.Kind, Desc: "create endpoint on branch " + b.Name})
					b.EndpointID = "pending"
				}
				// Deleted with its endpoint attached, so delete_branch retires a running compute.
				if b.Round == r-2 && b.BranchID != "" {
					task, _, err := cl.DeleteBranch(t.Context, fleet.poolID, ten.ClusterID, b.BranchID)
					if err != nil {
						return fmt.Errorf("tenant %d: fire branch delete %s: %w", b.Tenant, b.Name, err)
					}
					fired = append(fired, firedTask{ID: task.ID, Kind: task.Kind, Desc: "delete branch " + b.Name})
					b.BranchID = ""
				}
			}
			s.Set("tasks_fired", len(fired))

			running := 0
			_ = t.WaitFor("at least one fired task to be claimed and running", params.InFlightBudget, time.Second,
				func() (bool, error) {
					running = 0
					for _, f := range fired {
						task, err := cl.GetTask(t.Context, f.ID)
						if err != nil {
							return false, nil
						}
						if task.Status == controller.TaskRunning {
							running++
						}
					}
					return running > 0, nil
				})
			// Killing under only-pending tasks still tests the claim path.
			s.Set("running_when_killed", running)
			return nil
		}); err != nil {
			return err
		}

		var killed []string
		if err := t.Step(fmt.Sprintf("kill_controller_%d", r), func(s *core.StepHandle) error {
			cl, err := env.AmpController(t.Context)
			if err != nil {
				return err
			}
			running := 0
			for _, f := range fired {
				if task, err := cl.GetTask(t.Context, f.ID); err == nil && task.Status == controller.TaskRunning {
					running++
				}
			}
			s.Set("running_at_kill", running)
			names, err := kube.GetSelector(t.Context, "pod", controllerSelector, "{.items[*].metadata.name}")
			if err != nil {
				return fmt.Errorf("list controller pods (%s) in %q: %w", controllerSelector, kube.Namespace(), err)
			}
			pods := strings.Fields(names)
			if len(pods) == 0 {
				return fmt.Errorf("no pod matches %s in namespace %q, so there is no controller to kill", controllerSelector, kube.Namespace())
			}
			for _, pod := range pods {
				if err := kube.DeleteNow(t.Context, pod); err != nil {
					return fmt.Errorf("force-delete controller pod %s: %w", pod, err)
				}
			}
			s.Set("killed", strings.Join(pods, ","))
			killed = pods
			faults.Open()
			return nil
		}); err != nil {
			return err
		}

		if err := t.Step(fmt.Sprintf("control_plane_ready_%d", r), func(s *core.StepHandle) error {
			waited, err := awaitControlPlaneReady(t, fleet, params.SettleTimeout)
			if err != nil {
				return err
			}
			s.Set("ready_after", waited.Round(time.Second).String())
			faults.Close()
			return gate.attestNonTarget(t, s, killed...)
		}); err != nil {
			return err
		}

		if err := t.Step(fmt.Sprintf("tasks_finish_%d", r), func(s *core.StepHandle) error {
			cl, err := env.AmpController(t.Context)
			if err != nil {
				return err
			}
			for _, f := range fired {
				start := time.Now()
				wctx, cancel := context.WithTimeout(t.Context, params.SettleTimeout)
				task, err := cl.WaitTask(wctx, f.ID)
				cancel()
				if err != nil {
					return fmt.Errorf("task %s (%s, %q) did not succeed after the controller was killed mid-flight, though a task that loses its worker must be re-claimed and finished (meko#578, meko#971): %w",
						f.ID, f.Kind, f.Desc, err)
				}
				s.Set("settle_seconds_"+task.Kind+"_"+f.ID[:8], time.Since(start).Seconds())
			}
			for bi := range live {
				b := &live[bi]
				if b.Round != r || b.BranchID != "" {
					continue
				}
				branches, err := cl.ListAllBranches(t.Context, fleet.poolID, fleet.Tenants[b.Tenant].ClusterID)
				if err != nil {
					return err
				}
				for _, br := range branches {
					if br.Name == b.Name {
						b.BranchID = br.BranchID
					}
				}
				if b.BranchID == "" {
					return fmt.Errorf("branch %s is absent after its create task succeeded across a controller restart", b.Name)
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}

	if err := t.Step("no_stuck_tasks", func(s *core.StepHandle) error {
		cl, err := env.AmpController(t.Context)
		if err != nil {
			return err
		}
		var stuck []string
		err = t.WaitFor("every task to reach a terminal status", params.SettleTimeout, 10*time.Second,
			func() (bool, error) {
				stuck = stuck[:0]
				for _, status := range []string{controller.TaskPending, controller.TaskRunning} {
					list, err := cl.ListTasks(t.Context, controller.ListTasksOptions{Limit: 500, Status: status})
					if err != nil {
						return false, nil
					}
					for _, task := range list.Tasks {
						stuck = append(stuck, fmt.Sprintf("%s(%s,%s)", task.ID, task.Kind, task.Status))
					}
				}
				return len(stuck) == 0, nil
			})
		if err != nil {
			return fmt.Errorf("%d task(s) are still non-terminal %s after the last controller kill, though every worker loss must be recovered by a re-claim (meko#971): %s",
				len(stuck), params.SettleTimeout, strings.Join(stuck, ", "))
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("no_orphan_computes", func(s *core.StepHandle) error {
		cl, err := env.AmpController(t.Context)
		if err != nil {
			return err
		}
		var orphans []string
		err = t.WaitFor("every compute pod to belong to a registered endpoint", params.SettleTimeout, 15*time.Second,
			func() (bool, error) {
				known := map[string]bool{}
				clusters, err := cl.ListClusters(t.Context, fleet.poolID, 500, 0)
				if err != nil {
					return false, nil
				}
				for _, c := range clusters.Clusters {
					branches, err := cl.ListAllBranches(t.Context, fleet.poolID, c.ID)
					if err != nil {
						return false, nil
					}
					for _, br := range branches {
						eps, err := cl.ListAllEndpoints(t.Context, fleet.poolID, c.ID, br.BranchID)
						if err != nil {
							return false, nil
						}
						for _, ep := range eps {
							known[ep.ID] = true
						}
					}
				}
				pods, err := kube.ListPods(t.Context)
				if err != nil {
					return false, nil
				}
				orphans = orphans[:0]
				for _, pod := range pods {
					if !strings.HasPrefix(pod, endpointPodPrefix) {
						continue
					}
					owned := false
					for id := range known {
						if strings.HasPrefix(pod, endpointPodPrefix+id) {
							owned = true
							break
						}
					}
					if !owned {
						orphans = append(orphans, pod)
					}
				}
				return len(orphans) == 0, nil
			})
		if err != nil {
			return fmt.Errorf("%d compute pod(s) serve no endpoint the registry knows %s after the chaos, so a killed controller leaked computes: %s",
				len(orphans), params.SettleTimeout, strings.Join(orphans, ", "))
		}
		return nil
	}); err != nil {
		return err
	}

	// The data plane never depends on the controller being alive.
	if err := t.Step("data_plane_undisturbed", func(s *core.StepHandle) error {
		before := totalAcked(journals)
		runJournaledWriters(t.Context, env, fleet.Tenants, journals, t.Seed.Fork("resume"),
			params.ResumeDuration, commitDurabilityParams{PayloadBytes: params.PayloadBytes}, nil)
		delta := totalAcked(journals) - before
		s.Set("acked_after_chaos", delta)
		if delta == 0 {
			return fmt.Errorf("no tenant was acknowledged for a single write in %s after the controller recovered: killing the control plane must not stall tenant commits", params.ResumeDuration)
		}
		return verifyExactSets(t, s, env, fleet.Tenants, journals, params.PayloadBytes)
	}); err != nil {
		return err
	}

	if err := t.Step("control_plane_still_creates", func(s *core.StepHandle) error {
		before := len(fleet.Tenants)
		if err := fleet.Create(t, 1); err != nil {
			return fmt.Errorf("the control plane cannot create a tenant after the chaos: %w", err)
		}
		if len(fleet.Tenants) <= before {
			return fmt.Errorf("creating a tenant after the chaos reported success but added none")
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
