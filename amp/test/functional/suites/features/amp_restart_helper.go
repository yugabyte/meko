package features

import (
	"context"
	"fmt"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
)

// restartEndpointCompute rolls the endpoint's pod and reconnects at the fresh
// one; ok=false means no kube handle, so the caller skips its restart half.
func (a *ampFeatureRun) restartEndpointCompute(ctx context.Context, ep controller.Endpoint) (sqlConn, bool, error) {
	kube := a.f.ampKube()
	if kube == nil {
		a.f.logf("no kube handle for the endpoint pod, so the restart half is not run")
		return nil, false, nil
	}
	roller := &kubeComputeRoller{kube: kube, timeout: restartReadyTimeout}
	pod, err := roller.podName(ctx, ep.ID)
	if err != nil {
		return nil, false, err
	}
	if err := roller.deletePod(ctx, pod); err != nil {
		return nil, false, fmt.Errorf("delete endpoint compute pod %s: %w", pod, err)
	}
	if err := roller.waitReady(ctx, ep.ID); err != nil {
		return nil, false, fmt.Errorf("endpoint compute did not come back after restart: %w", err)
	}
	ip, err := roller.runningPodIP(ctx, ep.ID)
	if err != nil {
		return nil, false, err
	}
	c, err := a.f.endpointSQLAt(ctx, ip, ep)
	if err != nil {
		return nil, false, fmt.Errorf("reconnect after restart: %w", err)
	}
	return c, true, nil
}
