package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yugabyte/amp/controller/local/config"
)

// TestInitRoundTrip runs `init` into a temp repo and reloads the config,
// asserting the persisted config round-trips: templates present, keypair
// written, control_plane_api + yb block set, and the port lanes applied off
// --port-base.
func TestInitRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	t.Setenv("YB_AMP_REPO_DIR", dir)
	// Keep pg_distrib_dir deterministic (not required, just tidy).
	t.Setenv("POSTGRES_DISTRIB_DIR", "/pg_install")

	base := uint16(20000)
	if err := Init(InitArgs{
		YbDbname:       "ephemeral_db",
		YbHost:         "10.0.0.1",
		YbPort:         5433,
		YbHotPath:      "ysql",
		NumPageservers: 1,
		PortBase:       &base,
		Force:          ForceMustNotExist,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Keypair written.
	for _, name := range []string{"auth_private_key.pem", "auth_public_key.pem", "config"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s to exist: %v", name, err)
		}
	}
	// endpoints dir created.
	if fi, err := os.Stat(filepath.Join(dir, "endpoints")); err != nil || !fi.IsDir() {
		t.Errorf("endpoints dir missing: %v", err)
	}

	env, err := config.LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if env.ControlPlaneAPI != "http://127.0.0.1:20000/upcall/v1/" {
		t.Errorf("control_plane_api = %q", env.ControlPlaneAPI)
	}
	if len(env.PageserverTemplates) != 1 {
		t.Fatalf("pageserver templates = %d, want 1", len(env.PageserverTemplates))
	}
	pt := env.PageserverTemplates[0]
	if pt.ListenPgAddr != "127.0.0.1:20300" { // base + 300
		t.Errorf("ps template listen_pg_addr = %q, want 127.0.0.1:20300", pt.ListenPgAddr)
	}
	if pt.ListenHTTPAddr != "127.0.0.1:20400" { // base + 400
		t.Errorf("ps template listen_http_addr = %q, want 127.0.0.1:20400", pt.ListenHTTPAddr)
	}
	if pt.ListenGRPCAddr == nil || *pt.ListenGRPCAddr != "127.0.0.1:20500" { // base + 500
		t.Errorf("ps template listen_grpc_addr = %v, want 127.0.0.1:20500", pt.ListenGRPCAddr)
	}
	if !pt.NoSync {
		t.Error("ps template no_sync should be true")
	}
	if pt.YbStorageConn == nil {
		t.Error("ps template yb_storage_conn should be set")
	}
	if len(env.SafekeeperTemplates) != 1 {
		t.Fatalf("safekeeper templates = %d, want 1", len(env.SafekeeperTemplates))
	}
	st := env.SafekeeperTemplates[0]
	if st.PgPort != 20100 || st.HTTPPort != 20200 { // base + 100 / + 200
		t.Errorf("sk template ports = pg %d http %d, want 20100/20200", st.PgPort, st.HTTPPort)
	}
	if p, _ := portOf(env.EndpointStorage.ListenAddr); p != 20600 { // base + 600
		t.Errorf("endpoint_storage port = %d, want 20600", p)
	}
	if env.Controller.Yb == nil {
		t.Fatal("controller.yb should be set (external YB)")
	}
	if env.Controller.Yb.Dbname != "ephemeral_db" {
		t.Errorf("controller.yb dbname = %q, want ephemeral_db", env.Controller.Yb.Dbname)
	}
	if env.Controller.Yb.HostPorts != "10.0.0.1:5433" {
		t.Errorf("controller.yb host_ports = %q, want 10.0.0.1:5433", env.Controller.Yb.HostPorts)
	}
	if env.DefaultBranchID == nil || *env.DefaultBranchID != zeroBranchID {
		t.Errorf("default_branch_id = %v, want all-zeros", env.DefaultBranchID)
	}

	// No pageserver/safekeeper/endpoint_storage instances provisioned yet.
	if len(env.Pageservers) != 0 || len(env.Safekeepers) != 0 || len(env.EndpointStorages) != 0 {
		t.Errorf("expected no provisioned nodes, got ps=%d sk=%d es=%d",
			len(env.Pageservers), len(env.Safekeepers), len(env.EndpointStorages))
	}

	// Re-init with must-not-exist should now fail (dir exists).
	if err := Init(InitArgs{Force: ForceMustNotExist}); err == nil {
		t.Error("re-init with must-not-exist should fail on an existing repo")
	}
}

// TestYbInlineTOMLEmitsSelectableHotPath pins which lanes reach the rendered
// `[*.yb]` block.
//
// Regression: the renderer used to emit `hot_path` only for `"ysql"`. Once the
// Rust `HotPath::default()` became `yb_thin_client`, every other value —
// including the flag's own documented default — silently fell through to the
// thin client, so no other lane could be selected or fallen back to. The
// default lane must still be OMITTED: it is what an absent key already means,
// so spelling it out is redundant in every generated config.
func TestYbInlineTOMLEmitsSelectableHotPath(t *testing.T) {
	base := config.YbConnConfig{HostPorts: "127.0.0.1:5433", User: "yugabyte", Dbname: "yb_storage"}

	for _, tc := range []struct {
		name    string
		hotPath *string
		want    string // substring that must appear, "" = nothing emitted
	}{
		{"unset", nil, ""},
		{"default lane is omitted", strPtr(DefaultYbHotPath), ""},
		{"ysql is selectable", strPtr("ysql"), `hot_path = "ysql"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			c.HotPath = tc.hotPath
			got := ybInlineTOML(&c)
			if tc.want == "" {
				if strings.Contains(got, "hot_path") {
					t.Fatalf("expected no hot_path in %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("expected %q in %q", tc.want, got)
			}
		})
	}
}

func strPtr(s string) *string { return &s }
