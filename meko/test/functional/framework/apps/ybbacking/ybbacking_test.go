package ybbacking

import (
	"fmt"
	"testing"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/pg"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/yba"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/yugabyte"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/yugabyted"
)

// TestFromYBK8sSplitsCoordinatorAndInClusterPaths: a co-located store exposes
// the tailnet endpoint as ConnInfo/HostPortsCSV (coordinator-reachable) and the
// in-cluster Service DNS as InClusterHostPortsCSV.
func TestFromYBK8sSplitsCoordinatorAndInClusterPaths(t *testing.T) {
	conn := pg.ConnInfo{Host: "10.1.2.3", Port: 5433, User: "yugabyte", Database: "yugabyte", SSLMode: "disable"}
	bs := FromYBK8s(conn, []string{"10.1.2.3:5433", "10.1.2.4:5433"},
		[]string{"yb-tserver-service.mekoqa-yb-run7.svc.cluster.local:5433"})

	if bs.ConnInfo().Host != "10.1.2.3" {
		t.Errorf("ConnInfo = %+v, want the tailnet primary", bs.ConnInfo())
	}
	if got := bs.HostPortsCSV(); got != "10.1.2.3:5433,10.1.2.4:5433" {
		t.Errorf("HostPortsCSV = %q, want the tailnet CSV", got)
	}
	if got := bs.InClusterHostPortsCSV(); got != "yb-tserver-service.mekoqa-yb-run7.svc.cluster.local:5433" {
		t.Errorf("InClusterHostPortsCSV = %q, want the in-cluster DNS", got)
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

// A YBA KUBERNETES universe reports its tservers at pod DNS names. Those are
// unresolvable from outside the cluster and the pod IPs behind them are not
// reliably routable either, so the store must carry the kube location that puts
// callers on the exec/forward path instead of the dial path.
func TestFromYBAUniverseK8sIsColocated(t *testing.T) {
	const dns = "ybmekoqa-1-yb-tserver-0.ybmekoqa-1-yb-tservers.yb-dev-mekoqa-1.svc.cluster.local"
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
	if ns, pod := col.KubeColocation(); ns != "yb-dev-mekoqa-1" || pod != "ybmekoqa-1-yb-tserver-0" {
		t.Errorf("kube location = %q/%q, want yb-dev-mekoqa-1/ybmekoqa-1-yb-tserver-0", ns, pod)
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

// The yb_k8s deploy mode is in a cluster too, it must reach its backing the
// same way, without anyone passing the location in separately.
func TestFromYBK8sCarriesKubeLocation(t *testing.T) {
	col := FromYBK8s(
		pg.ConnInfo{Host: "10.21.140.9", Port: 5433},
		[]string{"10.21.140.9:5433"},
		[]string{"yb-tserver-0.yb-tservers.mekoqa-yb-7.svc.cluster.local:5433"},
	)
	if ns, pod := col.KubeColocation(); ns != "mekoqa-yb-7" || pod != "yb-tserver-0" {
		t.Errorf("kube location = %q/%q, want mekoqa-yb-7/yb-tserver-0", ns, pod)
	}
}

// A kubernetes universe's tserver DNS name carries where to exec admin commands.
// Without it the coordinator dials the universe, and where the pod CIDR is
// shadowed by another advertised tailnet route that dial times out against a
// healthy universe.
func TestKubeLocationFromDNS(t *testing.T) {
	const addr = "ybmekoqa-9225-gin2-yb-tserver-0.ybmekoqa-9225-gin2-yb-tservers.yb-dev-mekoqa-9225.svc.cluster.local:5433"
	ns, pod := KubeLocationFromDNS(addr)
	if ns != "yb-dev-mekoqa-9225" {
		t.Errorf("namespace = %q, want yb-dev-mekoqa-9225", ns)
	}
	if pod != "ybmekoqa-9225-gin2-yb-tserver-0" {
		t.Errorf("pod = %q, want the tserver pod", pod)
	}
	// Anything that is not a pod-qualified in-cluster name must leave the caller
	// on the dial path rather than inventing a namespace: a VM universe's node
	// address is dialable and has no pod to exec into.
	for _, other := range []string{"10.150.8.50:5433", "yb-tservers.ns.svc.cluster.local:5433", "", "host:5433"} {
		if ns, pod := KubeLocationFromDNS(other); ns != "" || pod != "" {
			t.Errorf("KubeLocationFromDNS(%q) = %q/%q, want empty", other, ns, pod)
		}
	}
}

func TestServiceFromPodDNS(t *testing.T) {
	for addr, want := range map[string]string{
		"yb-tserver-0.yb-tservers.mekoqa-yb-7.svc.cluster.local:5433":                      "yb-tservers.mekoqa-yb-7.svc.cluster.local.:5433",
		"ybmekoqa-1-yb-tserver-0.ybmekoqa-1-yb-tservers.yb-dev-mekoqa-1.svc.cluster.local": "ybmekoqa-1-yb-tservers.yb-dev-mekoqa-1.svc.cluster.local.:0",
	} {
		host, port, ok := ServiceFromPodDNS(addr)
		if !ok || fmt.Sprintf("%s:%d", host, port) != want {
			t.Errorf("ServiceFromPodDNS(%q) = %s:%d %v, want %s", addr, host, port, ok, want)
		}
	}
	for _, addr := range []string{"10.1.2.3:5433", "yb-tservers.ns.svc.cluster.local:5433", ""} {
		if _, _, ok := ServiceFromPodDNS(addr); ok {
			t.Errorf("ServiceFromPodDNS(%q) claimed a pod name", addr)
		}
	}
}
