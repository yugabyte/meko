package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// EndpointHealth is whether an endpoint's pod is serving, read off the pod at
// call time. Reason is the pod's own account of why not, empty when Ready.
type EndpointHealth struct {
	Ready  bool
	Reason string
}

// EndpointHealthReader is the deployer's optional ability to say whether its
// endpoints are serving. Optional rather than part of Deployer because only a
// deployer with a workload API to read can answer; the API reports nothing for
// one that cannot, rather than guessing.
type EndpointHealthReader interface {
	// EndpointHealth reads the pods behind the given endpoints in one call. The
	// ids are release names, EndpointName(Endpoint.ID), and so are the map's keys.
	// Every id is answered: one with no pod is not ready. An error means nothing could be read, and the caller must report
	// health as unknown.
	EndpointHealth(ctx context.Context, ids []string) (map[string]EndpointHealth, error)
}

var _ EndpointHealthReader = (*KubernetesDeployer)(nil)

// EndpointHealth lists the pods of the given releases with one set-based
// selector on the instance label, the label every endpoint pod carries whatever
// release of the chart rendered it — the identity labels are newer.
func (k *KubernetesDeployer) EndpointHealth(ctx context.Context, ids []string) (map[string]EndpointHealth, error) {
	out := make(map[string]EndpointHealth, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	q := url.Values{"labelSelector": {"app.kubernetes.io/instance in (" + strings.Join(ids, ",") + ")"}}
	body, err := k.k8sAPIGetErr(ctx, fmt.Sprintf("/api/v1/namespaces/%s/pods?%s", k.cfg.Namespace, q.Encode()))
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode pod list: %w", err)
	}
	byRelease := make(map[string][]json.RawMessage, len(ids))
	for _, item := range list.Items {
		rel := podInstance(item)
		byRelease[rel] = append(byRelease[rel], item)
	}
	for _, id := range ids {
		out[id] = releaseHealth(byRelease[id])
	}
	return out, nil
}

// releaseHealth folds a release's pods into one answer. The deployment uses
// Recreate, so there is normally one. A second is either the old pod still
// terminating or one that has finished — an evicted pod stays listed, phase
// Failed, beside its replacement. Neither serves anything, and neither may
// supply the reason for the pod that is actually starting.
func releaseHealth(pods []json.RawMessage) EndpointHealth {
	var live []json.RawMessage
	for _, p := range pods {
		if !podTerminating(p) && !podFinished(p) {
			live = append(live, p)
		}
	}
	if len(live) == 0 {
		if len(pods) > 0 {
			return EndpointHealth{Reason: "endpoint pod is terminating or has exited, and no replacement exists"}
		}
		return EndpointHealth{Reason: "no endpoint pod exists"}
	}
	for _, p := range live {
		if podReady(p) {
			return EndpointHealth{Ready: true}
		}
	}
	reason := diagnosePod(live[0]).summary
	if reason == "" {
		reason = "endpoint pod is not ready"
	}
	return EndpointHealth{Reason: reason}
}

func podInstance(podBody []byte) string {
	var p struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if json.Unmarshal(podBody, &p) != nil {
		return ""
	}
	return p.Metadata.Labels["app.kubernetes.io/instance"]
}

func podFinished(podBody []byte) bool {
	var p struct {
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	return json.Unmarshal(podBody, &p) == nil && (p.Status.Phase == "Failed" || p.Status.Phase == "Succeeded")
}

func podTerminating(podBody []byte) bool {
	var p struct {
		Metadata struct {
			DeletionTimestamp *string `json:"deletionTimestamp"`
		} `json:"metadata"`
	}
	return json.Unmarshal(podBody, &p) == nil && p.Metadata.DeletionTimestamp != nil
}

// podReady reads the pod's Ready condition, which the kubelet sets only when
// every container's readiness probe passes — a crash-looping sidecar makes the
// whole endpoint not ready, as it should: it is a surface clients were handed.
func podReady(podBody []byte) bool {
	var p struct {
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if json.Unmarshal(podBody, &p) != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}
