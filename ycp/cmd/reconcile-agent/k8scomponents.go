package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"meko/ycp/internal/api"
)

// Components reports what the data plane is running, as the console's components tab
// renders it: one row per workload, healthy-of-total, with the image tag as the
// version.
//
// The agent is the only thing that can report it. There is no inbound path to a BYOC
// data plane, so the control plane cannot look for itself, and the reconcile loop
// only knows about the resources it converges through the api-server — not whether
// the workloads beside it are up.
//
// What this covers is narrower than it first looks. Report is only reached when
// Engine.Reconcile succeeded (loop.go), and Reconcile begins with backend.List
// against the api-server — so a crash-looping api-server produces no status post at
// all, and the console sees a stale heartbeat rather than `meko-api-server 0/1`. The
// tab's value is the workloads the reconcile loop does not depend on: the UI, the MCP
// server, a bad yb-tserver replica.
//
// Deployments and StatefulSets only. DaemonSets and Jobs are deliberately out: a BYOC
// release runs none, and a completed Job reading 0/1 is a finished migration rather
// than an outage.
func (k *k8s) components() ([]api.Component, error) {
	// An agent whose Role lacks the grant is a deploy-time fact, not a transient
	// error: every tick would 403 identically, once per reconcile interval, forever.
	// Latch after the first so it costs one log line rather than a stream — the amp
	// chart's agent Role, and any meko release installed before this shipped, do not
	// carry it.
	if k.compsForbidden {
		return nil, nil
	}

	var out []api.Component
	for _, w := range []struct{ path, kind string }{
		{fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments", k.ns), "deployment"},
		{fmt.Sprintf("/apis/apps/v1/namespaces/%s/statefulsets", k.ns), "statefulset"},
	} {
		code, raw, err := k.do(http.MethodGet, w.path, "", nil)
		if err != nil {
			return nil, fmt.Errorf("list %ss: %w", w.kind, err)
		}
		if code == http.StatusForbidden {
			k.compsForbidden = true
			return nil, fmt.Errorf("list %ss: forbidden — the agent's Role needs list on apps/%ss; "+
				"components will not be reported until it does (not retried)", w.kind, w.kind)
		}
		if code != http.StatusOK {
			return nil, fmt.Errorf("list %ss: status %d", w.kind, code)
		}
		comps, err := parseWorkloads(raw)
		if err != nil {
			return nil, fmt.Errorf("decode %s list: %w", w.kind, err)
		}
		out = append(out, comps...)
	}
	// Stable order: the console renders the slice as given, and an unordered report
	// would reshuffle the table on every heartbeat.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// parseWorkloads maps an apps/v1 list response to components. Split out from the
// request so the mapping — which is where the fiddly cases are — is testable without
// a cluster.
func parseWorkloads(raw []byte) ([]api.Component, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				// A pointer because Kubernetes omits replicas when it is nil, which
				// has to be told apart from an explicit 0.
				Replicas *int `json:"replicas"`
				Template struct {
					Spec struct {
						Containers []struct {
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				ReadyReplicas int `json:"readyReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}

	out := make([]api.Component, 0, len(list.Items))
	for _, it := range list.Items {
		total := 1
		if it.Spec.Replicas != nil {
			total = *it.Spec.Replicas
		}
		// Scaled to zero: omitted rather than reported 0/0, because the console reads
		// total==0 as an outage — it paints the row red and lists the workload under
		// "Waiting on" — so a deliberately stopped component would show as a fault.
		if total == 0 {
			continue
		}
		var image string
		if cs := it.Spec.Template.Spec.Containers; len(cs) > 0 {
			image = cs[0].Image
		}
		out = append(out, api.Component{
			Name:    it.Metadata.Name,
			Healthy: it.Status.ReadyReplicas,
			Total:   total,
			Version: imageTag(image),
		})
	}
	return out, nil
}

// imageTag is the tag half of an image reference. A digest-pinned or untagged
// reference has no tag worth showing, and an empty Version leaves the console's
// version column blank rather than printing a hostname or a digest as a version.
func imageTag(image string) string {
	if image == "" || strings.Contains(image, "@") {
		return ""
	}
	i := strings.LastIndex(image, ":")
	if i < 0 || strings.Contains(image[i+1:], "/") {
		return ""
	}
	return image[i+1:]
}
