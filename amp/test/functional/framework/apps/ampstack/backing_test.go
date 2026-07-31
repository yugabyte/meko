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
