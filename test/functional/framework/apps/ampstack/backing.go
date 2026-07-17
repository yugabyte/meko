package ampstack

import "github.com/yugabyte/yb-amp/test/functional/framework/apps/pg"

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
