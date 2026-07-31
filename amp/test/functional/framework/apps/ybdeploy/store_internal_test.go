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
