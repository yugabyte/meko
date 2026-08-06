package ampstack

import "testing"

// TestBackingConnInfoBuiltin: the default (builtin_yugabyted) backing resolves
// to the in-namespace yugabyted pod FQDN on the standard YSQL port — the same
// target dbURL() already points the controller's AMP_DATABASE_URL at.
func TestBackingConnInfoBuiltin(t *testing.T) {
	a := New(Config{Namespace: "ampqa-ns1"})
	ci := a.BackingConnInfo()
	if want := "yugabyted-0.yugabyted.ampqa-ns1.svc.cluster.local"; ci.Host != want {
		t.Errorf("host = %q, want %q", ci.Host, want)
	}
	if ci.Port != 5433 {
		t.Errorf("port = %d, want 5433", ci.Port)
	}
	if ci.User != DefaultBackingUser {
		t.Errorf("user = %q, want %q", ci.User, DefaultBackingUser)
	}
	if ci.Database != DefaultDBName {
		t.Errorf("database = %q, want %q", ci.Database, DefaultDBName)
	}
}

// TestBackingConnInfoExternal: external mode resolves to the configured
// HostPorts (first entry) and the configured YB role/db/password, mirroring
// ensureExternalStorageDB's own connection.
func TestBackingConnInfoExternal(t *testing.T) {
	a := New(Config{
		Namespace: "ampqa-ns2",
		Backing: Backing{
			Mode:      BackingExternal,
			HostPorts: "10.1.2.3:5433,10.1.2.4:5433",
			User:      "custom_role",
			DBName:    "custom_db",
			Password:  "secret",
			SSLMode:   "require",
		},
	})
	ci := a.BackingConnInfo()
	if ci.Host != "10.1.2.3" || ci.Port != 5433 {
		t.Errorf("host:port = %s:%d, want 10.1.2.3:5433", ci.Host, ci.Port)
	}
	if ci.User != "custom_role" {
		t.Errorf("user = %q, want custom_role", ci.User)
	}
	if ci.Database != "custom_db" {
		t.Errorf("database = %q, want custom_db", ci.Database)
	}
	if ci.Password != "secret" {
		t.Errorf("password not carried through")
	}
	if ci.SSLMode != "require" {
		t.Errorf("sslmode = %q, want require", ci.SSLMode)
	}
}

// TestStorageAccessBuiltinNeedsForward: the built-in yugabyted is cluster-internal,
// so StorageAccess must NOT hand back the pod FQDN BackingConnInfo returns — a
// caller outside the cluster cannot resolve it. It reports a forward target
// instead, and leaves the address empty so a caller that ignores the target
// fails loudly rather than dialling nothing.
func TestStorageAccessBuiltinNeedsForward(t *testing.T) {
	a := New(Config{Namespace: "ampqa-ns1"})
	got := a.StorageAccess()
	if want := "pod/" + yugabytedPod; got.ForwardTarget != want {
		t.Errorf("forward target = %q, want %q", got.ForwardTarget, want)
	}
	if got.RemotePort != ybPGPort {
		t.Errorf("remote port = %d, want %d", got.RemotePort, ybPGPort)
	}
	if got.Conn.Host != "" || got.Conn.Port != 0 {
		t.Errorf("conn carries an address %s:%d, want none (it is not dialable from here)", got.Conn.Host, got.Conn.Port)
	}
	if got.Conn.Database != DefaultDBName || got.Conn.User != DefaultBackingUser {
		t.Errorf("conn = %s/%s, want %s/%s", got.Conn.User, got.Conn.Database, DefaultBackingUser, DefaultDBName)
	}
}

// TestStorageAccessExternalIsDirect: a plain external backing is reachable from
// here, so StorageAccess dials it with no forward.
func TestStorageAccessExternalIsDirect(t *testing.T) {
	a := New(Config{
		Namespace: "ampqa-ns2",
		Backing: Backing{
			Mode:      BackingExternal,
			HostPorts: "10.1.2.3:5433,10.1.2.4:5433",
			DBName:    "custom_db",
			Password:  "secret",
		},
	})
	got := a.StorageAccess()
	if got.ForwardTarget != "" {
		t.Errorf("forward target = %q, want none (external backings are directly reachable)", got.ForwardTarget)
	}
	if got.Conn.Host != "10.1.2.3" || got.Conn.Port != 5433 {
		t.Errorf("host:port = %s:%d, want 10.1.2.3:5433", got.Conn.Host, got.Conn.Port)
	}
	if got.Conn.Database != "custom_db" || got.Conn.Password != "secret" {
		t.Errorf("conn did not carry db/password through: %+v", got.Conn)
	}
}

// TestStorageAccessColocatedUsesAdminHostPorts is the case BackingConnInfo gets
// wrong for an outside caller: a co-located YB-in-k8s wires the controller at an
// in-cluster Service DNS name (HostPorts) that this process cannot resolve, and
// carries the coordinator-reachable address in AdminHostPorts. StorageAccess must
// pick the latter — the same choice ensureExternalStorageDB makes.
func TestStorageAccessColocatedUsesAdminHostPorts(t *testing.T) {
	a := New(Config{
		Namespace: "ampqa-ns3",
		Backing: Backing{
			Mode:           BackingExternal,
			HostPorts:      "yb-tserver-service.yb.svc.cluster.local:5433",
			AdminHostPorts: "100.64.0.7:5433",
		},
	})
	got := a.StorageAccess()
	if got.Conn.Host != "100.64.0.7" || got.Conn.Port != 5433 {
		t.Errorf("host:port = %s:%d, want the coordinator-reachable 100.64.0.7:5433, not the in-cluster name", got.Conn.Host, got.Conn.Port)
	}
	if got.ForwardTarget != "" {
		t.Errorf("forward target = %q, want none", got.ForwardTarget)
	}
}
