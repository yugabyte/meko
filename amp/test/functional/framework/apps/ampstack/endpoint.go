package ampstack

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// EndpointWorkloadName is the controller's deploy/release name for an endpoint:
// the endpoint UUID prefixed with "yb-ep-". The endpoint's workload object, its
// Service, and its pods all carry this name, and the pods carry it as the
// app.kubernetes.io/instance label.
func EndpointWorkloadName(endpointID string) string { return "yb-ep-" + endpointID }

// WaitEndpointWorkloadReady blocks until the endpoint's compute workload reports
// ready, whichever workload kind the controller deployed it as.
//
// A Deployment is waited on with condition=Available — the historical behaviour,
// and the only call made when the endpoint is a Deployment. A bare ReplicaSet has
// no conditions at all, so its equivalent gate is readyReplicas; the kind is
// resolved from the Deployment wait's own not-found result rather than a
// pre-probe, so the Deployment path stays a single kubectl call.
func WaitEndpointWorkloadReady(ctx context.Context, kube *k8s.Kubectl, endpointID string, timeout time.Duration) error {
	name := EndpointWorkloadName(endpointID)
	err := kube.WaitFor(ctx, "deployment/"+name, "condition=Available", timeout)
	if err == nil || !isNotFound(err) {
		return err
	}
	if rsErr := kube.WaitFor(ctx, "replicaset/"+name, "jsonpath={.status.readyReplicas}=1", timeout); rsErr != nil {
		return fmt.Errorf("endpoint %s has no ready compute workload: no Deployment %s (%v) and ReplicaSet %s never reported a ready replica: %w",
			endpointID, name, err, name, rsErr)
	}
	return nil
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not found")
}
