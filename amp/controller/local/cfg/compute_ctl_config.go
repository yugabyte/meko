package cfg

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
)

// The compute spec itself is rendered by the endpoint supervisor, on every
// deploy path including this one — see launch.EndpointStart. What stays here is
// the part of `compute_ctl`'s config the launcher alone can produce: the key set
// it validates bearer tokens against, built from this environment's own keypair,
// which `supervisorEnv` passes on as COMPUTE_CTL_JWKS_JSON.

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

// The endpoint row's stored `extended_config` carries these, so they describe a
// persisted shape rather than anything still rendered here: an endpoint created
// before the supervisor took over spec rendering must still deserialize.

// ComputeMode strings (compute_api::spec::ComputeMode). Only Primary is in
// scope; Static/Replica are not reproduced.
const ComputeModePrimary = "Primary"

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
