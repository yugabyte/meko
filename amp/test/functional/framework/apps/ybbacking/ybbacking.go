// Package ybbacking adapts the framework's YugabyteDB cluster apps
// (apps/yugabyted, apps/yugabyte, apps/yba) into a single BackingStore the amp
// control plane can sit on as its EXTERNAL backing store. It is the seam the
// foundation env uses to hand ampstack a YugabyteDB produced by any of those
// apps: each adapter maps a cluster's connection into a primary YSQL ConnInfo
// plus the full CSV of tserver host:port addresses, and ExternalBacking turns
// that into an ampstack.Backing (external mode) config.
//
// The adapters are pure/tiny — no lifecycle, no I/O — so they compose with the
// live cluster apps without coupling ampstack to any of them (ampstack imports
// none of these packages; this one imports all of them).
package ybbacking

import (
	"fmt"
	"net"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/yba"
	"github.com/yugabyte/amp/test/functional/framework/apps/yugabyte"
	"github.com/yugabyte/amp/test/functional/framework/apps/yugabyted"
)

// BackingStore is a resolved YugabyteDB backing store: a primary YSQL ConnInfo,
// the full CSV of tserver host:port addresses, and a human description of where
// it came from.
type BackingStore interface {
	ConnInfo() pg.ConnInfo
	HostPortsCSV() string
	Describe() string
}

// Static is a concrete BackingStore built from already-resolved values (the
// only implementation the adapters produce).
type Static struct {
	Conn  pg.ConnInfo
	Addrs []string
	Desc  string
}

// ConnInfo is the primary YSQL target.
func (s Static) ConnInfo() pg.ConnInfo { return s.Conn }

// HostPortsCSV is the comma-separated tserver host:port list (YB_HOST_PORTS).
func (s Static) HostPortsCSV() string { return strings.Join(s.Addrs, ",") }

// Describe is a one-line human summary.
func (s Static) Describe() string { return s.Desc }

var _ BackingStore = Static{}

// FromYugabyted adapts a yugabyted cluster app: its ConnInfo as the primary
// target and every node's YSQL address as the host-ports list.
func FromYugabyted(app *yugabyted.App) Static {
	addrs := app.YSQLAddrs()
	ci := app.ConnInfo()
	return Static{
		Conn:  ci,
		Addrs: addrs,
		Desc:  fmt.Sprintf("yugabyted cluster (%d nodes, primary %s)", len(addrs), ci.Addr()),
	}
}

// FromYugabyte adapts a raw yugabyte cluster app: its ConnInfo as the primary
// target and every node's tserver YSQL address as the host-ports list.
func FromYugabyte(app *yugabyte.App) Static {
	addrs := app.YSQLAddrs()
	ci := app.ConnInfo()
	return Static{
		Conn:  ci,
		Addrs: addrs,
		Desc:  fmt.Sprintf("yugabyte cluster (%d tservers, primary %s)", len(addrs), ci.Addr()),
	}
}

// Colocated is a BackingStore for the co-located YB-in-k8s mode (apps/ybk8s):
// the embedded Static carries the COORDINATOR-reachable (tailnet) endpoint the
// framework's layer-1 workload and the one-shot amp storage-DB create use,
// while ControllerAddrs is the IN-CLUSTER Service DNS the amp controller +
// endpoints connect at — so amp↔YB traffic never leaves the cluster. The
// coordinator cannot resolve an in-cluster .svc.cluster.local name, hence the
// two distinct paths.
type Colocated struct {
	Static
	ControllerAddrs []string
	// KubeNamespace / KubePod locate a tserver of this backing in the cluster,
	// so the one-shot yb_storage CREATE can be exec'd where the database lives
	// instead of dialled from the coordinator. Empty leaves the dial path.
	KubeNamespace string
	KubePod       string
}

// KubeColocation reports where to exec into this backing, and is what
// ExternalBacking reads to route the CREATE through kubectl.
func (c Colocated) KubeColocation() (namespace, pod string) { return c.KubeNamespace, c.KubePod }

// ControllerHostPortsCSV is the in-cluster host:port CSV the amp controller +
// endpoints connect at (ExternalBacking routes it into ampstack.Backing).
func (c Colocated) ControllerHostPortsCSV() string { return strings.Join(c.ControllerAddrs, ",") }

var _ BackingStore = Colocated{}

// FromYBK8s adapts a co-located YugabyteDB-in-k8s deploy: tailnetConn/tailnetAddrs
// are the coordinator-reachable (tailnet) endpoint, inClusterAddrs are the
// in-cluster Service DNS the amp controller connects at.
func FromYBK8s(tailnetConn pg.ConnInfo, tailnetAddrs, inClusterAddrs []string) Colocated {
	ns, pod := KubeLocationFromDNS(first(inClusterAddrs))
	return Colocated{
		Static: Static{
			Conn:  tailnetConn,
			Addrs: tailnetAddrs,
			Desc:  fmt.Sprintf("co-located YB-in-k8s (%d tservers, primary %s, amp via %s)", len(tailnetAddrs), tailnetConn.Addr(), strings.Join(inClusterAddrs, ",")),
		},
		ControllerAddrs: inClusterAddrs,
		KubeNamespace:   ns,
		KubePod:         pod,
	}
}

// FromYBAUniverse adapts a YBA universe: its first live tserver as the primary
// target and every live tserver as the host-ports list (via yba.YSQLConnInfo).
// A universe with no live tserver is an error.
//
// A kubernetes universe reports its tservers at in-cluster pod DNS names, so it
// yields a Colocated carrying that location: those names are not resolvable
// from here, and the pod IPs behind them are not necessarily routable either,
// so everything this process needs from the universe goes through kubectl. A VM
// universe's addresses are reachable both ways and stay a plain Static.
func FromYBAUniverse(u yba.Universe) (BackingStore, error) {
	ci, addrs, err := yba.YSQLConnInfo(u)
	if err != nil {
		return nil, err
	}
	static := Static{
		Conn:  ci,
		Addrs: addrs,
		Desc:  fmt.Sprintf("YBA universe %q (%d live tservers, primary %s)", u.Name, len(addrs), ci.Addr()),
	}
	if ns, pod := KubeLocationFromDNS(first(addrs)); ns != "" {
		static.Desc = fmt.Sprintf("YBA k8s universe %q (%d live tservers, primary %s)", u.Name, len(addrs), ci.Addr())
		return Colocated{
			Static:          static,
			ControllerAddrs: addrs,
			KubeNamespace:   ns,
			KubePod:         pod,
		}, nil
	}
	return static, nil
}

// FromProvisionedUniverse adapts a freshly provisioned YBA universe, whose
// operator-facing endpoint (conn/addrs) is resolved separately from the
// in-cluster addresses the amp controller connects at. A kubernetes universe
// yields a Colocated carrying both the controller addresses and the kube
// location; a VM universe (no in-cluster addresses — its node IPs are reachable
// both ways) stays a plain Static.
func FromProvisionedUniverse(conn pg.ConnInfo, addrs, inClusterAddrs []string) BackingStore {
	if len(inClusterAddrs) == 0 {
		return Static{
			Conn:  conn,
			Addrs: addrs,
			Desc:  fmt.Sprintf("provisioned YBA universe (%d live tservers, primary %s)", len(addrs), conn.Addr()),
		}
	}
	ns, pod := KubeLocationFromDNS(first(inClusterAddrs))
	return Colocated{
		Static: Static{
			Conn:  conn,
			Addrs: addrs,
			Desc:  fmt.Sprintf("provisioned YBA k8s universe (%d tservers, amp via %s, coordinator via %s)", len(inClusterAddrs), strings.Join(inClusterAddrs, ","), conn.Addr()),
		},
		ControllerAddrs: inClusterAddrs,
		KubeNamespace:   ns,
		KubePod:         pod,
	}
}

// KubeLocationFromDNS pulls the pod and namespace out of a StatefulSet pod's
// in-cluster address — "<pod>.<service>.<namespace>.svc.cluster.local[:port]",
// the shape a YugabyteDB-in-k8s deploy and a YBA kubernetes universe both
// report their tservers at. Anything else (a VM node's bare IP, a Service-only
// name) yields empty strings, which leaves the caller on the dial path.
func KubeLocationFromDNS(addr string) (namespace, pod string) {
	host := strings.TrimSpace(addr)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	parts := strings.Split(host, ".")
	// Exactly <pod>.<service>.<namespace>.svc… — a Service-only name
	// (<service>.<namespace>.svc…) is one label shorter and carries no pod, so
	// requiring "svc" in the fourth position is what tells them apart.
	if len(parts) < 4 || parts[3] != "svc" || parts[0] == "" || parts[2] == "" {
		return "", ""
	}
	return parts[2], parts[0]
}

func first(addrs []string) string {
	if len(addrs) == 0 {
		return ""
	}
	return addrs[0]
}

// controllerBacking is the optional surface a co-located store implements to
// point the amp controller at a DIFFERENT (in-cluster) address than the
// coordinator-reachable HostPortsCSV.
type controllerBacking interface {
	ControllerHostPortsCSV() string
}

// kubeColocated is the optional surface a store in THIS cluster implements to
// say where its tservers are, so ampstack execs the yb_storage CREATE in a pod
// rather than dialling an address the coordinator may have no route to.
type kubeColocated interface {
	KubeColocation() (namespace, pod string)
}

// ExternalBacking maps a BackingStore into an ampstack.Backing (external mode):
// the amp controller + endpoints are wired at HostPortsCSV with the store's YB
// role/password/sslmode. DBName is left empty so ampstack applies its yb_storage
// default — the store's ConnInfo.Database is the cluster's maintenance database,
// not the amp storage database.
//
// A co-located store (Colocated) additionally splits the two paths: the amp
// controller/endpoints connect over the in-cluster Service DNS
// (ControllerHostPortsCSV), while the one-shot yb_storage CREATE runs from the
// coordinator over the tailnet HostPortsCSV (AdminHostPorts) — the coordinator
// cannot resolve the in-cluster name.
func ExternalBacking(bs BackingStore) ampstack.Backing {
	ci := bs.ConnInfo()
	b := ampstack.Backing{
		Mode:      ampstack.BackingExternal,
		HostPorts: bs.HostPortsCSV(),
		User:      ci.User,
		Password:  ci.Password,
		SSLMode:   ci.SSLMode,
	}
	if kc, ok := bs.(kubeColocated); ok {
		if ns, pod := kc.KubeColocation(); ns != "" {
			b.KubeNamespace, b.KubePod = ns, pod
		}
	}
	if cb, ok := bs.(controllerBacking); ok {
		if inCluster := cb.ControllerHostPortsCSV(); inCluster != "" {
			b.AdminHostPorts = b.HostPorts // coordinator create over the tailnet endpoint
			b.HostPorts = inCluster        // amp controller/endpoints over in-cluster DNS
		}
	}
	return b
}
