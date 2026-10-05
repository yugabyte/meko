// Package ybbacking adapts the framework's YugabyteDB cluster apps
// (apps/yugabyted, apps/yugabyte, apps/yba, apps/ybk8s) into a single
// BackingStore: a primary YSQL ConnInfo plus the full CSV of tserver host:port
// addresses. The adapters are pure and tiny, with no lifecycle and no I/O.
package ybbacking

import (
	"fmt"
	"net"
	"strings"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/pg"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/yba"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/yugabyte"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/yugabyted"
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
// framework's own clients use, while InClusterAddrs is the in-cluster Service
// DNS the stack's pods connect at. The coordinator cannot resolve an in-cluster
// .svc.cluster.local name, hence the two paths.
type Colocated struct {
	Static
	InClusterAddrs []string
	// KubeNamespace / KubePod locate a tserver of this backing in the cluster,
	// so one-shot admin commands can be exec'd where the database lives
	// instead of dialled from the coordinator. Empty leaves the dial path.
	KubeNamespace string
	KubePod       string
}

// KubeColocation reports where to exec into this backing.
func (c Colocated) KubeColocation() (namespace, pod string) { return c.KubeNamespace, c.KubePod }

// InClusterHostPortsCSV is the in-cluster host:port CSV the stack's own pods
// connect at.
func (c Colocated) InClusterHostPortsCSV() string { return strings.Join(c.InClusterAddrs, ",") }

var _ BackingStore = Colocated{}

// FromYBK8s adapts a co-located YugabyteDB-in-k8s deploy: tailnetConn/tailnetAddrs
// are the coordinator-reachable (tailnet) endpoint, inClusterAddrs are the
// in-cluster Service DNS the stack connects at.
func FromYBK8s(tailnetConn pg.ConnInfo, tailnetAddrs, inClusterAddrs []string) Colocated {
	ns, pod := KubeLocationFromDNS(first(inClusterAddrs))
	return Colocated{
		Static: Static{
			Conn:  tailnetConn,
			Addrs: tailnetAddrs,
			Desc:  fmt.Sprintf("co-located YB-in-k8s (%d tservers, primary %s, in-cluster %s)", len(tailnetAddrs), tailnetConn.Addr(), strings.Join(inClusterAddrs, ",")),
		},
		InClusterAddrs: inClusterAddrs,
		KubeNamespace:  ns,
		KubePod:        pod,
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
			Static:         static,
			InClusterAddrs: addrs,
			KubeNamespace:  ns,
			KubePod:        pod,
		}, nil
	}
	return static, nil
}

// KubeLocationFromDNS pulls the pod and namespace out of a StatefulSet pod's
// in-cluster address, "<pod>.<service>.<namespace>.svc.cluster.local[:port]",
// the shape a YugabyteDB-in-k8s deploy and a YBA kubernetes universe both
// report their tservers at. Anything else (a VM node's bare IP, a Service-only
// name) yields empty strings, which leaves the caller on the dial path.
func KubeLocationFromDNS(addr string) (namespace, pod string) {
	host := strings.TrimSpace(addr)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	parts := strings.Split(host, ".")
	// Exactly <pod>.<service>.<namespace>.svc…, a Service-only name
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

// ServiceFromPodDNS turns a StatefulSet pod's address into its governing
// Service, which outlives the pod. ok is false for anything but a pod name.
func ServiceFromPodDNS(addr string) (host string, port int, ok bool) {
	ns, pod := KubeLocationFromDNS(addr)
	if ns == "" || pod == "" {
		return "", 0, false
	}
	hostPart := strings.TrimSpace(addr)
	if h, p, err := net.SplitHostPort(hostPart); err == nil {
		hostPart = h
		fmt.Sscan(p, &port)
	}
	parts := strings.Split(hostPart, ".")
	return fmt.Sprintf("%s.%s.svc.cluster.local.", parts[1], ns), port, true
}
