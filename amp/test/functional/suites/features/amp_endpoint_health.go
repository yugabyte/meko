package features

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	healthTable = "amp_health"
	// One kubelet status sync plus the readiness probe's failure threshold.
	healthFlipTimeout = 3 * time.Minute
	// A tag nobody publishes on the endpoint's own repository, so the pod fails
	// on the pull and not on auth.
	healthMissingTag = "0.0.0-ampqa-missing"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_endpoint_health",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "An endpoint's health is read live from its pod (meko#1746). A writer and a writer on a fork of its branch both report ready on get and list. With the first writer's workload scaled to zero its health turns not_ready naming the missing pod while its address stays set and the fork's writer stays ready; scaled back it turns ready and serves its rows. With the writer's compute image swapped for a tag that cannot be pulled its health turns not_ready with a reason naming the pull, and once the image is restored it is ready and serves again. A deleted pod comes back ready on its own.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     40 * time.Minute,
		Run:         runAmpEndpointHealth,
	})
}

type healthCheckRun struct {
	a       *ampFeatureRun
	cl      *controller.Client
	kube    *k8s.Kubectl
	writer  controller.Endpoint
	sibling controller.Endpoint
	kind    string
}

func runAmpEndpointHealth(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		h := &healthCheckRun{a: a}
		return []phase{
			{"health_ready", h.ready},
			{"health_scaled_to_zero", h.scaledToZero},
			{"health_image_pull_failure", h.imagePull},
			{"health_after_pod_delete", h.podDelete},
		}
	})
}

func (h *healthCheckRun) ready(ctx context.Context) error {
	cl, err := h.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	h.cl = cl
	h.kube = h.a.f.ampKube()
	if h.kube == nil {
		h.a.f.t.Skip("no kube handle for the amp namespace, so the endpoint's pod cannot be broken on purpose")
		return nil
	}
	proj := h.a.proj
	if h.writer, err = createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID); err != nil {
		return err
	}
	conn, err := h.a.f.endpointSQL(ctx, h.writer)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.Exec(ctx, "CREATE TABLE "+healthTable+" AS SELECT g AS id FROM generate_series(1, 500) g"); err != nil {
		return fmt.Errorf("seed %s: %w", healthTable, err)
	}
	// A writer on a fork, not a reader on the same branch: a read_only endpoint
	// waits forever on the branch's write token (meko#833).
	fork := "health-" + h.a.f.suffix()
	if err := createBranchRetrying(ctx, cl, proj.poolID, proj.projID, controller.CreateBranchRequest{Name: fork, ParentBranchID: proj.branchID}); err != nil {
		return fmt.Errorf("fork the writer's branch: %w", err)
	}
	br, err := findBranchCtl(ctx, cl, proj.poolID, proj.projID, fork)
	if err != nil {
		return err
	}
	if h.sibling, err = createEndpoint(ctx, cl, proj.poolID, proj.projID, br.BranchID); err != nil {
		return err
	}
	for _, ep := range []controller.Endpoint{h.writer, h.sibling} {
		if err := h.waitHealth(ctx, ep, controller.HealthReady, ""); err != nil {
			return err
		}
	}
	return h.listAgrees(ctx, controller.HealthReady)
}

func (h *healthCheckRun) listAgrees(ctx context.Context, writerWant string) error {
	proj := h.a.proj
	want := map[string]string{h.writer.ID: writerWant, h.sibling.ID: controller.HealthReady}
	seen := 0
	for _, branchID := range []string{proj.branchID, h.sibling.BranchID} {
		eps, err := h.cl.ListAllEndpoints(ctx, proj.poolID, proj.projID, branchID)
		if err != nil {
			return fmt.Errorf("list endpoints of branch %s: %w", branchID, err)
		}
		for _, ep := range eps {
			got, err := h.cl.GetEndpoint(ctx, proj.poolID, proj.projID, branchID, ep.ID)
			if err != nil {
				return err
			}
			if ep.Health == nil || got.Health == nil {
				return fmt.Errorf("endpoint %s reports no health on list (%v) or get (%v), though the controller runs on Kubernetes and can read the pod", ep.ID, ep.Health, got.Health)
			}
			if ep.Health.Status != got.Health.Status {
				return fmt.Errorf("endpoint %s: list says %q and get says %q for the same pod", ep.ID, ep.Health.Status, got.Health.Status)
			}
			if w, ok := want[ep.ID]; ok {
				seen++
				if ep.Health.Status != w {
					return fmt.Errorf("endpoint %s reports %q on list and get, want %q", ep.ID, ep.Health.Status, w)
				}
			}
		}
	}
	if seen != len(want) {
		return fmt.Errorf("list returned %d of the %d endpoints under test", seen, len(want))
	}
	return nil
}

// reasonHas, when set, must appear in the lower-cased reason.
func (h *healthCheckRun) waitHealth(ctx context.Context, ep controller.Endpoint, want, reasonHas string) error {
	proj := h.a.proj
	var last *controller.EndpointHealth
	var lastErr error
	err := pollUntil(ctx, healthFlipTimeout, 2*time.Second, func() (bool, error) {
		got, err := h.cl.GetEndpoint(ctx, proj.poolID, proj.projID, ep.BranchID, ep.ID)
		if err != nil {
			lastErr = err
			return false, nil
		}
		if strings.TrimSpace(got.Address) == "" {
			return false, fmt.Errorf("endpoint %s lost its address while its health was being watched; address is set once and never cleared", ep.ID)
		}
		last = got.Health
		if last == nil || last.Status != want {
			return false, nil
		}
		if want == controller.HealthReady && last.Reason != "" {
			return false, fmt.Errorf("endpoint %s is ready but carries reason %q, which the spec says is absent when ready", ep.ID, last.Reason)
		}
		if want == controller.HealthNotReady && strings.TrimSpace(last.Reason) == "" {
			return false, nil
		}
		return reasonHas == "" || strings.Contains(strings.ToLower(last.Reason), reasonHas), nil
	})
	if err != nil {
		return fmt.Errorf("endpoint %s did not report health %q (reason containing %q) within %s; last health %+v, last read error %v: %w", ep.ID, want, reasonHas, healthFlipTimeout, last, lastErr, err)
	}
	return nil
}

func (h *healthCheckRun) workloadKind(ctx context.Context) string {
	if h.kind != "" {
		return h.kind
	}
	h.kind = "replicaset"
	if _, err := h.kube.Get(ctx, "deployment", ampstack.EndpointWorkloadName(h.writer.ID), "{.metadata.name}"); err == nil {
		h.kind = "deployment"
	}
	return h.kind
}

func (h *healthCheckRun) scale(ctx context.Context, n int) error {
	kind := h.workloadKind(ctx)
	name := ampstack.EndpointWorkloadName(h.writer.ID)
	if err := h.kube.Patch(ctx, kind, name, "merge", fmt.Sprintf(`{"spec":{"replicas":%d}}`, n)); err != nil {
		return fmt.Errorf("scale %s/%s to %d: %w", kind, name, n, err)
	}
	return nil
}

func (h *healthCheckRun) serves(ctx context.Context, where string) error {
	if err := h.waitHealth(ctx, h.writer, controller.HealthReady, ""); err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	conn, err := h.a.f.endpointSQL(ctx, h.writer)
	if err != nil {
		return fmt.Errorf("%s: the writer reports ready but does not accept a connection: %w", where, err)
	}
	defer conn.Close()
	n, err := conn.QueryInt(ctx, "SELECT count(*) FROM "+healthTable)
	if err != nil {
		return fmt.Errorf("%s: the writer reports ready but cannot read %s: %w", where, healthTable, err)
	}
	if n != 500 {
		return fmt.Errorf("%s: %s has %d rows, want 500", where, healthTable, n)
	}
	return nil
}

func (h *healthCheckRun) scaledToZero(ctx context.Context) error {
	if h.kube == nil {
		return nil
	}
	if err := h.scale(ctx, 0); err != nil {
		return err
	}
	restored := false
	defer func() {
		if !restored {
			_ = h.scale(context.WithoutCancel(ctx), 1)
		}
	}()
	if err := h.waitHealth(ctx, h.writer, controller.HealthNotReady, "no endpoint pod"); err != nil {
		return fmt.Errorf("with the writer scaled to zero: %w", err)
	}
	if err := h.waitHealth(ctx, h.sibling, controller.HealthReady, ""); err != nil {
		return fmt.Errorf("the fork's writer, while only the first writer was scaled to zero: %w", err)
	}
	if err := h.listAgrees(ctx, controller.HealthNotReady); err != nil {
		return err
	}
	if err := h.scale(ctx, 1); err != nil {
		return err
	}
	restored = true
	return h.serves(ctx, "after scaling the writer back to one")
}

func (h *healthCheckRun) imagePull(ctx context.Context) error {
	if h.kube == nil {
		return nil
	}
	kind := h.workloadKind(ctx)
	name := ampstack.EndpointWorkloadName(h.writer.ID)
	orig, err := h.kube.Get(ctx, kind, name, `{.spec.template.spec.containers[?(@.name=="endpoint")].image}`)
	if err != nil || orig == "" {
		return fmt.Errorf("read the endpoint container's image of %s/%s (got %q): %v", kind, name, orig, err)
	}
	repo := orig
	if i := strings.LastIndex(orig, ":"); i > strings.LastIndex(orig, "/") {
		repo = orig[:i]
	}
	setImage := func(ctx context.Context, image string) error {
		patch := fmt.Sprintf(`{"spec":{"template":{"spec":{"containers":[{"name":"endpoint","image":%q}]}}}}`, image)
		if err := h.kube.Patch(ctx, kind, name, "strategic", patch); err != nil {
			return fmt.Errorf("set the endpoint image of %s/%s to %s: %w", kind, name, image, err)
		}
		if kind == "replicaset" {
			// A bare ReplicaSet does not replace a running pod on a template edit.
			pod, err := h.kube.GetSelector(ctx, "pod", "app.kubernetes.io/instance="+name, "{.items[0].metadata.name}")
			if err == nil && pod != "" {
				return h.kube.Delete(ctx, "pod", pod)
			}
		}
		return nil
	}
	if err := setImage(ctx, repo+":"+healthMissingTag); err != nil {
		return err
	}
	restored := false
	defer func() {
		if !restored {
			_ = setImage(context.WithoutCancel(ctx), orig)
		}
	}()
	if err := h.waitHealth(ctx, h.writer, controller.HealthNotReady, "pull"); err != nil {
		return fmt.Errorf("with the writer's image swapped for %s: %w", healthMissingTag, err)
	}
	if err := setImage(ctx, orig); err != nil {
		return err
	}
	restored = true
	return h.serves(ctx, "after restoring the writer's image")
}

func (h *healthCheckRun) podDelete(ctx context.Context) error {
	if h.kube == nil {
		return nil
	}
	pod, err := h.kube.GetSelector(ctx, "pod", "app.kubernetes.io/instance="+ampstack.EndpointWorkloadName(h.writer.ID), "{.items[0].metadata.name}")
	if err != nil || pod == "" {
		return fmt.Errorf("find the writer's pod (got %q): %v", pod, err)
	}
	if err := h.kube.Delete(ctx, "pod", pod); err != nil {
		return fmt.Errorf("delete the writer's pod %s: %w", pod, err)
	}
	return h.serves(ctx, "after deleting the writer's pod "+pod)
}
