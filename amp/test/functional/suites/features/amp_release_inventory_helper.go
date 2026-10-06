package features

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
)

// A deleted endpoint's pod and PVC can sit in Terminating for a while; that
// is not a leak, an object still present after this is.
const releaseLeftoverSettle = 3 * time.Minute

// releaseInventory records what an endpoint's release created and returns the
// check that none of it outlives the endpoint's delete.
type releaseInventory func(ctx context.Context, ep controller.Endpoint) (func(context.Context) error, error)

// Every kind the endpoint chart renders, plus what helm and cert-manager
// create on its behalf. Kinds whose CRD the cluster lacks are skipped.
var releaseKinds = []string{
	"deployment", "replicaset", "pod", "service", "secret", "configmap", "persistentvolumeclaim",
	"certificates.cert-manager.io", "ingressroutetcps.traefik.io", "middlewaretcps.traefik.io", "dnsendpoints.externaldns.k8s.io",
}

func (f *flow) releaseInventory(ctx context.Context, ep controller.Endpoint) (func(context.Context) error, error) {
	kube := f.ampKube()
	if kube == nil {
		f.logf("no kube handle for the amp namespace, so the endpoint delete is not checked for leftover objects")
		return nil, nil
	}
	release := ampstack.EndpointWorkloadName(ep.ID)
	list := func(ctx context.Context) ([]string, error) {
		var out []string
		for _, kind := range releaseKinds {
			names, err := kube.GetSelector(ctx, kind, "app.kubernetes.io/instance="+release, "{.items[*].metadata.name}")
			if err != nil {
				if strings.Contains(err.Error(), "the server doesn't have a resource type") {
					continue
				}
				return nil, fmt.Errorf("list %s of release %s: %w", kind, release, err)
			}
			for _, n := range strings.Fields(names) {
				out = append(out, kind+"/"+n)
			}
		}
		helm, err := kube.GetSelector(ctx, "secret", "owner=helm,name="+release, "{.items[*].metadata.name}")
		if err != nil {
			return nil, fmt.Errorf("list helm storage secrets of release %s: %w", release, err)
		}
		for _, n := range strings.Fields(helm) {
			out = append(out, "secret/"+n)
		}
		// cert-manager writes a Certificate's Secret without the chart's labels.
		tls, err := kube.GetSelector(ctx, "certificates.cert-manager.io", "app.kubernetes.io/instance="+release, "{.items[*].spec.secretName}")
		if err == nil {
			for _, n := range strings.Fields(tls) {
				if _, err := kube.Get(ctx, "secret", n, "{.metadata.name}"); err == nil {
					out = append(out, "secret/"+n)
				}
			}
		}
		slices.Sort(out)
		return slices.Compact(out), nil
	}
	before, err := list(ctx)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(before, func(o string) bool { return strings.HasPrefix(o, "deployment/") }) {
		return nil, fmt.Errorf("no Deployment carries app.kubernetes.io/instance=%s before the delete (found %v), so the inventory would check nothing", release, before)
	}
	f.logf("release %s owns %d objects before the endpoint delete: %s", release, len(before), strings.Join(before, ", "))
	return func(ctx context.Context) error {
		var left []string
		err := pollUntil(ctx, releaseLeftoverSettle, 5*time.Second, func() (bool, error) {
			now, err := list(ctx)
			if err != nil {
				return false, err
			}
			left = now
			return len(now) == 0, nil
		})
		if err != nil {
			return fmt.Errorf("deleteEndpoint succeeded but %d objects of release %s were still in the namespace %s later: %s (before the delete: %s): a leaked PVC or LoadBalancer is billed and nothing else notices it",
				len(left), release, releaseLeftoverSettle, strings.Join(left, ", "), strings.Join(before, ", "))
		}
		return nil
	}, nil
}
