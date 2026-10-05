package functional

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/chaos"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	k8senv "github.com/yugabyte/meko/meko/test/functional/framework/env/k8s"
	execk8s "github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

const (
	langfuseSelector = "app=langfuse-web-worker"
	// The api-server's Langfuse client gives up after 20s per call, and a create
	// makes several, so the REST client has to outwait them to see the answer.
	outageCallTimeout = 3 * time.Minute
	// Behind the production ingress a request this slow comes back as "Upstream
	// request timed out" instead of an answer.
	ingressTimeout = 60 * time.Second
	recoverWithin  = 3 * time.Minute
	endpointSettle = 10 * time.Second
)

func init() {
	core.Register(core.Test{
		Name:       "functional/langfuse_outage",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags("resilience"),
		OwningTeam: core.TeamAPIServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack, FaultsSharedInfra: true},
		Timeout:    20 * time.Minute,
		// A frozen pod fails its liveness probe, and the 5xx the delete answers is
		// asserted, MEKO-811, by its own step.
		Tolerates: []string{core.GuardPodRestart, core.GuardHTTP5xx},
		Description: "Freeze the Langfuse pod. A datapack created meanwhile is either refused, or gets its Langfuse project once Langfuse is back " +
			"and then serves MCP calls (MEKO-621). Deleting a datapack is not blocked by Langfuse (MEKO-811). Create answers within the " +
			"ingress timeout, and no error names an internal endpoint (MEKO-812).",
		Run: langfuseOutage,
	})
}

func langfuseOutage(t *core.T) error {
	e := stacktest.ClusterEnv(t)
	stack := e.Stack()
	api := stacktest.API(t).WithTimeout(outageCallTimeout)
	suffix := t.ResourceSuffix()

	var made []string
	defer func() {
		ctx, cancel := stacktest.CleanupCtx()
		defer cancel()
		for _, id := range made {
			_ = api.DeleteDatapack(ctx, id)
		}
	}()

	var before mekoapi.Datapack
	if err := t.Step("create_before_outage", func(h *core.StepHandle) error {
		var err error
		before, err = api.CreateDatapack(t, mekoapi.CreateDatapackRequest{Name: "lf-before-" + suffix})
		if err != nil {
			return err
		}
		made = append(made, before.ID)
		h.Set("datapack_id", before.ID)
		return waitForProject(t, stack, before.ID, 30*time.Second)
	}); err != nil {
		return err
	}

	fault, target, pod, err := freezeLangfuse(t, stack.Kube())
	if err != nil {
		return err
	}
	frozen := true
	thaw := func() error {
		if !frozen {
			return nil
		}
		ctx, cancel := stacktest.CleanupCtx()
		defer cancel()
		if err := fault.Recover(ctx, target); err != nil {
			return err
		}
		frozen = false
		return nil
	}
	defer func() { _ = thaw() }()

	var during mekoapi.Datapack
	var errs []error
	if err := t.Step("create_during_outage", func(h *core.StepHandle) error {
		name := "lf-during-" + suffix
		start := time.Now()
		dp, err := api.CreateDatapack(t, mekoapi.CreateDatapackRequest{Name: name})
		took := time.Since(start)
		h.Set("took", took.Round(time.Millisecond).String())
		if dp.ID != "" {
			during = dp
			made = append(made, dp.ID)
			h.Set("datapack_id", dp.ID)
		}
		h.Set("outcome", outcome(err))
		if took > ingressTimeout {
			return fmt.Errorf("create took %s with Langfuse down, past the %s an ingress waits, so a user sees \"Upstream request timed out\"", took.Round(time.Second), ingressTimeout)
		}
		if err == nil {
			return nil
		}
		left, lerr := datapackNamed(t, api, name)
		if lerr != nil {
			return lerr
		}
		if left != "" {
			made = append(made, left)
			return fmt.Errorf("create failed (%v) but left datapack %s behind", err, left)
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("delete_during_outage", func(h *core.StepHandle) error {
		err := api.DeleteDatapack(t, before.ID)
		h.Set("outcome", outcome(err))
		if err != nil {
			return fmt.Errorf("delete refused while Langfuse is down (MEKO-811): %w", err)
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("thaw", func(*core.StepHandle) error { return thaw() }); err != nil {
		return err
	}

	// The probe waits for the pod to pass readiness again: a create while the
	// Service still has no ready endpoint loses its project for good (MEKO-621),
	// and the probe would then measure that bug instead of the recovery.
	if err := t.Step("langfuse_back", func(*core.StepHandle) error {
		if err := stack.Kube().WaitFor(t, "pod/"+pod, "condition=Ready", recoverWithin); err != nil {
			return fmt.Errorf("Langfuse did not pass readiness after the thaw: %w", err)
		}
		select {
		case <-time.After(endpointSettle):
		case <-t.Done():
			return t.Err()
		}
		probe, err := api.CreateDatapack(t, mekoapi.CreateDatapackRequest{Name: "lf-probe-" + suffix})
		if err != nil {
			return err
		}
		made = append(made, probe.ID)
		return waitForProject(t, stack, probe.ID, time.Minute)
	}); err != nil {
		return err
	}

	if during.ID != "" {
		if err := t.Step("project_after_recovery", func(*core.StepHandle) error {
			if err := waitForProject(t, stack, during.ID, recoverWithin); err != nil {
				return fmt.Errorf("create answered success during the outage and nothing provisioned the project afterwards (MEKO-621): %w", err)
			}
			return nil
		}); err != nil {
			errs = append(errs, err)
		}

		if err := t.Step("mcp_scoped_call_after_recovery", func(h *core.StepHandle) error {
			return conversationWorks(t, api, during.ID, h)
		}); err != nil {
			errs = append(errs, err)
		}
	}

	if err := t.Step("delete_after_recovery", func(*core.StepHandle) error {
		var failed []string
		for _, id := range made {
			if err := api.DeleteDatapack(t, id); err != nil && !mekoapi.IsNotFound(err) {
				failed = append(failed, fmt.Sprintf("%s: %v", id, err))
			}
		}
		made = nil
		if len(failed) > 0 {
			return fmt.Errorf("deletes failed with Langfuse back: %s", strings.Join(failed, "; "))
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// freezeLangfuse stops the Langfuse pod's processes in place, so connections
// to it hang instead of being refused, the way an overloaded or partitioned
// Langfuse behaves.
func freezeLangfuse(t *core.T, kube *execk8s.Kubectl) (chaos.ChaosAction, *chaos.Target, string, error) {
	var fault chaos.ChaosAction
	var target *chaos.Target
	var pod string
	err := t.Step("freeze_langfuse", func(h *core.StepHandle) error {
		var err error
		pod, err = kube.GetSelector(t, "pods", langfuseSelector, "{.items[0].metadata.name}")
		if err != nil || pod == "" {
			return fmt.Errorf("find the Langfuse pod (%s): %v", langfuseSelector, err)
		}
		h.Set("pod", pod)
		fault, err = chaos.DefaultRegistry().Build(chaos.ActionPodFreeze, chaos.FromConfig(t.Options))
		if err != nil {
			return err
		}
		target = chaos.NewPodTarget("langfuse", kube, pod, "")
		return fault.Inject(t, target)
	})
	return fault, target, pod, err
}

// conversationWorks opens a conversation on datapack id over MCP, the first call
// a client makes and the one that fails with "No Langfuse project found".
func conversationWorks(t *core.T, api *mekoapi.Client, id string, h *core.StepHandle) error {
	tok, err := api.CreateToken(t, mekoapi.CreateTokenRequest{Name: "mekoqa-lf-" + t.ResourceSuffix(), TTLDays: 1})
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := stacktest.CleanupCtx()
		defer cancel()
		_ = api.RevokeToken(ctx, tok.Record.ID)
	}()
	mcp := stacktest.MCP(t, tok.Value)
	defer func() {
		ctx, cancel := stacktest.CleanupCtx()
		defer cancel()
		mcp.Close(ctx)
	}()
	if _, err := mcp.Initialize(t); err != nil {
		return err
	}
	res, err := mcp.CallTool(t, "conversation_create", map[string]any{"datapack_id": id, "title": "mekoqa langfuse_outage"})
	if err != nil {
		return err
	}
	h.Set("conversation_create", res.Text())
	if res.IsError {
		return fmt.Errorf("conversation_create on %s answered a tool error (MEKO-621): %s", id, res.Text())
	}
	return nil
}

func waitForProject(t *core.T, stack k8senv.Stack, id string, within time.Duration) error {
	return t.WaitFor(fmt.Sprintf("a Langfuse project for datapack %s", id), within, 5*time.Second, func() (bool, error) {
		return stacktest.HasLangfuseProject(t, stack, id)
	})
}

func datapackNamed(t *core.T, api *mekoapi.Client, name string) (string, error) {
	list, err := api.ListDatapacks(t)
	if err != nil {
		return "", err
	}
	for _, d := range list {
		if d.Name == name {
			return d.ID, nil
		}
	}
	return "", nil
}

func outcome(err error) string {
	if err == nil {
		return "ok"
	}
	if st := mekoapi.StatusOf(err); st != 0 {
		return fmt.Sprintf("HTTP %d", st)
	}
	return err.Error()
}
