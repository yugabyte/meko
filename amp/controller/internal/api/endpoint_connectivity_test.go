package api

import (
	"testing"

	"github.com/yugabyte/amp/controller/internal/deploy"
)

func ptr[T any](v T) *T { return &v }

// connOK is a deployer that renders the load-balancer Service, as the Kubernetes
// deployer does; connNo is one that does not (docker, local).
type connOK struct{ deploy.Deployer }

func (connOK) SupportsConnectivity() bool { return true }

type connNo struct{ deploy.Deployer }

// A nil block is what every caller that predates this option sends, so it must
// mean cluster_ip rather than an error.
func TestEndpointConnectivityDefaultsToClusterIP(t *testing.T) {
	mode, cidrs, err := endpointConnectivity(nil, connOK{})
	if err != nil {
		t.Fatalf("nil connectivity must be accepted, got %v", err)
	}
	if mode != "" || cidrs != nil {
		t.Errorf("nil connectivity = (%q, %v), want (\"\", nil) so the deployer takes its default", mode, cidrs)
	}
}

func TestEndpointConnectivityAcceptsEveryMode(t *testing.T) {
	for _, want := range []string{
		deploy.ConnectivityClusterIP,
		deploy.ConnectivityNLBInternal,
		deploy.ConnectivityNLBExternal,
	} {
		mode, _, err := endpointConnectivity(&EndpointConnectivity{Mode: ptr(EndpointConnectivityMode(want))}, connOK{})
		if err != nil {
			t.Errorf("mode %q must be accepted, got %v", want, err)
			continue
		}
		if mode != want {
			t.Errorf("mode %q round-tripped as %q", want, mode)
		}
	}
}

func TestEndpointConnectivityRejectsUnknownMode(t *testing.T) {
	_, _, err := endpointConnectivity(&EndpointConnectivity{Mode: ptr(EndpointConnectivityMode("public"))}, connOK{})
	if err == nil {
		t.Fatal("an unknown mode must be a 400, not a task that fails minutes later")
	}
	if err.Error.Code != "invalid_request" {
		t.Errorf("code = %q, want invalid_request", err.Error.Code)
	}
}

// A bare IP is the mistake callers actually make, and AWS rejects it at the
// Service — so it has to fail here, where the message can say why.
func TestEndpointConnectivityRejectsBareIP(t *testing.T) {
	_, _, err := endpointConnectivity(&EndpointConnectivity{
		Mode:         ptr(EndpointConnectivityMode(deploy.ConnectivityNLBExternal)),
		AllowedCidrs: &[]string{"96.33.50.4"},
	}, connOK{})
	if err == nil {
		t.Fatal("a bare IP with no prefix length must be rejected")
	}
	if got := err.Error.Message; got == "" {
		t.Error("the error must explain that a prefix length is required")
	}
}

func TestEndpointConnectivityKeepsValidCIDRs(t *testing.T) {
	_, cidrs, err := endpointConnectivity(&EndpointConnectivity{
		Mode:         ptr(EndpointConnectivityMode(deploy.ConnectivityNLBInternal)),
		AllowedCidrs: &[]string{"10.0.0.0/16", " 96.33.50.4/32 ", ""},
	}, connOK{})
	if err != nil {
		t.Fatalf("valid CIDRs must be accepted, got %v", err)
	}
	// Whitespace trimmed, empty entries dropped — the list goes straight into
	// loadBalancerSourceRanges, where either would be rejected by the apiserver.
	want := []string{"10.0.0.0/16", "96.33.50.4/32"}
	if len(cidrs) != len(want) {
		t.Fatalf("cidrs = %v, want %v", cidrs, want)
	}
	for i := range want {
		if cidrs[i] != want[i] {
			t.Errorf("cidrs[%d] = %q, want %q", i, cidrs[i], want[i])
		}
	}
}

// Source ranges only exist on a load balancer, so pairing them with cluster_ip
// is a request the caller has misunderstood — silently dropping them would hand
// back an endpoint with none of the restriction they asked for.
func TestEndpointConnectivityRejectsCIDRsWithClusterIP(t *testing.T) {
	_, _, err := endpointConnectivity(&EndpointConnectivity{
		Mode:         ptr(EndpointConnectivityMode(deploy.ConnectivityClusterIP)),
		AllowedCidrs: &[]string{"10.0.0.0/16"},
	}, connOK{})
	if err == nil {
		t.Fatal("allowed_cidrs with cluster_ip must be rejected rather than ignored")
	}
}

// The docker and local deployers ignore EndpointSpec.Connectivity, so accepting
// an nlb_* mode there would persist — and report back on GET — an exposure the
// endpoint does not have.
func TestEndpointConnectivityRejectsNLBOnDeployerWithoutSupport(t *testing.T) {
	for _, mode := range []string{deploy.ConnectivityNLBInternal, deploy.ConnectivityNLBExternal} {
		_, _, err := endpointConnectivity(
			&EndpointConnectivity{Mode: ptr(EndpointConnectivityMode(mode))}, connNo{})
		if err == nil {
			t.Errorf("mode %q must be rejected on a deployer that cannot provision a load balancer", mode)
		}
	}
	// cluster_ip needs no load balancer, so it stays valid on every driver.
	if _, _, err := endpointConnectivity(
		&EndpointConnectivity{Mode: ptr(EndpointConnectivityMode(deploy.ConnectivityClusterIP))}, connNo{}); err != nil {
		t.Errorf("cluster_ip must remain valid on every driver, got %v", err)
	}
}
