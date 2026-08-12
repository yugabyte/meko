package ybbacking

import (
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/yba"
	"github.com/yugabyte/amp/test/functional/framework/apps/yugabyte"
	"github.com/yugabyte/amp/test/functional/framework/apps/yugabyted"
)

// TestFromYBK8sSplitsCoordinatorAndInClusterPaths: a co-located store exposes
// the tailnet endpoint as ConnInfo/HostPortsCSV (coordinator-reachable) and the
// in-cluster Service DNS as ControllerHostPortsCSV.
func TestFromYBK8sSplitsCoordinatorAndInClusterPaths(t *testing.T) {
	conn := pg.ConnInfo{Host: "10.1.2.3", Port: 5433, User: "yugabyte", Database: "yugabyte", SSLMode: "disable"}
	bs := FromYBK8s(conn, []string{"10.1.2.3:5433", "10.1.2.4:5433"},
		[]string{"yb-tserver-service.ampqa-yb-run7.svc.cluster.local:5433"})

	if bs.ConnInfo().Host != "10.1.2.3" {
		t.Errorf("ConnInfo = %+v, want the tailnet primary", bs.ConnInfo())
	}
	if got := bs.HostPortsCSV(); got != "10.1.2.3:5433,10.1.2.4:5433" {
		t.Errorf("HostPortsCSV = %q, want the tailnet CSV", got)
	}
	if got := bs.ControllerHostPortsCSV(); got != "yb-tserver-service.ampqa-yb-run7.svc.cluster.local:5433" {
		t.Errorf("ControllerHostPortsCSV = %q, want the in-cluster DNS", got)
	}
}

// TestExternalBackingColocatedRoutesTwoPaths: mapping a co-located store into
// ampstack.Backing points the CONTROLLER at the in-cluster DNS while the
// one-shot yb_storage CREATE (AdminHostPorts) runs over the tailnet CSV.
func TestExternalBackingColocatedRoutesTwoPaths(t *testing.T) {
	conn := pg.ConnInfo{Host: "10.1.2.3", Port: 5433, User: "yugabyte", SSLMode: "disable"}
	bs := FromYBK8s(conn, []string{"10.1.2.3:5433", "10.1.2.4:5433"},
		[]string{"yb-tserver-service.ampqa-yb-run7.svc.cluster.local:5433"})

	b := ExternalBacking(bs)
	if b.Mode != ampstack.BackingExternal {
		t.Errorf("Mode = %q, want external", b.Mode)
	}
	if b.HostPorts != "yb-tserver-service.ampqa-yb-run7.svc.cluster.local:5433" {
		t.Errorf("HostPorts = %q, want the in-cluster DNS (amp controller path)", b.HostPorts)
	}
	if b.AdminHostPorts != "10.1.2.3:5433,10.1.2.4:5433" {
		t.Errorf("AdminHostPorts = %q, want the tailnet CSV (coordinator create path)", b.AdminHostPorts)
	}
	if b.User != "yugabyte" {
		t.Errorf("User = %q, want yugabyte", b.User)
	}
	if b.DBName != "" {
		t.Errorf("DBName = %q, want empty (ampstack defaults it)", b.DBName)
	}
}

// TestExternalBackingNonColocatedLeavesAdminEmpty: a plain (non-co-located)
// store maps HostPorts straight through with no AdminHostPorts split.
func TestExternalBackingNonColocatedLeavesAdminEmpty(t *testing.T) {
	app := yugabyted.New([]yugabyted.Node{{Advertise: "10.0.0.1"}}, yugabyted.Spec{})
	b := ExternalBacking(FromYugabyted(app))
	if b.HostPorts != "10.0.0.1:5433" {
		t.Errorf("HostPorts = %q", b.HostPorts)
	}
	if b.AdminHostPorts != "" {
		t.Errorf("AdminHostPorts = %q, want empty for a non-co-located store", b.AdminHostPorts)
	}
}

func TestFromYugabytedMapsAllNodes(t *testing.T) {
	app := yugabyted.New([]yugabyted.Node{
		{Advertise: "10.0.0.1"},
		{Advertise: "10.0.0.2"},
		{Advertise: "10.0.0.3"},
	}, yugabyted.Spec{})

	bs := FromYugabyted(app)
	if got := bs.HostPortsCSV(); got != "10.0.0.1:5433,10.0.0.2:5433,10.0.0.3:5433" {
		t.Errorf("HostPortsCSV = %q", got)
	}
	if ci := bs.ConnInfo(); ci.Host != "10.0.0.1" || ci.Port != 5433 || ci.User != "yugabyte" {
		t.Errorf("ConnInfo = %+v", ci)
	}
	if bs.Describe() == "" {
		t.Error("Describe should be non-empty")
	}
}

func TestFromYugabyteMapsEveryTserver(t *testing.T) {
	app := yugabyte.New([]yugabyte.Node{
		{Advertise: "n1"},
		{Advertise: "n2"},
		{Advertise: "n3"},
	}, yugabyte.Spec{YSQLPort: 6000})

	bs := FromYugabyte(app)
	if got := bs.HostPortsCSV(); got != "n1:6000,n2:6000,n3:6000" {
		t.Errorf("HostPortsCSV = %q", got)
	}
	if ci := bs.ConnInfo(); ci.Host != "n1" || ci.Port != 6000 {
		t.Errorf("ConnInfo = %+v", ci)
	}
}

func TestFromYBAUniverseUsesLiveTservers(t *testing.T) {
	u := yba.Universe{
		Name: "uni",
		UniverseDetails: yba.UniverseDetails{
			NodeDetailsSet: []yba.NodeDetails{
				{IsTserver: true, State: "Live", CloudInfo: yba.CloudInfo{PrivateIP: "10.1.0.1"}},
				{IsTserver: true, State: "Live", CloudInfo: yba.CloudInfo{PrivateIP: "10.1.0.2"}},
				{IsTserver: false, State: "Live", CloudInfo: yba.CloudInfo{PrivateIP: "10.1.0.9"}}, // master-only, skipped
			},
		},
	}
	bs, err := FromYBAUniverse(u)
	if err != nil {
		t.Fatalf("FromYBAUniverse: %v", err)
	}
	if got := bs.HostPortsCSV(); got != "10.1.0.1:5433,10.1.0.2:5433" {
		t.Errorf("HostPortsCSV = %q", got)
	}
	if ci := bs.ConnInfo(); ci.Host != "10.1.0.1" {
		t.Errorf("ConnInfo primary = %+v", ci)
	}
}

func TestFromYBAUniverseNoTserverErrors(t *testing.T) {
	if _, err := FromYBAUniverse(yba.Universe{Name: "empty"}); err == nil {
		t.Error("a universe with no live tserver must error")
	}
}

func TestExternalBackingMapsIntoAmpstack(t *testing.T) {
	app := yugabyted.New([]yugabyted.Node{{Advertise: "10.0.0.1"}, {Advertise: "10.0.0.2"}}, yugabyted.Spec{})
	b := ExternalBacking(FromYugabyted(app))

	if b.Mode != ampstack.BackingExternal {
		t.Errorf("Mode = %q, want external", b.Mode)
	}
	if b.HostPorts != "10.0.0.1:5433,10.0.0.2:5433" {
		t.Errorf("HostPorts = %q", b.HostPorts)
	}
	if b.User != "yugabyte" {
		t.Errorf("User = %q, want yugabyte", b.User)
	}
	// DBName left empty so ampstack applies its yb_storage default — the cluster
	// ConnInfo.Database ("yugabyte") is the maintenance DB, not the amp store.
	if b.DBName != "" {
		t.Errorf("DBName = %q, want empty (ampstack defaults it)", b.DBName)
	}
}

// A YBA KUBERNETES universe reports its tservers at pod DNS names. Those are
// unresolvable from outside the cluster and the pod IPs behind them are not
// reliably routable either, so the store must carry the kube location that puts
// ampstack on the exec/forward path instead of the dial path.
func TestFromYBAUniverseK8sIsColocated(t *testing.T) {
	const dns = "ybampqa-1-yb-tserver-0.ybampqa-1-yb-tservers.yb-dev-ampqa-1.svc.cluster.local"
	u := yba.Universe{
		Name: "uni-k8s",
		UniverseDetails: yba.UniverseDetails{
			NodeDetailsSet: []yba.NodeDetails{
				{IsTserver: true, State: "Live", CloudInfo: yba.CloudInfo{PrivateIP: dns}},
			},
		},
	}
	bs, err := FromYBAUniverse(u)
	if err != nil {
		t.Fatalf("FromYBAUniverse: %v", err)
	}
	col, ok := bs.(Colocated)
	if !ok {
		t.Fatalf("store type = %T, want Colocated", bs)
	}
	if ns, pod := col.KubeColocation(); ns != "yb-dev-ampqa-1" || pod != "ybampqa-1-yb-tserver-0" {
		t.Errorf("kube location = %q/%q, want yb-dev-ampqa-1/ybampqa-1-yb-tserver-0", ns, pod)
	}
	b := ExternalBacking(bs)
	if b.KubeNamespace != "yb-dev-ampqa-1" || b.KubePod != "ybampqa-1-yb-tserver-0" {
		t.Errorf("ampstack.Backing lost the location: %+v", b)
	}
}

// A VM universe's node addresses are reachable both ways: no location, no
// behaviour change.
func TestFromYBAUniverseVMStaysStatic(t *testing.T) {
	u := yba.Universe{
		Name: "uni-vm",
		UniverseDetails: yba.UniverseDetails{
			NodeDetailsSet: []yba.NodeDetails{
				{IsTserver: true, State: "Live", CloudInfo: yba.CloudInfo{PrivateIP: "10.1.0.1"}},
			},
		},
	}
	bs, err := FromYBAUniverse(u)
	if err != nil {
		t.Fatalf("FromYBAUniverse: %v", err)
	}
	if _, ok := bs.(Static); !ok {
		t.Errorf("store type = %T, want Static", bs)
	}
}

// The yb_k8s deploy mode is in a cluster too — it must reach its backing the
// same way, without anyone passing the location in separately.
func TestFromYBK8sCarriesKubeLocation(t *testing.T) {
	col := FromYBK8s(
		pg.ConnInfo{Host: "10.21.140.9", Port: 5433},
		[]string{"10.21.140.9:5433"},
		[]string{"yb-tserver-0.yb-tservers.ampqa-yb-7.svc.cluster.local:5433"},
	)
	if ns, pod := col.KubeColocation(); ns != "ampqa-yb-7" || pod != "yb-tserver-0" {
		t.Errorf("kube location = %q/%q, want ampqa-yb-7/yb-tserver-0", ns, pod)
	}
}
