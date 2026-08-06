package ampstack

import "github.com/yugabyte/amp/test/functional/framework/apps/pg"

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
// ForwardTarget:RemotePort and dial the local end; the caller owns that
// forward's lifetime, which is why this reports a target rather than opening one.
type StorageAccess struct {
	Conn          pg.ConnInfo
	ForwardTarget string // kubectl port-forward target; "" when Conn is directly dialable
	RemotePort    int    // port to forward to; 0 when ForwardTarget is ""
}

// StorageAccess resolves how this process reaches the amp storage database.
func (a *App) StorageAccess() StorageAccess {
	ci := a.BackingConnInfo()
	if !a.externalBacking() {
		// The built-in yugabyted answers only inside the cluster. Forward to the
		// pod, not the headless Service, so the target is unambiguous.
		ci.Host, ci.Port = "", 0
		return StorageAccess{Conn: ci, ForwardTarget: "pod/" + yugabytedPod, RemotePort: ybPGPort}
	}
	// External: adminHostPorts is the coordinator-reachable CSV — the same
	// address ensureExternalStorageDB dials, and it falls back to HostPorts when
	// the backing is not co-located.
	ci.Host, ci.Port = firstHostPort(a.adminHostPorts())
	return StorageAccess{Conn: ci}
}
