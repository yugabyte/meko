package core

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// ComputeDefaultUser is the Postgres user an endpoint's clients connect as: the
// bootstrap superuser `initdb` created, which owns the catalog.
//
// It is a constant rather than a per-endpoint choice because the name is fixed
// in three places at once — the pageserver's `superuser` setting (what initdb
// was run with), the supervisor's boot::DEFAULT_USER, and a dozen literal
// `cloud_admin` references inside compute_tools. Making it configurable is
// yugabyte/meko#1354; this package will take it as a parameter then.
const ComputeDefaultUser = "cloud_admin"

// Password length bounds, mirrored by CreateEndpointRequest.password's
// minLength/maxLength in the OpenAPI spec.
const (
	MinEndpointPasswordLen = 8
	MaxEndpointPasswordLen = 128
)

// ValidateEndpointPassword reports why password cannot be used, or nil if it can.
//
// Printable ASCII only, which is a SCRAM requirement rather than a preference:
// Postgres runs the password through SASLprep (RFC 4013) before deriving a
// verifier, and so does libpq before computing a client proof.
// SCRAMPasswordVerifier below does not implement SASLprep, so a password whose
// normalized form differs from its bytes would yield a verifier no client can
// authenticate against — surfacing as "password authentication failed" at
// connect time, long after the create returned 202. Over U+0020..U+007E
// SASLprep is the identity function, so restricting to that range makes the two
// agree by construction.
//
// md5 needed none of this (it hashes the raw bytes), so this is the one way the
// SCRAM switch narrows what a caller may send.
func ValidateEndpointPassword(password string) error {
	if n := len(password); n < MinEndpointPasswordLen || n > MaxEndpointPasswordLen {
		return fmt.Errorf("password must be %d-%d characters", MinEndpointPasswordLen, MaxEndpointPasswordLen)
	}
	for _, r := range password {
		if r < 0x20 || r > 0x7e {
			return fmt.Errorf("password must contain only printable ASCII characters (U+0020-U+007E)")
		}
	}
	return nil
}

// SCRAM parameters, both Postgres' own defaults: a verifier computed here and
// one Postgres derives from `ALTER ROLE ... PASSWORD 'plain'` are then
// indistinguishable, so nothing downstream can come to depend on the difference.
const (
	scramIterations = 4096
	scramSaltLen    = 16
)

// SCRAMPasswordVerifier is the password in the form `pg_authid` stores it under
// `password_encryption = scram-sha-256`, which the endpoint's compute spec sets:
//
//	SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>
//
// Postgres recognizes a string of that shape as already hashed and stores it
// verbatim, which is why this — and not the plaintext — is what travels from the
// request to the pod.
//
// Unlike the md5 verifier it replaces, it is not password-equivalent: a client
// proof requires ClientKey and only SHA256(ClientKey) is stored, so the release
// Secret and the create task's step row stop being things that can log in. They
// remain secrets — the verifier still permits an offline dictionary attack, and
// ServerKey forges the server's half of the handshake.
//
// No username is mixed in (md5 salted with it, SCRAM salts randomly), so the
// same password yields a different verifier on every call.
func SCRAMPasswordVerifier(password string) (string, error) {
	salt := make([]byte, scramSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate SCRAM salt: %w", err)
	}
	return scramVerifierWithSalt(password, salt, scramIterations)
}

// scramVerifierWithSalt is SCRAMPasswordVerifier with the salt and iteration
// count supplied, which is what lets a test reproduce a verifier Postgres itself
// wrote (RFC 5802 §3, as pg_be_scram_build_secret writes it).
func scramVerifierWithSalt(password string, salt []byte, iterations int) (string, error) {
	saltedPassword, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("derive SCRAM salted password: %w", err)
	}
	clientKey := scramHMAC(saltedPassword, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := scramHMAC(saltedPassword, "Server Key")

	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s",
		iterations, b64(salt), b64(storedKey[:]), b64(serverKey)), nil
}

func scramHMAC(key []byte, msg string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return mac.Sum(nil)
}
