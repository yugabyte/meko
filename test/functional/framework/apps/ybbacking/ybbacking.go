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
	"strings"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/yba"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/yugabyte"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/yugabyted"
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
}

// ControllerHostPortsCSV is the in-cluster host:port CSV the amp controller +
// endpoints connect at (ExternalBacking routes it into ampstack.Backing).
func (c Colocated) ControllerHostPortsCSV() string { return strings.Join(c.ControllerAddrs, ",") }

var _ BackingStore = Colocated{}

// FromYBK8s adapts a co-located YugabyteDB-in-k8s deploy: tailnetConn/tailnetAddrs
// are the coordinator-reachable (tailnet) endpoint, inClusterAddrs are the
// in-cluster Service DNS the amp controller connects at.
func FromYBK8s(tailnetConn pg.ConnInfo, tailnetAddrs, inClusterAddrs []string) Colocated {
	return Colocated{
		Static: Static{
			Conn:  tailnetConn,
			Addrs: tailnetAddrs,
			Desc:  fmt.Sprintf("co-located YB-in-k8s (%d tservers, primary %s, amp via %s)", len(tailnetAddrs), tailnetConn.Addr(), strings.Join(inClusterAddrs, ",")),
		},
		ControllerAddrs: inClusterAddrs,
	}
}

// FromYBAUniverse adapts a YBA universe: its first live tserver as the primary
// target and every live tserver as the host-ports list (via yba.YSQLConnInfo).
// A universe with no live tserver is an error.
func FromYBAUniverse(u yba.Universe) (Static, error) {
	ci, addrs, err := yba.YSQLConnInfo(u)
	if err != nil {
		return Static{}, err
	}
	return Static{
		Conn:  ci,
		Addrs: addrs,
		Desc:  fmt.Sprintf("YBA universe %q (%d live tservers, primary %s)", u.Name, len(addrs), ci.Addr()),
	}, nil
}

// controllerBacking is the optional surface a co-located store implements to
// point the amp controller at a DIFFERENT (in-cluster) address than the
// coordinator-reachable HostPortsCSV.
type controllerBacking interface {
	ControllerHostPortsCSV() string
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
	if cb, ok := bs.(controllerBacking); ok {
		if inCluster := cb.ControllerHostPortsCSV(); inCluster != "" {
			b.AdminHostPorts = b.HostPorts // coordinator create over the tailnet endpoint
			b.HostPorts = inCluster        // amp controller/endpoints over in-cluster DNS
		}
	}
	return b
}
