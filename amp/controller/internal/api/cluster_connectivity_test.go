package api_test

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
)

// connDeployer is fakeDeployer plus the capability the nlb_* modes require; the
// bare fake stands in for a driver that cannot render a load balancer.
type connDeployer struct{ *fakeDeployer }

func (connDeployer) SupportsConnectivity() bool { return true }

// The one-call onboarding path creates the writer itself, so connectivity has to
// travel handler -> task args -> step -> deploy spec. A break anywhere in that
// chain is silent: the cluster comes up, and the endpoint is simply in-cluster.
func TestCreateClusterCarriesConnectivityToTheWriter(t *testing.T) {
	fake := &fakeDeployer{}
	prov := api.Provisioning{Deployer: connDeployer{fake}}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "conn-pool")

	withWriter := true
	mode := api.EndpointConnectivityMode(deploy.ConnectivityNLBInternal)
	cidrs := []string{"10.0.0.0/16"}
	ok, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name:       "exposed",
		WithWriter: &withWriter, Password: strptr(testPassword),
		Connectivity: &api.EndpointConnectivity{
			Mode:         &mode,
			AllowedCidrs: &cidrs,
		},
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if ok.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", ok.StatusCode(), ok.Body)
	}
	drainTasks(t, store, backend, prov)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.created) != 1 {
		t.Fatalf("deployer saw %d endpoints, want the one writer", len(fake.created))
	}
	got := fake.created[0]
	if got.Connectivity != deploy.ConnectivityNLBInternal {
		t.Errorf("writer spec Connectivity = %q, want %q", got.Connectivity, deploy.ConnectivityNLBInternal)
	}
	if !reflect.DeepEqual(got.AllowedCIDRs, cidrs) {
		t.Errorf("writer spec AllowedCIDRs = %v, want %v", got.AllowedCIDRs, cidrs)
	}
}

// Omitted connectivity has to keep meaning cluster_ip, or every caller that
// predates the option changes behaviour.
func TestCreateClusterWithoutConnectivityLeavesTheDeployerDefault(t *testing.T) {
	fake := &fakeDeployer{}
	prov := api.Provisioning{Deployer: connDeployer{fake}}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "plain-pool")

	withWriter := true
	ok, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name: "plain", WithWriter: &withWriter, Password: strptr(testPassword),
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if ok.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", ok.StatusCode(), ok.Body)
	}
	drainTasks(t, store, backend, prov)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.created) != 1 {
		t.Fatalf("deployer saw %d endpoints, want the one writer", len(fake.created))
	}
	if got := fake.created[0]; got.Connectivity != "" || got.AllowedCIDRs != nil {
		t.Errorf("writer spec = (%q, %v), want empty so the deployer applies cluster_ip",
			got.Connectivity, got.AllowedCIDRs)
	}
}

// Refused, not ignored: without a writer there is nothing to expose, and
// accepting it would report success for an exposure that never happens.
func TestCreateClusterRejectsConnectivityWithoutWriter(t *testing.T) {
	prov := api.Provisioning{Deployer: connDeployer{&fakeDeployer{}}}
	client, _, _ := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "no-writer-pool")

	mode := api.EndpointConnectivityMode(deploy.ConnectivityNLBExternal)
	got, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name:         "bare",
		Connectivity: &api.EndpointConnectivity{Mode: &mode},
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if got.StatusCode() != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", got.StatusCode(), got.Body)
	}
}

// The mode enum and CIDR parsing are the same validator createEndpoint uses;
// this pins that the composite path actually reaches it.
func TestCreateClusterValidatesConnectivity(t *testing.T) {
	prov := api.Provisioning{Deployer: connDeployer{&fakeDeployer{}}}
	client, _, _ := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "bad-conn-pool")

	withWriter := true
	bogus := api.EndpointConnectivityMode("nlb_bogus")
	bare := []string{"10.0.0.1"}
	external := api.EndpointConnectivityMode(deploy.ConnectivityNLBExternal)

	for _, tc := range []struct {
		name string
		conn api.EndpointConnectivity
	}{
		{"unknown mode", api.EndpointConnectivity{Mode: &bogus}},
		{"CIDR with no prefix length", api.EndpointConnectivity{Mode: &external, AllowedCidrs: &bare}},
	} {
		got, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
			Name: "c-" + tc.name, WithWriter: &withWriter, Password: strptr(testPassword), Connectivity: &tc.conn,
		})
		if err != nil {
			t.Fatalf("%s: CreateCluster: %v", tc.name, err)
		}
		if got.StatusCode() != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body = %s", tc.name, got.StatusCode(), got.Body)
		}
	}
}
