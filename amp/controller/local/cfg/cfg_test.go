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

	"github.com/google/uuid"
	"github.com/pelletier/go-toml/v2"

	"github.com/yugabyte/amp/controller/local/config"
)

// The values below are the exact inputs of the live `yb_amp_local` bring-up
// that produced testdata/golden/* (init --port-base 30000 --yb-dbname
// amp_golden, then tenant/endpoint create + endpoint start). Hardcoding them
// (rather than reading them out of the goldens) is what makes a wrong field
// NAME/type surface: the writer must reproduce the golden from these
// literals.
var goldBranchID = uuid.MustParse("9a57cdca-fd66-3c91-ab83-654f21378725")

const (
	goldPgDistribDir = "/net/dev-server-hsunder/share/code/amp/pg_install"
	goldControlPlane = "http://127.0.0.1:30000/upcall/v1/"
	goldSafekeeperPg = "127.0.0.1:30100"
	goldYbInlineConn = `{ host_ports = "127.0.0.1:5433", user = "yugabyte", dbname = "amp_golden" }`
	goldEndpointID   = "ep-main"
	goldComputePort  = 55432
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
	// Bake the identity (branch_id), matching the init -> set_timeline_identity
	// sequence. The generation is not configured — the pageserver claims it.
	if err := SetPageserverIdentity(path, goldBranchID); err != nil {
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

func TestSetPageserverIdentityPreservesKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pageserver.toml")
	if err := WritePageserverToml(path, goldPageserverParams()); err != nil {
		t.Fatal(err)
	}
	before := decodeTOML(t, readGolden(t, "pageserver.toml"))
	if err := SetPageserverIdentity(path, goldBranchID); err != nil {
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
	branch := goldBranchID
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

// --- round trips ---

func TestSafekeeperTomlRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "safekeeper.toml")
	branch := goldBranchID
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

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
