package api_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
)

// healthDeployer is a fakeDeployer that can also say whether its endpoints'
// pods are serving.
type healthDeployer struct {
	*fakeDeployer
	health map[string]deploy.EndpointHealth
	err    error
}

func (d *healthDeployer) EndpointHealth(_ context.Context, ids []string) (map[string]deploy.EndpointHealth, error) {
	if d.err != nil {
		return nil, d.err
	}
	out := map[string]deploy.EndpointHealth{}
	for _, id := range ids {
		if h, ok := d.health[id]; ok {
			out[id] = h
		}
	}
	return out, nil
}

// An endpoint keeps its address after its pod stops serving, so health is what
// tells a caller the two apart (meko#1746). It is read live and reported only
// when the deployer can read it: unknown must never render as healthy, and a
// failed read must never render as down.
func TestEndpointHealth(t *testing.T) {
	dep := &healthDeployer{fakeDeployer: &fakeDeployer{}}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "health-pool")
	withWriter := true
	cr, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name: "health", WithWriter: &withWriter, Password: strptr(testPassword),
	})
	if err != nil || cr.StatusCode() != http.StatusAccepted {
		t.Fatalf("create cluster: %v (%d)", err, cr.StatusCode())
	}
	drainTasks(t, store, backend, prov)
	clusterID := *cr.JSON202.ClusterId
	cl, err := client.GetClusterWithResponse(ctx, pool.Id, clusterID)
	if err != nil || cl.JSON200 == nil {
		t.Fatalf("get cluster: %v", err)
	}
	branchID := cl.JSON200.DefaultBranch.BranchId

	read := func(t *testing.T) (listed, got *api.EndpointHealth) {
		t.Helper()
		eps, err := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
		if err != nil || eps.JSON200 == nil || len(eps.JSON200.Endpoints) != 1 {
			t.Fatalf("list endpoints: %v", err)
		}
		ep := eps.JSON200.Endpoints[0]
		g, err := client.GetEndpointWithResponse(ctx, pool.Id, clusterID, branchID, ep.Id)
		if err != nil || g.JSON200 == nil {
			t.Fatalf("get endpoint: %v", err)
		}
		return ep.Health, g.JSON200.Health
	}
	eps, err := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
	if err != nil || eps.JSON200 == nil || len(eps.JSON200.Endpoints) != 1 {
		t.Fatalf("list endpoints: %v", err)
	}
	release := deploy.EndpointName(eps.JSON200.Endpoints[0].Id)

	t.Run("ready", func(t *testing.T) {
		dep.health, dep.err = map[string]deploy.EndpointHealth{release: {Ready: true}}, nil
		for _, h := range pair(read(t)) {
			if h == nil || h.Status != api.Ready || h.Reason != nil {
				t.Errorf("health = %+v, want ready with no reason", h)
			}
		}
	})
	t.Run("not ready carries the pod's reason", func(t *testing.T) {
		const why = "container documentdb-gateway (image gw:1) cannot start: CrashLoopBackOff"
		dep.health, dep.err = map[string]deploy.EndpointHealth{release: {Reason: why}}, nil
		for _, h := range pair(read(t)) {
			if h == nil || h.Status != api.NotReady || h.Reason == nil || *h.Reason != why {
				t.Errorf("health = %+v, want not_ready %q", h, why)
			}
		}
	})
	t.Run("a failed read reports nothing", func(t *testing.T) {
		dep.health, dep.err = nil, errors.New("apiserver unreachable")
		for _, h := range pair(read(t)) {
			if h != nil {
				t.Errorf("health = %+v, want absent", h)
			}
		}
	})
}

// A deployer that cannot read its pods reports no health at all.
func TestEndpointHealthAbsentWithoutReader(t *testing.T) {
	prov := api.Provisioning{Deployer: &fakeDeployer{}}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "no-health-pool")
	withWriter := true
	cr, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name: "nohealth", WithWriter: &withWriter, Password: strptr(testPassword),
	})
	if err != nil || cr.StatusCode() != http.StatusAccepted {
		t.Fatalf("create cluster: %v (%d)", err, cr.StatusCode())
	}
	drainTasks(t, store, backend, prov)
	cl, err := client.GetClusterWithResponse(ctx, pool.Id, *cr.JSON202.ClusterId)
	if err != nil || cl.JSON200 == nil {
		t.Fatalf("get cluster: %v", err)
	}
	eps, err := client.ListEndpointsWithResponse(ctx, pool.Id, *cr.JSON202.ClusterId, cl.JSON200.DefaultBranch.BranchId, nil)
	if err != nil || eps.JSON200 == nil || len(eps.JSON200.Endpoints) != 1 {
		t.Fatalf("list endpoints: %v", err)
	}
	if h := eps.JSON200.Endpoints[0].Health; h != nil {
		t.Errorf("health = %+v, want absent", h)
	}
}

func pair(a, b *api.EndpointHealth) []*api.EndpointHealth { return []*api.EndpointHealth{a, b} }
