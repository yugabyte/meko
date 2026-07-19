package cfg

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/yugabyte/amp/controller/local/config"
)

// ComputeMode strings (compute_api::spec::ComputeMode). Only Primary is in
// scope for the minimal port; Static/Replica are not reproduced.
const ComputeModePrimary = "Primary"

// ComputeAudit strings (compute_api::spec::ComputeAudit). The launcher
// always writes Disabled.
const ComputeAuditDisabled = "Disabled"

// PageserverProtocol wire strings (compute_api::spec::PageserverProtocol,
// serde-renamed to lowercase).
const (
	PageserverProtocolLibpq = "libpq"
	PageserverProtocolGrpc  = "grpc"
)

// ComputeFeature is a compute_ctl feature flag (serde snake_case). The
// launcher's minimal path passes none.
type ComputeFeature = string

// GenericOption mirrors compute_api::spec::GenericOption.
type GenericOption struct {
	Name    string  `json:"name"`
	Value   *string `json:"value"`
	Vartype string  `json:"vartype"`
}

// Role mirrors compute_api::spec::Role. Options is Rust GenericOptions
// (Option<Vec>): nil serializes as null.
type Role struct {
	Name              string          `json:"name"`
	EncryptedPassword *string         `json:"encrypted_password"`
	Options           []GenericOption `json:"options"`
}

// Database mirrors compute_api::spec::Database. restrict_conn/invalid have
// serde skip_deserializing but ARE serialized, so they are always emitted.
type Database struct {
	Name         string          `json:"name"`
	Owner        string          `json:"owner"`
	Options      []GenericOption `json:"options"`
	RestrictConn bool            `json:"restrict_conn"`
	Invalid      bool            `json:"invalid"`
}

// Cluster mirrors compute_api::spec::Cluster.
type Cluster struct {
	ClusterID      *string         `json:"cluster_id"`
	Name           *string         `json:"name"`
	State          *string         `json:"state"`
	Roles          []Role          `json:"roles"`
	Databases      []Database      `json:"databases"`
	PostgresqlConf *string         `json:"postgresql_conf"`
	Settings       []GenericOption `json:"settings"`
}

// PageserverConnectionItem mirrors compute_api::spec::PageserverConnectionItem.
type PageserverConnectionItem struct {
	ID       *config.NodeID `json:"id"`
	LibpqURL *string        `json:"libpq_url"`
	GrpcURL  *string        `json:"grpc_url"`
}

// PageserverConnectionInfo mirrors compute_api::spec::PageserverConnectionInfo.
type PageserverConnectionInfo struct {
	Pageservers    []PageserverConnectionItem `json:"pageservers"`
	PreferProtocol string                     `json:"prefer_protocol"`
}

// ComputeSpec mirrors compute_api::spec::ComputeSpec for the Primary path.
// The Rust struct has no struct-level skip rules, so EVERY field is
// serialized — the Option fields become JSON null when nil (pointers with NO
// omitempty). Vec fields become `[]` when empty (non-nil slices). Field
// order matches the Rust struct.
type ComputeSpec struct {
	FormatVersion             float64                   `json:"format_version"`
	OperationUUID             *string                   `json:"operation_uuid"`
	Features                  []ComputeFeature          `json:"features"`
	SwapSizeBytes             *uint64                   `json:"swap_size_bytes"`
	DiskQuotaBytes            *uint64                   `json:"disk_quota_bytes"`
	DisableLfcResizing        *bool                     `json:"disable_lfc_resizing"`
	Cluster                   Cluster                   `json:"cluster"`
	DeltaOperations           *json.RawMessage          `json:"delta_operations"`
	SkipPgCatalogUpdates      bool                      `json:"skip_pg_catalog_updates"`
	StorageBranchID           *config.BranchID          `json:"storage_branch_id"`
	PageserverConnectionInfo  *PageserverConnectionInfo `json:"pageserver_connection_info"`
	PageserverConnstring      *string                   `json:"pageserver_connstring"`
	ProjectID                 *string                   `json:"project_id"`
	BranchID                  *string                   `json:"branch_id"`
	EndpointID                *string                   `json:"endpoint_id"`
	SafekeeperConnstrings     []string                  `json:"safekeeper_connstrings"`
	Mode                      string                    `json:"mode"`
	StorageAuthToken          *string                   `json:"storage_auth_token"`
	RemoteExtensions          *json.RawMessage          `json:"remote_extensions"`
	PgbouncerSettings         *json.RawMessage          `json:"pgbouncer_settings"`
	LocalProxyConfig          *json.RawMessage          `json:"local_proxy_config"`
	ReconfigureConcurrency    int                       `json:"reconfigure_concurrency"`
	DropSubscriptionsBefore   bool                      `json:"drop_subscriptions_before_start"`
	AuditLogLevel             string                    `json:"audit_log_level"`
	LogsExportHost            *string                   `json:"logs_export_host"`
	EndpointStorageAddr       *string                   `json:"endpoint_storage_addr"`
	EndpointStorageToken      *string                   `json:"endpoint_storage_token"`
	Autoprewarm               bool                      `json:"autoprewarm"`
	OffloadLfcIntervalSeconds *uint64                   `json:"offload_lfc_interval_seconds"`
	SuspendTimeoutSeconds     int64                     `json:"suspend_timeout_seconds"`
	DatabricksSettings        *json.RawMessage          `json:"databricks_settings"`
}

// TlsConfig mirrors compute_api::responses::TlsConfig.
type TlsConfig struct {
	KeyPath  string `json:"key_path"`
	CertPath string `json:"cert_path"`
}

// Jwk is one JSON Web Key. Field names/renames match jsonwebtoken's Jwk
// (CommonParameters flattened with the OKP AlgorithmParameters); the
// None-valued common fields are skipped (omitempty). For the launcher's
// Ed25519 signing key, use/key_ops/alg/kid are all present.
type Jwk struct {
	Use    string   `json:"use,omitempty"`
	KeyOps []string `json:"key_ops,omitempty"`
	Alg    string   `json:"alg,omitempty"`
	Kid    string   `json:"kid,omitempty"`
	Kty    string   `json:"kty"`
	Crv    string   `json:"crv"`
	X      string   `json:"x"`
}

// JwkSet mirrors jsonwebtoken::jwk::JwkSet (`{"keys": [...]}`).
type JwkSet struct {
	Keys []Jwk `json:"keys"`
}

// ComputeCtlConfig mirrors compute_api::responses::ComputeCtlConfig.
type ComputeCtlConfig struct {
	Jwks JwkSet     `json:"jwks"`
	Tls  *TlsConfig `json:"tls"`
}

// ComputeConfig mirrors compute_api::responses::ComputeConfig — the body of
// config.json. spec is Some in the attached case.
type ComputeConfig struct {
	Spec             *ComputeSpec     `json:"spec"`
	ComputeCtlConfig ComputeCtlConfig `json:"compute_ctl_config"`
}

// BuildJWKS reproduces Rust `ComputeControlPlane::create_jwks_from_pem`: a
// single Ed25519 (OKP) verify key whose `kid` is the base64url(no-pad)
// SHA-256 of the raw 32-byte public key and whose `x` is the base64url
// (no-pad) raw public key. The input is the 32-byte ed25519 public key (the
// SPKI subject public key the Rust code extracts from the PEM).
func BuildJWKS(pub ed25519.PublicKey) JwkSet {
	raw := []byte(pub)
	sum := sha256.Sum256(raw)
	enc := base64.RawURLEncoding
	return JwkSet{
		Keys: []Jwk{{
			Use:    "sig",
			KeyOps: []string{"verify"},
			Alg:    "EdDSA",
			Kid:    enc.EncodeToString(sum[:]),
			Kty:    "OKP",
			Crv:    "Ed25519",
			X:      enc.EncodeToString(raw),
		}},
	}
}

// ComputeSpecParams is the input to BuildComputeConfig — the values the Rust
// `Endpoint::start` threads into the ComputeSpec (control_plane/src/endpoint.rs
// ~785-876) for the Primary path. Fields the minimal launcher always fixes
// (format_version, suspend_timeout, audit level, and the many null options)
// are filled by BuildComputeConfig, not exposed here.
type ComputeSpecParams struct {
	// PostgresqlConf is the in-spec postgresql.conf (from BuildPostgresqlConf),
	// placed at cluster.postgresql_conf.
	PostgresqlConf string
	// CreateTestUser adds the "test" role + "ybampdb" database (Rust
	// args.create_test_user).
	CreateTestUser bool
	// StorageBranchID is the 128-bit storage-layer branch id.
	StorageBranchID config.BranchID
	EndpointID      string
	// Mode must be ComputeModePrimary (the only supported mode).
	Mode string
	// PageserverConnInfo is the pageserver connection block.
	PageserverConnInfo PageserverConnectionInfo
	// PageserverConnstring is the legacy comma-joined libpq string (or nil).
	PageserverConnstring *string
	// SafekeeperConnstrings are the "127.0.0.1:<compute_port>" safekeeper
	// addresses.
	SafekeeperConnstrings []string
	// StorageAuthToken is the JWT the compute uses for storage (nil in the
	// Trust path).
	StorageAuthToken *string
	// SkipPgCatalogUpdates mirrors the endpoint's skip flag (true for the
	// harness's `endpoint start` without --update-catalog).
	SkipPgCatalogUpdates         bool
	DropSubscriptionsBeforeStart bool
	// ReconfigureConcurrency defaults to 1 when zero.
	ReconfigureConcurrency    int
	EndpointStorageAddr       *string
	EndpointStorageToken      *string
	Autoprewarm               bool
	OffloadLfcIntervalSeconds *uint64
	// Features is usually empty; serialized as [].
	Features []ComputeFeature
	// ComputeCtlConfig carries the JWKS + TLS (build the JWKS with BuildJWKS).
	ComputeCtlConfig ComputeCtlConfig
}

// BuildComputeConfig builds the ComputeConfig written to config.json for a
// Primary compute. Port of the ComputeSpec construction in Rust
// `Endpoint::start`.
func BuildComputeConfig(p ComputeSpecParams) (ComputeConfig, error) {
	if p.Mode != ComputeModePrimary {
		return ComputeConfig{}, fmt.Errorf(
			"cfg.BuildComputeConfig only supports Primary mode, got %q", p.Mode)
	}

	var roles []Role
	var databases []Database
	if p.CreateTestUser {
		roles = []Role{{Name: "test"}}
		databases = []Database{{Name: "ybampdb", Owner: "test"}}
	} else {
		roles = []Role{}
		databases = []Database{}
	}

	features := p.Features
	if features == nil {
		features = []ComputeFeature{}
	}
	safekeepers := p.SafekeeperConnstrings
	if safekeepers == nil {
		safekeepers = []string{}
	}
	reconfigure := p.ReconfigureConcurrency
	if reconfigure == 0 {
		reconfigure = 1
	}

	pgConf := p.PostgresqlConf
	storageBranch := p.StorageBranchID
	endpointID := p.EndpointID
	connInfo := p.PageserverConnInfo

	spec := &ComputeSpec{
		FormatVersion:      1.0,
		OperationUUID:      nil,
		Features:           features,
		SwapSizeBytes:      nil,
		DiskQuotaBytes:     nil,
		DisableLfcResizing: nil,
		Cluster: Cluster{
			ClusterID:      nil,
			Name:           nil,
			State:          nil,
			Roles:          roles,
			Databases:      databases,
			PostgresqlConf: &pgConf,
			Settings:       nil,
		},
		DeltaOperations:           nil,
		SkipPgCatalogUpdates:      p.SkipPgCatalogUpdates,
		StorageBranchID:           &storageBranch,
		PageserverConnectionInfo:  &connInfo,
		PageserverConnstring:      p.PageserverConnstring,
		ProjectID:                 nil,
		BranchID:                  nil,
		EndpointID:                &endpointID,
		SafekeeperConnstrings:     safekeepers,
		Mode:                      ComputeModePrimary,
		StorageAuthToken:          p.StorageAuthToken,
		RemoteExtensions:          nil,
		PgbouncerSettings:         nil,
		LocalProxyConfig:          nil,
		ReconfigureConcurrency:    reconfigure,
		DropSubscriptionsBefore:   p.DropSubscriptionsBeforeStart,
		AuditLogLevel:             ComputeAuditDisabled,
		LogsExportHost:            nil,
		EndpointStorageAddr:       p.EndpointStorageAddr,
		EndpointStorageToken:      p.EndpointStorageToken,
		Autoprewarm:               p.Autoprewarm,
		OffloadLfcIntervalSeconds: p.OffloadLfcIntervalSeconds,
		SuspendTimeoutSeconds:     -1,
		DatabricksSettings:        nil,
	}

	return ComputeConfig{Spec: spec, ComputeCtlConfig: p.ComputeCtlConfig}, nil
}

// WriteComputeConfigJSON writes config.json. Mirrors Rust
// `serde_json::to_string_pretty(&config)` (2-space indent).
func WriteComputeConfigJSON(path string, c ComputeConfig) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize config.json: %w", err)
	}
	return writeFile(path, data)
}
