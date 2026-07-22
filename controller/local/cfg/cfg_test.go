package cfg

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/yugabyte/amp/controller/local/config"
)

// The values below are the exact inputs of the live `yb_amp_local` bring-up
// that produced testdata/golden/* (init --port-base 30000 --yb-dbname
// amp_golden, then tenant/endpoint create + endpoint start). Hardcoding them
// (rather than reading them out of the goldens) is what makes a wrong field
// NAME/type surface: the writer must reproduce the golden from these
// literals.
const (
	goldBranchID     = "9a57cdcafd663c91ab83654f21378725"
	goldPgDistribDir = "/net/dev-server-hsunder/share/code/amp/pg_install"
	goldControlPlane = "http://127.0.0.1:30000/upcall/v1/"
	goldSafekeeperPg = "127.0.0.1:30100"
	goldYbInlineConn = `{ host_ports = "127.0.0.1:5433", user = "yugabyte", dbname = "amp_golden" }`
	goldPemfile      = "/tmp/claude-2206/-net-dev-server-hsunder-share-code-amp/bca88cc9-be37-4760-9c1f-bbfa36b6fbac/scratchpad/goldenrepo/.yb_amp/auth_public_key.pem"
	goldEndpointID   = "ep-main"
	goldComputePort  = 55432
	goldGeneration   = 2
)

func goldenPath(name string) string { return filepath.Join("testdata", "golden", name) }

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(goldenPath(name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return data
}

// decodeTOML parses TOML bytes into an order-independent map.
func decodeTOML(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode toml: %v\n---\n%s", err, data)
	}
	return m
}

// decodeJSON parses JSON bytes into an order-independent value.
func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode json: %v\n---\n%s", err, data)
	}
	return v
}

func assertEqualTree(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: semantic trees differ\n got: %#v\nwant: %#v", what, got, want)
	}
}

func strPtr(s string) *string { return &s }
func u16Ptr(v uint16) *uint16 { return &v }

// --- pageserver.toml + set-identity ---

func goldPageserverParams() PageserverParams {
	yb := goldYbInlineConn
	return PageserverParams{
		Conf: config.YbAmpLocalInitPageserverConf{
			ListenPgAddr:   "127.0.0.1:30300",
			ListenHTTPAddr: "127.0.0.1:30400",
			ListenGRPCAddr: strPtr("127.0.0.1:30500"),
			PgAuthType:     config.AuthTrust,
			HTTPAuthType:   config.AuthTrust,
			GRPCAuthType:   config.AuthTrust,
			NoSync:         true,
			YbStorageConn:  &yb,
		},
		PgDistribDir:     goldPgDistribDir,
		SafekeeperPgAddr: goldSafekeeperPg,
		ControlPlaneAPI:  goldControlPlane,
	}
}

func TestPageserverTomlGolden(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pageserver.toml")
	if err := WritePageserverToml(path, goldPageserverParams()); err != nil {
		t.Fatalf("WritePageserverToml: %v", err)
	}
	// Bake the identity (branch_id / generation=2 / [timeline_init] bootstrap),
	// matching the init -> set_timeline_identity -> attach sequence.
	init := BootstrapInit(config.DefaultPgVersion)
	if err := SetPageserverIdentity(path, goldBranchID, goldGeneration, &init); err != nil {
		t.Fatalf("SetPageserverIdentity: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertEqualTree(t, "pageserver.toml", decodeTOML(t, got), decodeTOML(t, readGolden(t, "pageserver.toml")))
}

func TestPageserverTomlRequiresYbStorageConn(t *testing.T) {
	p := goldPageserverParams()
	p.Conf.YbStorageConn = nil
	if err := WritePageserverToml(filepath.Join(t.TempDir(), "x.toml"), p); err == nil {
		t.Fatal("expected error when yb_storage_conn is nil")
	}
}

func TestTimelineInitBranchShape(t *testing.T) {
	// The Branch variant carries ancestor_branch_id + ancestor_lsn only.
	m, err := BranchInit("aaaa", "0/169AD58").toMap()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"mode": "branch", "ancestor_branch_id": "aaaa", "ancestor_lsn": "0/169AD58"}
	assertEqualTree(t, "branch timeline_init", m, want)
}

func TestSetPageserverIdentityPreservesKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pageserver.toml")
	if err := WritePageserverToml(path, goldPageserverParams()); err != nil {
		t.Fatal(err)
	}
	before := decodeTOML(t, readGolden(t, "pageserver.toml"))
	init := BootstrapInit(18)
	if err := SetPageserverIdentity(path, goldBranchID, 2, &init); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	m := decodeTOML(t, got)
	// yb_storage_conn (a table) survives the read-modify-write.
	if !reflect.DeepEqual(m["yb_storage_conn"], before["yb_storage_conn"]) {
		t.Errorf("yb_storage_conn not preserved: %#v", m["yb_storage_conn"])
	}
	if m["listen_grpc_addr"] != "127.0.0.1:30500" {
		t.Errorf("listen_grpc_addr not preserved: %#v", m["listen_grpc_addr"])
	}
}

// --- metadata.json ---

func TestMetadataJSONGolden(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	m := NodeMetadata{
		PostgresHost: "localhost",
		PostgresPort: 30300,
		GrpcHost:     strPtr("localhost"),
		GrpcPort:     u16Ptr(30500),
		HTTPHost:     "localhost",
		HTTPPort:     30400,
		HTTPSPort:    nil,
	}
	if err := WriteMetadataJSON(path, m); err != nil {
		t.Fatalf("WriteMetadataJSON: %v", err)
	}
	got, _ := os.ReadFile(path)
	assertEqualTree(t, "metadata.json", decodeJSON(t, got), decodeJSON(t, readGolden(t, "metadata.json")))
}

// --- safekeeper.toml ---

func TestSafekeeperTomlGolden(t *testing.T) {
	path := filepath.Join(t.TempDir(), "safekeeper.toml")
	branch := config.BranchID(goldBranchID)
	yb := goldYbInlineConn
	p := SafekeeperParams{
		BranchID:      &branch,
		ListenAddr:    "127.0.0.1",
		PgPort:        30100,
		HTTPPort:      30200,
		YbStorageConn: &yb,
	}
	if err := WriteSafekeeperToml(path, p); err != nil {
		t.Fatalf("WriteSafekeeperToml: %v", err)
	}
	got, _ := os.ReadFile(path)
	assertEqualTree(t, "safekeeper.toml", decodeTOML(t, got), decodeTOML(t, readGolden(t, "safekeeper.toml")))
}

// --- endpoint_storage.json ---

func TestEndpointStorageJSONGolden(t *testing.T) {
	path := filepath.Join(t.TempDir(), "endpoint_storage.json")
	p := EndpointStorageParams{
		Listen:  "127.0.0.1:30600",
		Pemfile: goldPemfile,
		YbConn: &config.YbConnConfig{
			HostPorts: "127.0.0.1:5433",
			User:      "yugabyte",
			Dbname:    "amp_golden",
		},
	}
	if err := WriteEndpointStorageJSON(path, p); err != nil {
		t.Fatalf("WriteEndpointStorageJSON: %v", err)
	}
	got, _ := os.ReadFile(path)
	assertEqualTree(t, "endpoint_storage.json", decodeJSON(t, got), decodeJSON(t, readGolden(t, "endpoint_storage.json")))
}

// --- config.json (compute spec) + jwks + postgresql.conf ---

func readEd25519PublicKeyPEM(t *testing.T, path string) ed25519.PublicKey {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("pem decode failed")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("not ed25519: %T", key)
	}
	return pub
}

func TestJWKSGolden(t *testing.T) {
	pub := readEd25519PublicKeyPEM(t, goldenPath("auth_public_key.pem"))
	jwks := BuildJWKS(pub)
	// Pull the golden JWKS out of config.json.
	golden := decodeJSON(t, readGolden(t, "config.json")).(map[string]any)
	wantJwks := golden["compute_ctl_config"].(map[string]any)["jwks"]
	gotBytes, _ := json.Marshal(jwks)
	assertEqualTree(t, "jwks", decodeJSON(t, gotBytes), wantJwks)
}

func TestPostgresqlConfGolden(t *testing.T) {
	// Extract the golden postgresql.conf string and rebuild it.
	golden := decodeJSON(t, readGolden(t, "config.json")).(map[string]any)
	wantConf := golden["spec"].(map[string]any)["cluster"].(map[string]any)["postgresql_conf"].(string)

	got := BuildPostgresqlConf(PostgresqlConfParams{
		ListenAddr:             "127.0.0.1",
		Port:                   goldComputePort,
		UseLocalTLS:            false,
		SafekeeperComputePorts: []uint16{30100},
	})
	if got != wantConf {
		t.Errorf("postgresql.conf mismatch:\n got: %q\nwant: %q", got, wantConf)
	}
}

func TestConfigJSONGolden(t *testing.T) {
	golden := decodeJSON(t, readGolden(t, "config.json")).(map[string]any)
	goldenSpec := golden["spec"].(map[string]any)

	// endpoint_storage_token is an opaque passthrough JWT (time-dependent);
	// feed the golden's value back so the compare covers structure, not the
	// signature.
	token := goldenSpec["endpoint_storage_token"].(string)
	pgConf := goldenSpec["cluster"].(map[string]any)["postgresql_conf"].(string)

	pub := readEd25519PublicKeyPEM(t, goldenPath("auth_public_key.pem"))

	cfg, err := BuildComputeConfig(ComputeSpecParams{
		PostgresqlConf:  pgConf,
		CreateTestUser:  false,
		StorageBranchID: goldBranchID,
		EndpointID:      goldEndpointID,
		Mode:            ComputeModePrimary,
		PageserverConnInfo: PageserverConnectionInfo{
			Pageservers: []PageserverConnectionItem{{
				LibpqURL: strPtr("postgres://no_user@127.0.0.1:30300?sslmode=disable"),
				GrpcURL:  strPtr("grpc://no_user@127.0.0.1:30500"),
			}},
			PreferProtocol: PageserverProtocolLibpq,
		},
		PageserverConnstring:  strPtr("postgres://no_user@127.0.0.1:30300?sslmode=disable"),
		SafekeeperConnstrings: []string{"127.0.0.1:30100"},
		SkipPgCatalogUpdates:  true,
		EndpointStorageAddr:   strPtr("127.0.0.1:30600"),
		EndpointStorageToken:  &token,
		ComputeCtlConfig:      ComputeCtlConfig{Jwks: BuildJWKS(pub)},
	})
	if err != nil {
		t.Fatalf("BuildComputeConfig: %v", err)
	}
	if err := WriteComputeConfigJSON(filepath.Join(t.TempDir(), "config.json"), cfg); err != nil {
		t.Fatalf("WriteComputeConfigJSON: %v", err)
	}
	gotBytes, _ := json.Marshal(cfg)
	assertEqualTree(t, "config.json", decodeJSON(t, gotBytes), golden)
}

func TestBuildComputeConfigRejectsNonPrimary(t *testing.T) {
	if _, err := BuildComputeConfig(ComputeSpecParams{Mode: "Replica"}); err == nil {
		t.Fatal("expected error for non-Primary mode")
	}
}

// --- round trips ---

func TestSafekeeperTomlRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "safekeeper.toml")
	branch := config.BranchID(goldBranchID)
	tenantOnly := uint16(30150)
	https := uint16(30250)
	wal := uint32(128)
	yb := goldYbInlineConn
	authKey := "/keys/auth_public_key.pem"
	p := SafekeeperParams{
		BranchID:          &branch,
		ListenAddr:        "127.0.0.1",
		PgPort:            30100,
		PgTenantOnlyPort:  &tenantOnly,
		HTTPPort:          30200,
		HTTPSPort:         &https,
		YbStorageConn:     &yb,
		WalCacheSizeMib:   &wal,
		AuthPublicKeyPath: &authKey,
		AuthTokenPath:     strPtr("/data/peer_jwt_token"),
	}
	if err := WriteSafekeeperToml(path, p); err != nil {
		t.Fatal(err)
	}
	m := decodeTOML(t, mustRead(t, path))
	// optional fields present when set
	for _, k := range []string{"listen_pg_tenant_only", "listen_https", "wal_cache_size_mib",
		"pg_auth_public_key_path", "pg_tenant_only_auth_public_key_path", "http_auth_public_key_path", "auth_token_path"} {
		if _, ok := m[k]; !ok {
			t.Errorf("expected key %q present", k)
		}
	}
}

func TestEndpointStorageVerbatimJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "es.json")
	raw := `{"type":"YbStore","host_ports":"h:1","user":"u","dbname":"d"}`
	p := EndpointStorageParams{Listen: "127.0.0.1:9993", Pemfile: "/k.pem", RemoteStorageJSON: &raw}
	if err := WriteEndpointStorageJSON(path, p); err != nil {
		t.Fatal(err)
	}
	m := decodeJSON(t, mustRead(t, path)).(map[string]any)
	sk := m["yb_storage_conn"].(map[string]any)
	if sk["type"] != "YbStore" || sk["host_ports"] != "h:1" {
		t.Errorf("verbatim storage kind wrong: %#v", sk)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
