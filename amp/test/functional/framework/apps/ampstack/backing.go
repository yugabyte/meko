package ampstack

import (
	"context"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
)

// backingVersionProbe bounds the direct-dial version read. It is best-effort
// reporting, never a gate, so it must not hold up provisioning.
const backingVersionProbe = 5 * time.Second

// queryBackingVersion dials the backing and reads `select version()`.
func queryBackingVersion(ctx context.Context, ci pg.ConnInfo) string {
	cctx, cancel := context.WithTimeout(ctx, backingVersionProbe)
	defer cancel()
	client, err := pg.Connect(cctx, ci)
	if err != nil {
		return ""
	}
	defer client.Close()
	v, err := client.QueryString(cctx, "select version()")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

// BackingConnInfo exposes connection info for the stack's own YugabyteDB
// backing store — the built-in single-node yugabyted or the external cluster,
// whichever this App is configured for — the same target dbURL() drives the
// controller's own AMP_DATABASE_URL at. It exists so a caller outside this
// package (an env provider live-detecting the backing version) can run its
// own query without ampstack knowing anything about version detection.
func (a *App) BackingConnInfo() pg.ConnInfo {
	host, port := firstHostPort(a.backingHostPorts())
	return pg.ConnInfo{
		Host:     host,
		Port:     port,
		User:     a.backingUser(),
		Password: a.cfg.Backing.Password,
		Database: a.storageDBName(),
		SSLMode:  orDefault(a.cfg.Backing.SSLMode, DefaultBackingSSLMode),
	}
}

// StorageAccess describes how to reach the amp storage database from THIS
// process, which BackingConnInfo deliberately does not: that returns the address
// the controller and endpoints are wired at, and in two of the three shapes it
// is an in-cluster name this process cannot resolve — the built-in yugabyted's
// pod FQDN, and a co-located external YB's Service DNS.
//
// Conn always carries the role, password, sslmode and database. When
// ForwardTarget is set, Conn has no address and the caller must port-forward
// ForwardTarget:RemotePort in ForwardNamespace and dial the local end; the
// caller owns that forward's lifetime, which is why this reports a target
// rather than opening one.
type StorageAccess struct {
	Conn             pg.ConnInfo
	ForwardTarget    string // kubectl port-forward target; "" when Conn is directly dialable
	ForwardNamespace string // namespace holding ForwardTarget; "" → the stack's own
	RemotePort       int    // port to forward to; 0 when ForwardTarget is ""
}

// StorageAccess resolves how this process reaches the amp storage database.
//
// Anything living in a cluster is forwarded to, never dialled: a pod address is
// routable only from inside, and on a cluster whose pod CIDR is shadowed by an
// unrelated advertised route the dial does not even fail fast — it times out
// against a perfectly healthy database. Only a backing that named no kube
// location (a VM universe, a cluster owned elsewhere) is dialled directly.
func (a *App) StorageAccess() StorageAccess {
	ci := a.BackingConnInfo()
	if !a.externalBacking() {
		// The built-in yugabyted answers only inside the cluster. Forward to the
		// pod, not the headless Service, so the target is unambiguous.
		ci.Host, ci.Port = "", 0
		return StorageAccess{Conn: ci, ForwardTarget: "pod/" + yugabytedPod, RemotePort: ybPGPort}
	}
	if ns := strings.TrimSpace(a.cfg.Backing.KubeNamespace); ns != "" {
		ci.Host, ci.Port = "", 0
		return StorageAccess{
			Conn:             ci,
			ForwardTarget:    "pod/" + a.backingPod(),
			ForwardNamespace: ns,
			RemotePort:       ybPGPort,
		}
	}
	// External and not in a cluster we can name: adminHostPorts is the
	// coordinator-reachable CSV, falling back to HostPorts.
	ci.Host, ci.Port = firstHostPort(a.adminHostPorts())
	return StorageAccess{Conn: ci}
}
