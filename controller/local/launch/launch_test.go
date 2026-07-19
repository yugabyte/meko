package launch

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yugabyte/amp/controller/local/config"
)

func ptrStr(s string) *string { return &s }

// envWithTemplates mirrors the Rust `env_with_templates` test fixture: one
// pageserver + one safekeeper template, no provisioned nodes yet.
func envWithTemplates() *config.LocalEnv {
	return &config.LocalEnv{
		BaseDataDir:     "/tmp/repo",
		PgDistribDir:    "/pg",
		YbAmpDistribDir: "/yb_amp",
		PageserverTemplates: []config.YbAmpLocalInitPageserverConf{{
			ID:             1,
			ListenPgAddr:   "127.0.0.1:64000",
			ListenHTTPAddr: "127.0.0.1:9898",
			ListenGRPCAddr: ptrStr("127.0.0.1:51051"),
			PgAuthType:     config.AuthTrust,
			HTTPAuthType:   config.AuthTrust,
			GRPCAuthType:   config.AuthTrust,
			NoSync:         true,
			YbStorageConn:  ptrStr("{ host = 'h' }"),
		}},
		SafekeeperTemplates: []config.SafekeeperConf{{
			ID:            1,
			PgPort:        5454,
			HTTPPort:      7676,
			YbStorageConn: ptrStr("{ host = 'h' }"),
		}},
		EndpointStorage: config.EndpointStorageConf{ListenAddr: "127.0.0.1:9993"},
		ControlPlaneAPI: "http://127.0.0.1:1234/upcall/v1/",
	}
}

func TestFirstPageserverReusesTemplateIDAndPorts(t *testing.T) {
	env := envWithTemplates()
	conf, err := allocatePageserverConf(env, "main")
	if err != nil {
		t.Fatal(err)
	}
	if conf.ID != 1 {
		t.Errorf("id = %d, want 1", conf.ID)
	}
	if conf.ListenPgAddr != "127.0.0.1:64000" {
		t.Errorf("listen_pg_addr = %q, want 127.0.0.1:64000", conf.ListenPgAddr)
	}
	if conf.ListenGRPCAddr == nil || *conf.ListenGRPCAddr != "127.0.0.1:51051" {
		t.Errorf("listen_grpc_addr = %v, want 127.0.0.1:51051", conf.ListenGRPCAddr)
	}
	if want := filepath.FromSlash("/tmp/repo/endpoints/main/pageserver"); conf.DataDir != want {
		t.Errorf("data_dir = %q, want %q", conf.DataDir, want)
	}
}

func TestSecondPageserverGetsNextIDAndPortDelta(t *testing.T) {
	env := envWithTemplates()
	first, err := allocatePageserverConf(env, "main")
	if err != nil {
		t.Fatal(err)
	}
	env.Pageservers = append(env.Pageservers, first)
	conf, err := allocatePageserverConf(env, "ep_b1")
	if err != nil {
		t.Fatal(err)
	}
	// id 2, delta = (2-1)*2 = 2.
	if conf.ID != 2 {
		t.Errorf("id = %d, want 2", conf.ID)
	}
	if conf.ListenPgAddr != "127.0.0.1:64002" {
		t.Errorf("listen_pg_addr = %q, want 127.0.0.1:64002", conf.ListenPgAddr)
	}
	if conf.ListenGRPCAddr == nil || *conf.ListenGRPCAddr != "127.0.0.1:51053" {
		t.Errorf("listen_grpc_addr = %v, want 127.0.0.1:51053", conf.ListenGRPCAddr)
	}
}

func TestFirstSafekeeperReusesTemplateIDAndPorts(t *testing.T) {
	env := envWithTemplates()
	conf, err := allocateSafekeeperConf(env, "main", "aa")
	if err != nil {
		t.Fatal(err)
	}
	if conf.ID != 1 || conf.PgPort != 5454 || conf.HTTPPort != 7676 {
		t.Errorf("got id=%d pg=%d http=%d, want 1/5454/7676", conf.ID, conf.PgPort, conf.HTTPPort)
	}
	if conf.BranchID == nil || *conf.BranchID != "aa" {
		t.Errorf("branch_id = %v, want aa", conf.BranchID)
	}
	if conf.EndpointID != "main" {
		t.Errorf("endpoint_id = %q, want main", conf.EndpointID)
	}
	if conf.YbStorageConn == nil || *conf.YbStorageConn != "{ host = 'h' }" {
		t.Errorf("yb_storage_conn = %v", conf.YbStorageConn)
	}
}

func TestSecondSafekeeperGetsNextIDAndPortDelta(t *testing.T) {
	env := envWithTemplates()
	first, err := allocateSafekeeperConf(env, "main", "aa")
	if err != nil {
		t.Fatal(err)
	}
	env.Safekeepers = append(env.Safekeepers, first)
	conf, err := allocateSafekeeperConf(env, "ep_b1", "bb")
	if err != nil {
		t.Fatal(err)
	}
	// id 2, delta = 2.
	if conf.ID != 2 || conf.PgPort != 5456 || conf.HTTPPort != 7678 {
		t.Errorf("got id=%d pg=%d http=%d, want 2/5456/7678", conf.ID, conf.PgPort, conf.HTTPPort)
	}
	if conf.EndpointID != "ep_b1" {
		t.Errorf("endpoint_id = %q, want ep_b1", conf.EndpointID)
	}
}

func TestFirstEndpointStorageReusesTemplatePort(t *testing.T) {
	env := envWithTemplates()
	conf, err := allocateEndpointStorageConf(env, "main")
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := portOf(conf.ListenAddr); p != 9993 {
		t.Errorf("port = %d, want 9993", p)
	}
	if conf.EndpointID != "main" {
		t.Errorf("endpoint_id = %q, want main", conf.EndpointID)
	}
}

func TestSecondEndpointStorageGetsNextFreePort(t *testing.T) {
	env := envWithTemplates()
	first, err := allocateEndpointStorageConf(env, "main")
	if err != nil {
		t.Fatal(err)
	}
	env.EndpointStorages = append(env.EndpointStorages, first)
	conf, err := allocateEndpointStorageConf(env, "ep_b1")
	if err != nil {
		t.Fatal(err)
	}
	// Lowest free port of the form base + 2k → 9995.
	if p, _ := portOf(conf.ListenAddr); p != 9995 {
		t.Errorf("port = %d, want 9995", p)
	}
}

func TestEndpointStorageReusesFreedPortAfterDeprovision(t *testing.T) {
	env := envWithTemplates()
	a, _ := allocateEndpointStorageConf(env, "main")
	env.EndpointStorages = append(env.EndpointStorages, a)
	b, _ := allocateEndpointStorageConf(env, "ep_b1")
	env.EndpointStorages = append(env.EndpointStorages, b)
	// Drop the first endpoint's instance (port 9993 freed).
	var kept []config.EndpointStorageConf
	for _, e := range env.EndpointStorages {
		if e.EndpointID != "main" {
			kept = append(kept, e)
		}
	}
	env.EndpointStorages = kept
	conf, err := allocateEndpointStorageConf(env, "ep_b2")
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := portOf(conf.ListenAddr); p != 9993 {
		t.Errorf("port = %d, want 9993 (reused)", p)
	}
}

func TestPageserverInitConfPreservesTemplateYbStorageConn(t *testing.T) {
	env := envWithTemplates()
	allocated, err := allocatePageserverConf(env, "main")
	if err != nil {
		t.Fatal(err)
	}
	initConf, err := pageserverInitConfFor(env, &allocated)
	if err != nil {
		t.Fatal(err)
	}
	if initConf.ID != allocated.ID {
		t.Errorf("id = %d, want %d", initConf.ID, allocated.ID)
	}
	if initConf.ListenPgAddr != allocated.ListenPgAddr {
		t.Errorf("listen_pg_addr = %q, want %q", initConf.ListenPgAddr, allocated.ListenPgAddr)
	}
	if initConf.YbStorageConn == nil || *initConf.YbStorageConn != "{ host = 'h' }" {
		t.Errorf("yb_storage_conn = %v", initConf.YbStorageConn)
	}
	if !initConf.NoSync {
		t.Error("no_sync should be preserved")
	}
}

// TestSuccessfullyCreatedLineInvariant asserts the exact stdout contract the
// integration harness scrapes: a line containing "successfully created" whose
// 2nd whitespace token is the branch id.
func TestSuccessfullyCreatedLineInvariant(t *testing.T) {
	branchID := "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0"
	line := successfullyCreatedLine(branchID)
	if !strings.Contains(line, "successfully created") {
		t.Fatalf("line %q must contain 'successfully created'", line)
	}
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[1] != branchID {
		t.Fatalf("2nd whitespace token of %q = %q, want %q", line, secondToken(fields), branchID)
	}
}

func secondToken(fields []string) string {
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

func TestYbInlineTOML(t *testing.T) {
	c := config.YbConnConfig{HostPorts: "127.0.0.1:5433", User: "yugabyte", Dbname: "yb_storage"}
	got := ybInlineTOML(&c)
	want := `{ host_ports = "127.0.0.1:5433", user = "yugabyte", dbname = "yb_storage" }`
	if got != want {
		t.Errorf("ybInlineTOML = %q, want %q", got, want)
	}
	hp := "ysql"
	c.HotPath = &hp
	if got := ybInlineTOML(&c); !strings.Contains(got, `hot_path = "ysql"`) {
		t.Errorf("ysql hot_path not emitted: %q", got)
	}
}

func TestMutableChanged(t *testing.T) {
	env := envWithTemplates()
	before := SnapshotMutable(env)
	if MutableChanged(before, env) {
		t.Error("no mutation should report unchanged")
	}
	// Appending a safekeeper is a persisted mutation.
	env.Safekeepers = append(env.Safekeepers, config.SafekeeperConf{ID: 1, EndpointID: "main"})
	if !MutableChanged(before, env) {
		t.Error("appending a safekeeper should report changed")
	}
	// Setting the default branch is a persisted mutation.
	env2 := envWithTemplates()
	before2 := SnapshotMutable(env2)
	def := "abc"
	env2.DefaultBranchID = &def
	if !MutableChanged(before2, env2) {
		t.Error("setting default branch should report changed")
	}
}
