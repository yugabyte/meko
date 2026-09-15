package api

import (
	"strings"
	"testing"

	"github.com/yugabyte/amp/controller/internal/deploy"
)

func ptr[T any](v T) *T { return &v }

// connOK is a deployer that renders the load-balancer Service, as the Kubernetes
// deployer does; connNo is one that does not (docker, local).
type connOK struct{ deploy.Deployer }

func (connOK) SupportsConnectivity() bool { return true }

type connNo struct{ deploy.Deployer }

// ingressOK is a Kubernetes deployer with shared ingress turned on AND able to
// place an endpoint on a shard — the only configuration in which an ingress mode
// is usable. connOK is the same driver with the switch off, which is every
// deployment today; ingressNoPlacement has the switch on but no placement, the
// window this series passes through. The three together cover both gates rather
// than only the driver.
type ingressOK struct{ deploy.Deployer }

func (ingressOK) SupportsConnectivity() bool { return true }
func (ingressOK) SupportsIngress() bool      { return true }
func (ingressOK) PlacesIngress() bool        { return true }

type ingressNoPlacement struct{ deploy.Deployer }

func (ingressNoPlacement) SupportsConnectivity() bool { return true }
func (ingressNoPlacement) SupportsIngress() bool      { return true }

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
		deploy.ConnectivityIngressInternal,
		deploy.ConnectivityIngressExternal,
	} {
		mode, _, err := endpointConnectivity(&EndpointConnectivity{Mode: ptr(EndpointConnectivityMode(want))}, ingressOK{})
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

// The ingress modes need the deployment to have opted in, not just the right
// driver. Refused here rather than at deploy time so the caller learns now — and
// refused rather than quietly downgraded to a dedicated load balancer, which
// would put the per-endpoint bill back without anyone choosing it.
func TestEndpointConnectivityRejectsIngressWithoutSharedIngress(t *testing.T) {
	for _, mode := range []string{deploy.ConnectivityIngressInternal, deploy.ConnectivityIngressExternal} {
		// The Kubernetes driver, but the feature switched off: every deployment today.
		_, _, err := endpointConnectivity(
			&EndpointConnectivity{Mode: ptr(EndpointConnectivityMode(mode))}, connOK{})
		if err == nil {
			t.Fatalf("mode %q must be rejected when the deployment has no shared ingress", mode)
		}
		if err.Error.Code != "unsupported_connectivity" {
			t.Errorf("code = %q, want unsupported_connectivity", err.Error.Code)
		}
		// A driver that cannot render the route at all is refused too.
		if _, _, err := endpointConnectivity(
			&EndpointConnectivity{Mode: ptr(EndpointConnectivityMode(mode))}, connNo{}); err == nil {
			t.Errorf("mode %q must be rejected on a deployer that cannot render an ingress route", mode)
		}
	}
	// The nlb_* modes must not start depending on the ingress switch.
	for _, mode := range []string{deploy.ConnectivityNLBInternal, deploy.ConnectivityNLBExternal} {
		if _, _, err := endpointConnectivity(
			&EndpointConnectivity{Mode: ptr(EndpointConnectivityMode(mode))}, connOK{}); err != nil {
			t.Errorf("mode %q must stay valid without shared ingress, got %v", mode, err)
		}
	}
}

// An ingress endpoint shares its load balancer, so loadBalancerSourceRanges — a
// per-Service field — cannot express a per-endpoint allowlist. Refusing is the
// point: silently dropping one is how an endpoint ends up reachable from more
// than the caller asked for, with no way for them to tell.
func TestEndpointConnectivityRejectsCIDRsWithIngress(t *testing.T) {
	for _, mode := range []string{deploy.ConnectivityIngressInternal, deploy.ConnectivityIngressExternal} {
		_, _, err := endpointConnectivity(&EndpointConnectivity{
			Mode:         ptr(EndpointConnectivityMode(mode)),
			AllowedCidrs: &[]string{"10.0.0.0/8"},
		}, ingressOK{})
		if err == nil {
			t.Fatalf("allowed_cidrs with %q must be refused, not accepted and ignored", mode)
		}
		if err.Error.Code != "invalid_request" {
			t.Errorf("code = %q, want invalid_request", err.Error.Code)
		}
	}
	// Without an allowlist the same modes are fine.
	if _, _, err := endpointConnectivity(&EndpointConnectivity{
		Mode: ptr(EndpointConnectivityMode(deploy.ConnectivityIngressInternal)),
	}, ingressOK{}); err != nil {
		t.Errorf("ingress_internal without allowed_cidrs must be accepted, got %v", err)
	}
	// And the allowlist still works where it is actually implemented.
	if _, cidrs, err := endpointConnectivity(&EndpointConnectivity{
		Mode:         ptr(EndpointConnectivityMode(deploy.ConnectivityNLBInternal)),
		AllowedCidrs: &[]string{"10.0.0.0/8"},
	}, ingressOK{}); err != nil || len(cidrs) != 1 {
		t.Errorf("nlb_internal with allowed_cidrs = (%v, %v), want it kept", cidrs, err)
	}
}

// The switch being on is not enough: until the deploy path can place an endpoint
// on a shard, accepting the mode would record an exposure the endpoint does not
// have — the create succeeds, GET reports ingress_internal, and the address is
// the ordinary in-cluster one. This is the same interlock ConnectivitySupporter
// provides for the nlb_* modes, and it is why the API opens to these modes when
// placement is wired rather than when the flag exists.
func TestEndpointConnectivityRejectsIngressUntilPlacementIsWired(t *testing.T) {
	for _, mode := range []string{deploy.ConnectivityIngressInternal, deploy.ConnectivityIngressExternal} {
		_, _, err := endpointConnectivity(
			&EndpointConnectivity{Mode: ptr(EndpointConnectivityMode(mode))}, ingressNoPlacement{})
		if err == nil {
			t.Fatalf("mode %q must be refused while the deploy path cannot place it", mode)
		}
		if err.Error.Code != "unsupported_connectivity" {
			t.Errorf("code = %q, want unsupported_connectivity", err.Error.Code)
		}
		// The two refusals must stay distinguishable: the operator's next step is
		// "turn the switch on" in one case and "you cannot yet" in the other.
		if !strings.Contains(err.Error.Message, "not wired up") {
			t.Errorf("message %q does not distinguish missing placement from the switch being off",
				err.Error.Message)
		}
		// And with placement wired, the same mode is accepted.
		if _, _, err := endpointConnectivity(
			&EndpointConnectivity{Mode: ptr(EndpointConnectivityMode(mode))}, ingressOK{}); err != nil {
			t.Errorf("mode %q must be accepted once placement is wired, got %v", mode, err)
		}
	}
}
