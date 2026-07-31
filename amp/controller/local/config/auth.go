package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	"github.com/golang-jwt/jwt/v5"
)

// GenerateAuthKeys writes an ed25519 keypair for JWT auth: a PKCS#8
// private key PEM at privateKeyPath and an SPKI (PKIX) public key PEM at
// publicKeyPath. This reproduces Rust `generate_auth_keys`, which shells
// out to `openssl genpkey -algorithm ed25519` + `openssl pkey -pubout`; a
// native crypto/ed25519 keypair marshaled as PKCS#8 (private) / PKIX
// (public) yields byte-identical key formats to those openssl commands.
//
// IMPORTANT: yb-endpoint-storage (Rust) reads the *public* PEM via
// jsonwebtoken's `DecodingKey::from_ed_pem`, which requires a standard
// SPKI-encoded ed25519 public key. MarshalPKIXPublicKey produces exactly
// that, so the public PEM stays interoperable with the Rust daemons.
func GenerateAuthKeys(privateKeyPath, publicKeyPath string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate ed25519 keypair: %w", err)
	}

	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("marshal private key (PKCS#8): %w", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	if err := os.WriteFile(privateKeyPath, privPEM, 0o600); err != nil {
		return fmt.Errorf("write private key %q: %w", privateKeyPath, err)
	}

	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return fmt.Errorf("marshal public key (PKIX/SPKI): %w", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	if err := os.WriteFile(publicKeyPath, pubPEM, 0o644); err != nil {
		return fmt.Errorf("write public key %q: %w", publicKeyPath, err)
	}
	return nil
}

// readEd25519PrivateKey parses a PKCS#8-encoded ed25519 private key PEM.
func readEd25519PrivateKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key %q: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("private key %q is not PEM-encoded", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key %q: %w", path, err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key %q is not an ed25519 key (got %T)", path, key)
	}
	return priv, nil
}

// ReadPublicKey parses the SPKI-encoded ed25519 public key PEM at
// GetPublicKeyPath.
func (e *LocalEnv) ReadPublicKey() (ed25519.PublicKey, error) {
	path := e.GetPublicKeyPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read public key %q: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("public key %q is not PEM-encoded", path)
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key %q: %w", path, err)
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public key %q is not an ed25519 key (got %T)", path, key)
	}
	return pub, nil
}

// SignAuthToken signs claims into an EdDSA (Ed25519) JWT using the PKCS#8
// ed25519 private key PEM at privateKeyPath. This matches Rust
// `utils::auth::encode_from_key_file` (EdDSA JWT over the same key), so the
// tokens verify against the SPKI public key the Rust daemons load.
func SignAuthToken(privateKeyPath string, claims jwt.Claims) (string, error) {
	priv, err := readEd25519PrivateKey(privateKeyPath)
	if err != nil {
		return "", err
	}
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	signed, err := token.SignedString(priv)
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}
	return signed, nil
}

// GenerateAuthToken signs an EdDSA JWT with this env's private key, used to
// mint compute + endpoint_storage tokens. Mirrors Rust
// `LocalEnv::generate_auth_token`.
func (e *LocalEnv) GenerateAuthToken(claims jwt.Claims) (string, error) {
	return SignAuthToken(e.GetPrivateKeyPath(), claims)
}

// Scope mirrors the token scopes in Rust `utils::auth::Scope` (serde
// rename_all = "lowercase", with two explicit renames). Only the values
// the launcher mints are provided.
type Scope = string

const (
	ScopeTenant         Scope = "tenant"
	ScopeTenantEndpoint Scope = "tenantendpoint"
	ScopePageServerAPI  Scope = "pageserverapi"
	ScopeSafekeeperData Scope = "safekeeperdata"
	ScopeGenerationsAPI Scope = "generations_api"
	ScopeAdmin          Scope = "admin"
	ScopeInfra          Scope = "infra"
	ScopeScrubber       Scope = "scrubber"
	ScopeControllerPeer Scope = "controller_peer"
)

// NewStorageClaims builds the JWT claims payload the storage daemons
// expect (Rust `utils::auth::Claims`). The wire key for the branch stays
// `tenant_id` for JWT compatibility. Pass an empty branchID to omit it.
func NewStorageClaims(scope Scope, branchID BranchID) jwt.MapClaims {
	claims := jwt.MapClaims{"scope": scope}
	if branchID != "" {
		claims["tenant_id"] = branchID
	}
	return claims
}
