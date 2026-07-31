package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func strPtr(s string) *string { return &s }
func u16Ptr(v uint16) *uint16 { return &v }

// fullConfig builds an OnDiskConfig exercising every skip rule and the
// nested sub-structs, for round-trip testing.
func fullConfig() OnDiskConfig {
	branch := BranchID("3d1f7595b468230304e0b73cecbcb081")
	yb := &YbConnConfig{
		HostPorts:   "10.0.0.1:5433,10.0.0.2:5440",
		User:        "yugabyte",
		Dbname:      "yb_storage",
		PasswordEnv: strPtr("YB_PW"),
		Sslmode:     strPtr("require"),
	}
	ctrl := DefaultYbAmpControllerConf()
	ctrl.Yb = yb
	ybConn := "{ host_ports = \"10.0.0.1:5433\", user = \"yugabyte\", dbname = \"yb_storage\" }"
	return OnDiskConfig{
		PgDistribDir:    "/pg",
		YbAmpDistribDir: "/yb-amp",
		DefaultBranchID: &branch,
		PrivateKeyPath:  "auth_private_key.pem",
		PublicKeyPath:   "auth_public_key.pem",
		Controller:      ctrl,
		Safekeepers: []SafekeeperConf{{
			PgPort: 5454, HTTPPort: 7676, YbStorageConn: strPtr(ybConn),
			BranchID: &branch, EndpointID: "main",
		}},
		PageserverTemplates: []YbAmpLocalInitPageserverConf{{
			ListenPgAddr:   "127.0.0.1:64000",
			ListenHTTPAddr: "127.0.0.1:9898",
			ListenGRPCAddr: strPtr("127.0.0.1:51051"),
			PgAuthType:     AuthTrust,
			HTTPAuthType:   AuthTrust,
			GRPCAuthType:   AuthTrust,
			YbStorageConn:  strPtr(ybConn),
			Other:          map[string]any{"extra_pageserver_key": "value123"},
		}},
		SafekeeperTemplates: []SafekeeperConf{{
			PgPort: 5454, HTTPPort: 7676, EndpointID: "",
		}},
		EndpointStorage: DefaultEndpointStorageConf(),
		ControlPlaneAPI: strPtr("http://127.0.0.1:1234/upcall/v1/"),
	}
}

func TestOnDiskConfigRoundTrip(t *testing.T) {
	cfg := fullConfig()
	data, err := cfg.marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, err := unmarshalOnDisk(data)
	if err != nil {
		t.Fatalf("unmarshal: %v\n---\n%s", err, data)
	}
	if !reflect.DeepEqual(cfg, *back) {
		t.Fatalf("round-trip mismatch:\nwant %#v\ngot  %#v\n---toml---\n%s", cfg, *back, data)
	}
}

func TestHumantimeSerialization(t *testing.T) {
	cfg := fullConfig()
	data, err := cfg.marshal()
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		`max_offline = '10s'`,
		`max_warming_up = '30s'`,
		`heartbeat_interval = '1s'`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in output:\n%s", want, s)
		}
	}
}

func TestHumantimeFormatParse(t *testing.T) {
	cases := []struct {
		d   time.Duration
		str string
	}{
		{10 * time.Second, "10s"},
		{30 * time.Second, "30s"},
		{time.Second, "1s"},
		{1500 * time.Millisecond, "1s 500ms"},
		{90 * time.Minute, "1h 30m"},
		{0, "0s"},
		{500 * time.Millisecond, "500ms"},
	}
	for _, c := range cases {
		if got := formatHumantime(c.d); got != c.str {
			t.Errorf("formatHumantime(%v) = %q, want %q", c.d, got, c.str)
		}
		back, err := parseHumantime(c.str)
		if err != nil {
			t.Errorf("parseHumantime(%q): %v", c.str, err)
			continue
		}
		if back != c.d {
			t.Errorf("parseHumantime(%q) = %v, want %v", c.str, back, c.d)
		}
	}
	// Lenient parse forms humantime accepts.
	for _, in := range []string{"1h30m", "1minute", "2days", "500 ms"} {
		if _, err := parseHumantime(in); err != nil {
			t.Errorf("parseHumantime(%q) should succeed: %v", in, err)
		}
	}
}

func TestSkipRules(t *testing.T) {
	// use_local_tls omitted when false; emitted when true.
	cfg := fullConfig()
	data, _ := cfg.marshal()
	if strings.Contains(string(data), "use_local_tls") {
		t.Error("use_local_tls should be omitted when false")
	}
	if strings.Contains(string(data), "endpoint_storages") {
		t.Error("endpoint_storages should be omitted when empty")
	}
	// bool fields with no skip rule are always emitted, even false.
	if !strings.Contains(string(data), "use_https_pageserver_api = false") {
		t.Errorf("use_https_pageserver_api should always be emitted:\n%s", data)
	}
	// SafekeeperConf.endpoint_id is always emitted, even empty.
	if !strings.Contains(string(data), `endpoint_id = ''`) &&
		!strings.Contains(string(data), `endpoint_id = ""`) {
		t.Errorf("safekeeper endpoint_id should be emitted even when empty:\n%s", data)
	}

	cfg.UseLocalTLS = true
	cfg.EndpointStorages = []EndpointStorageConf{DefaultEndpointStorageConf()}
	data, _ = cfg.marshal()
	if !strings.Contains(string(data), "use_local_tls = true") {
		t.Error("use_local_tls should be emitted when true")
	}
	if !strings.Contains(string(data), "endpoint_storages") {
		t.Error("endpoint_storages should be emitted when non-empty")
	}
}

func TestPageserversRejectedOnLoad(t *testing.T) {
	toml := `
pg_distrib_dir = "/pg"
yb_amp_distrib_dir = "/yb-amp"
control_plane_api = "http://127.0.0.1:1234/upcall/v1/"

[[pageservers]]
id = 1
listen_pg_addr = "127.0.0.1:64000"
`
	_, err := unmarshalOnDisk([]byte(toml))
	if err == nil {
		t.Fatal("expected error when 'pageservers' key is present")
	}
	if !strings.Contains(err.Error(), "pageservers") {
		t.Errorf("error should mention pageservers, got: %v", err)
	}
}

func TestDefaultsAppliedOnLoad(t *testing.T) {
	// A minimal config with an empty [controller] table: the humantime
	// defaults and the non-zero bool defaults must be filled in, matching
	// serde's #[serde(default)].
	toml := `
pg_distrib_dir = "/pg"
yb_amp_distrib_dir = "/yb-amp"
private_key_path = "auth_private_key.pem"
public_key_path = "auth_public_key.pem"
control_plane_api = "http://127.0.0.1:1234/upcall/v1/"

[controller]
max_offline = "42s"
`
	cfg, err := unmarshalOnDisk([]byte(toml))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Controller.MaxOffline.Duration() != 42*time.Second {
		t.Errorf("max_offline: got %v", cfg.Controller.MaxOffline.Duration())
	}
	// Defaults for fields absent from the file.
	if cfg.Controller.MaxWarmingUp.Duration() != 30*time.Second {
		t.Errorf("max_warming_up default not applied: %v", cfg.Controller.MaxWarmingUp.Duration())
	}
	if cfg.Controller.HeartbeatInterval.Duration() != time.Second {
		t.Errorf("heartbeat_interval default not applied: %v", cfg.Controller.HeartbeatInterval.Duration())
	}
	if !cfg.Controller.TimelinesOntoSafekeepers {
		t.Error("timelines_onto_safekeepers default (true) not applied")
	}
	if !cfg.Controller.UseLocalComputeNotifications {
		t.Error("use_local_compute_notifications default (true) not applied")
	}
	// endpoint_storage listen addr default.
	if cfg.EndpointStorage.ListenAddr != endpointStorageDefaultAddr {
		t.Errorf("endpoint_storage default addr not applied: %q", cfg.EndpointStorage.ListenAddr)
	}
}

func TestYbConnConfigToDatabaseURL(t *testing.T) {
	c := &YbConnConfig{HostPorts: "127.0.0.1:5433", User: "yugabyte", Dbname: "yb_storage"}
	url, err := c.ToDatabaseURL()
	if err != nil {
		t.Fatal(err)
	}
	if url != "postgresql://yugabyte@127.0.0.1:5433/yb_storage" {
		t.Errorf("got %q", url)
	}

	// multi-host + sslmode
	c = &YbConnConfig{HostPorts: "10.0.0.1:5433,10.0.0.2:5440", User: "yugabyte", Dbname: "yb_storage", Sslmode: strPtr("require")}
	url, _ = c.ToDatabaseURL()
	want := "postgresql://yugabyte@10.0.0.1:5433,10.0.0.2:5440/yb_storage?sslmode=require"
	if url != want {
		t.Errorf("got %q want %q", url, want)
	}

	// password_env resolved + percent-encoded
	t.Setenv("YB_URL_TEST_PW", "Yugabyte!1")
	c = &YbConnConfig{HostPorts: "h:5433", User: "yugabyte", Dbname: "yb_storage", PasswordEnv: strPtr("YB_URL_TEST_PW")}
	url, _ = c.ToDatabaseURL()
	if url != "postgresql://yugabyte:Yugabyte%211@h:5433/yb_storage" {
		t.Errorf("got %q", url)
	}

	// missing password env errors
	c = &YbConnConfig{HostPorts: "h:5433", PasswordEnv: strPtr("YB_URL_TEST_DEFINITELY_UNSET")}
	if _, err := c.ToDatabaseURL(); err == nil {
		t.Error("expected error for missing password env")
	}

	// default port filled for a bare host, defaults for user/dbname
	c = &YbConnConfig{HostPorts: "onlyhost"}
	url, _ = c.ToDatabaseURL()
	if url != "postgresql://yugabyte@onlyhost:5433/yb_storage" {
		t.Errorf("got %q", url)
	}
}

func TestBasePath(t *testing.T) {
	t.Run("abs env", func(t *testing.T) {
		t.Setenv("YB_AMP_REPO_DIR", "/abs/repo")
		p, err := BasePath()
		if err != nil {
			t.Fatal(err)
		}
		if p != "/abs/repo" {
			t.Errorf("got %q", p)
		}
	})
	t.Run("relative env errors", func(t *testing.T) {
		t.Setenv("YB_AMP_REPO_DIR", "rel/repo")
		if _, err := BasePath(); err == nil {
			t.Error("expected error for non-absolute YB_AMP_REPO_DIR")
		}
	})
	t.Run("unset falls back to pwd/.yb_amp", func(t *testing.T) {
		t.Setenv("YB_AMP_REPO_DIR", "")
		p, err := BasePath()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(p, ".yb_amp") || !filepath.IsAbs(p) {
			t.Errorf("got %q", p)
		}
	})
}

func TestDataDirHelpers(t *testing.T) {
	env := &LocalEnv{BaseDataDir: "/tmp/repo", PgDistribDir: "/pg", YbAmpDistribDir: "/yb-amp"}
	if got := env.PageserverDataDir("main"); got != "/tmp/repo/endpoints/main/pageserver" {
		t.Errorf("PageserverDataDir: %q", got)
	}
	if got := env.SafekeeperDataDir("ep_b1"); got != "/tmp/repo/endpoints/ep_b1/safekeeper" {
		t.Errorf("SafekeeperDataDir: %q", got)
	}
	if got := env.EndpointStorageDataDir("main"); got != "/tmp/repo/endpoints/main/endpoint_storage" {
		t.Errorf("EndpointStorageDataDir: %q", got)
	}
	if got := env.PgBinDir(18); got != "/pg/v18/bin" {
		t.Errorf("PgBinDir: %q", got)
	}
	if got := env.PgLibDir(18); got != "/pg/v18/lib" {
		t.Errorf("PgLibDir: %q", got)
	}
	if got := env.PageserverBin(); got != "/yb-amp/yb-pageserver" {
		t.Errorf("PageserverBin: %q", got)
	}
	if got := env.SafekeeperBin(); got != "/yb-amp/yb-safekeeper" {
		t.Errorf("SafekeeperBin: %q", got)
	}
	if got := env.EndpointStorageBin(); got != "/yb-amp/yb-endpoint-storage" {
		t.Errorf("EndpointStorageBin: %q", got)
	}
}

func writePageserverDir(t *testing.T, endpoints, name string, pgPort int) {
	t.Helper()
	dir := filepath.Join(endpoints, name, "pageserver")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	psToml := "listen_pg_addr = '127.0.0.1:" + strconv.Itoa(pgPort) + "'\n" +
		"listen_http_addr = '127.0.0.1:" + strconv.Itoa(pgPort+1000) + "'\n" +
		"pg_auth_type = 'Trust'\n" +
		"http_auth_type = 'Trust'\n" +
		"grpc_auth_type = 'Trust'\n"
	if err := os.WriteFile(filepath.Join(dir, "pageserver.toml"), []byte(psToml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverPageservers(t *testing.T) {
	tmp := t.TempDir()
	endpoints := filepath.Join(tmp, "endpoints")
	writePageserverDir(t, endpoints, "main", 64000)
	writePageserverDir(t, endpoints, "ep_b1", 64002)
	// An endpoint without a pageserver/ subdir is skipped.
	if err := os.MkdirAll(filepath.Join(endpoints, "ro", "pgdata"), 0o755); err != nil {
		t.Fatal(err)
	}

	found, err := DiscoverPageservers(endpoints)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("want 2, got %d", len(found))
	}
	// Pageservers are keyed by their datadir (endpoint), not a node id.
	byDir := map[string]PageServerConf{}
	for _, p := range found {
		byDir[p.DataDir] = p
	}
	main := byDir[filepath.Join(endpoints, "main", "pageserver")]
	if main.ListenPgAddr != "127.0.0.1:64000" {
		t.Errorf("main listen_pg_addr: %q", main.ListenPgAddr)
	}
	if main.PgAuthType != AuthTrust {
		t.Errorf("main pg_auth_type: %q", main.PgAuthType)
	}
	if _, ok := byDir[filepath.Join(endpoints, "ep_b1", "pageserver")]; !ok {
		t.Errorf("ep_b1 pageserver not discovered: %+v", found)
	}
}

func TestDiscoverPageserversEmptyWhenNoEndpointsDir(t *testing.T) {
	tmp := t.TempDir()
	found, err := DiscoverPageservers(filepath.Join(tmp, "endpoints"))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("want empty, got %d", len(found))
	}
}

func TestDiscoverPageserversErrorsOnMalformedToml(t *testing.T) {
	tmp := t.TempDir()
	endpoints := filepath.Join(tmp, "endpoints")
	dir := filepath.Join(endpoints, "main", "pageserver")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "pageserver.toml"), []byte("this is not = valid = toml"), 0o644)
	if _, err := DiscoverPageservers(endpoints); err == nil {
		t.Fatal("expected error on malformed pageserver.toml")
	}
}

func TestLoadPersistConfigRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	cfg := fullConfig()
	if err := cfg.Persist(filepath.Join(tmp, "config")); err != nil {
		t.Fatal(err)
	}
	// Provision a pageserver so discovery finds it.
	writePageserverDir(t, filepath.Join(tmp, "endpoints"), "main", 64000)

	env, err := LoadConfig(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if env.ControlPlaneAPI != "http://127.0.0.1:1234/upcall/v1/" {
		t.Errorf("control_plane_api: %q", env.ControlPlaneAPI)
	}
	if len(env.Pageservers) != 1 || env.Pageservers[0].ListenPgAddr != "127.0.0.1:64000" {
		t.Errorf("discovered pageservers: %+v", env.Pageservers)
	}
	if env.Controller.MaxOffline.Duration() != 10*time.Second {
		t.Errorf("controller max_offline: %v", env.Controller.MaxOffline.Duration())
	}
	if env.Controller.Yb == nil || env.Controller.Yb.HostPorts != "10.0.0.1:5433,10.0.0.2:5440" {
		t.Errorf("controller.yb: %+v", env.Controller.Yb)
	}

	// Persist again from the env and confirm the config file round-trips.
	if err := env.PersistConfig(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(filepath.Join(tmp, "config"))
	if err != nil {
		t.Fatal(err)
	}
	original, _ := unmarshalOnDisk(mustRead(t, filepath.Join(tmp, "config")))
	if !reflect.DeepEqual(*original, *reloaded) {
		t.Errorf("persist/reload mismatch")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAuthKeypairAndJWT(t *testing.T) {
	tmp := t.TempDir()
	priv := filepath.Join(tmp, "auth_private_key.pem")
	pub := filepath.Join(tmp, "auth_public_key.pem")
	if err := GenerateAuthKeys(priv, pub); err != nil {
		t.Fatal(err)
	}

	// PEM headers must be the standard PKCS#8 / SPKI labels.
	privPEM := string(mustRead(t, priv))
	pubPEM := string(mustRead(t, pub))
	if !strings.Contains(privPEM, "BEGIN PRIVATE KEY") {
		t.Errorf("private PEM header wrong:\n%s", privPEM)
	}
	if !strings.Contains(pubPEM, "BEGIN PUBLIC KEY") {
		t.Errorf("public PEM header wrong:\n%s", pubPEM)
	}

	env := &LocalEnv{BaseDataDir: tmp, PrivateKeyPath: "auth_private_key.pem", PublicKeyPath: "auth_public_key.pem"}
	claims := NewStorageClaims(ScopeTenant, "3d1f7595b468230304e0b73cecbcb081")
	token, err := env.GenerateAuthToken(claims)
	if err != nil {
		t.Fatal(err)
	}

	// Verify with the public key.
	pubKey, err := env.ReadPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.Parse(token, func(tok *jwt.Token) (any, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodEd25519); !ok {
			t.Fatalf("unexpected signing method: %v", tok.Header["alg"])
		}
		return pubKey, nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	mc := parsed.Claims.(jwt.MapClaims)
	if mc["scope"] != "tenant" || mc["tenant_id"] != "3d1f7595b468230304e0b73cecbcb081" {
		t.Errorf("claims: %+v", mc)
	}
	if parsed.Header["alg"] != "EdDSA" {
		t.Errorf("alg header: %v", parsed.Header["alg"])
	}
}
