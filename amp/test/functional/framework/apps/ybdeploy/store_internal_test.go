package ybdeploy

import (
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybbacking"
)

// TestYBAProvisionedBackingK8sSplit: a kubernetes universe (non-empty
// inClusterAddrs) yields a co-located store whose ExternalBacking wires the amp
// controller/endpoints at the IN-CLUSTER addresses and reserves the tailnet pod
// IPs for the coordinator's one-shot yb_storage CREATE — the split a plain
// Static (which would hand the in-cluster controller the pod IPs) does not draw.
func TestYBAProvisionedBackingK8sSplit(t *testing.T) {
	conn := pg.ConnInfo{Host: "100.64.0.1", Port: 5433, User: "yugabyte"}
	podIPs := []string{"100.64.0.1:5433", "100.64.0.2:5433"}
	inCluster := []string{"10.2.0.1:5433", "10.2.0.2:5433"}

	store := ybaProvisionedBacking(conn, podIPs, inCluster)
	if _, ok := store.(ybbacking.Colocated); !ok {
		t.Fatalf("k8s store type = %T, want ybbacking.Colocated", store)
	}

	b := ybbacking.ExternalBacking(store)
	if b.Mode != ampstack.BackingExternal {
		t.Errorf("Mode = %q, want external", b.Mode)
	}
	if b.HostPorts != strings.Join(inCluster, ",") {
		t.Errorf("HostPorts = %q, want the in-cluster addrs %q (controller/endpoints dial in-cluster)", b.HostPorts, strings.Join(inCluster, ","))
	}
	if b.AdminHostPorts != strings.Join(podIPs, ",") {
		t.Errorf("AdminHostPorts = %q, want the pod IPs %q (coordinator create over the tailnet)", b.AdminHostPorts, strings.Join(podIPs, ","))
	}
}

// TestYBAProvisionedBackingVMStatic: a VM universe (empty inClusterAddrs — its
// node IPs are reachable both ways) yields a plain Static: HostPorts is the node
// addrs and AdminHostPorts is left empty (the coordinator create falls back to
// HostPorts).
func TestYBAProvisionedBackingVMStatic(t *testing.T) {
	conn := pg.ConnInfo{Host: "10.0.0.5", Port: 5433, User: "yugabyte"}
	addrs := []string{"10.0.0.5:5433", "10.0.0.6:5433"}

	store := ybaProvisionedBacking(conn, addrs, nil)
	if _, ok := store.(ybbacking.Static); !ok {
		t.Fatalf("VM store type = %T, want ybbacking.Static", store)
	}

	b := ybbacking.ExternalBacking(store)
	if b.HostPorts != strings.Join(addrs, ",") {
		t.Errorf("HostPorts = %q, want the node addrs %q", b.HostPorts, strings.Join(addrs, ","))
	}
	if b.AdminHostPorts != "" {
		t.Errorf("AdminHostPorts = %q, want empty (VM addrs reachable both ways)", b.AdminHostPorts)
	}
}

// A kubernetes universe's tserver DNS name carries where to exec the yb_storage
// CREATE. Getting this wrong is not a cosmetic miss: without it ampstack dials
// the universe from the coordinator, and where the pod CIDR is shadowed by
// another advertised tailnet route that dial times out against a healthy RF=3
// universe (run 98bbfdef: "connect 10.21.137.4:5433 … dial error: timeout").
func TestKubeLocationFromDNS(t *testing.T) {
	const addr = "ybampqa-9225-gin2-yb-tserver-0.ybampqa-9225-gin2-yb-tservers.yb-dev-ampqa-9225.svc.cluster.local:5433"
	ns, pod := ybbacking.KubeLocationFromDNS(addr)
	if ns != "yb-dev-ampqa-9225" {
		t.Errorf("namespace = %q, want yb-dev-ampqa-9225", ns)
	}
	if pod != "ybampqa-9225-gin2-yb-tserver-0" {
		t.Errorf("pod = %q, want the tserver pod", pod)
	}
	// Anything that is not a pod-qualified in-cluster name must leave the caller
	// on the dial path rather than inventing a namespace: a VM universe's node
	// address is dialable and has no pod to exec into.
	for _, other := range []string{"10.150.8.50:5433", "yb-tservers.ns.svc.cluster.local:5433", "", "host:5433"} {
		if ns, pod := ybbacking.KubeLocationFromDNS(other); ns != "" || pod != "" {
			t.Errorf("KubeLocationFromDNS(%q) = %q/%q, want empty", other, ns, pod)
		}
	}
}

// The co-located store must carry that location through to ampstack.Backing, or
// the exec path is never taken.
func TestYBAProvisionedBackingCarriesKubeLocation(t *testing.T) {
	conn := pg.ConnInfo{Host: "10.21.137.4", Port: 5433, User: "yugabyte", Database: "yugabyte"}
	inCluster := []string{
		"u-yb-tserver-0.u-yb-tservers.yb-dev-u.svc.cluster.local:5433",
		"u-yb-tserver-1.u-yb-tservers.yb-dev-u.svc.cluster.local:5433",
	}
	b := ybbacking.ExternalBacking(ybaProvisionedBacking(conn, []string{"10.21.137.4:5433"}, inCluster))
	if b.KubeNamespace != "yb-dev-u" || b.KubePod != "u-yb-tserver-0" {
		t.Errorf("backing kube location = %q/%q, want yb-dev-u/u-yb-tserver-0", b.KubeNamespace, b.KubePod)
	}
	// A VM universe has nothing to exec into and must stay on the dial path.
	vm := ybbacking.ExternalBacking(ybaProvisionedBacking(conn, []string{"10.150.8.50:5433"}, nil))
	if vm.KubeNamespace != "" || vm.KubePod != "" {
		t.Errorf("a VM universe must not claim a kube location, got %q/%q", vm.KubeNamespace, vm.KubePod)
	}
}
