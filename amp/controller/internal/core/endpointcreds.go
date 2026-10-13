package core

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
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

// Plaintext password length bounds. The OpenAPI spec mirrors the minimum; its
// maxLength is higher because the field also accepts a verifier
// (EndpointPasswordVerifier).
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

// ErrInvalidPassword is wrapped by every EndpointPasswordVerifier error that is
// the caller's fault (a 400). Any other error from it is the controller's own.
var ErrInvalidPassword = errors.New("invalid password")

// invalidPasswordError keeps the sentinel's text out of the message a caller is
// shown.
type invalidPasswordError struct{ err error }

func (e invalidPasswordError) Error() string   { return e.err.Error() }
func (e invalidPasswordError) Unwrap() []error { return []error{ErrInvalidPassword, e.err} }

// EndpointPasswordVerifier turns a caller's password into the verifier an
// endpoint is deployed with. A well-formed SCRAM-SHA-256 verifier is returned
// unchanged, which lets a caller set a password without the plaintext ever
// reaching the control plane. Input starting SCRAM-SHA-256$ is always judged as
// a verifier, so a malformed one is refused rather than hashed: as a plaintext it
// would make a string the caller holds as a hash, not a secret, its password.
// Anything else is a plaintext, validated by ValidateEndpointPassword and hashed.
//
// An error wrapping ErrInvalidPassword is the input's; any other is a failure to
// draw the salt.
func EndpointPasswordVerifier(input string) (string, error) {
	if strings.HasPrefix(input, scramPrefix) {
		if err := checkSCRAMVerifier(input); err != nil {
			return "", invalidPasswordError{err}
		}
		return input, nil
	}
	if err := ValidateEndpointPassword(input); err != nil {
		return "", invalidPasswordError{err}
	}
	return SCRAMPasswordVerifier(input)
}

const scramPrefix = "SCRAM-SHA-256$"

// maxSCRAMVerifierLen is the `password` field's maxLength in the OpenAPI spec,
// which nothing on the wire enforces. Without it a salt of any size passes, and
// the root's first writer makes that verifier its password for good: past ~2KB libpq
// refuses the server's SCRAM message, past 128KiB no pod can exec with it in its
// environment.
const maxSCRAMVerifierLen = 256

// checkSCRAMVerifier reports why v is not a verifier the controller accepts, or
// nil. The shape is what Postgres' parse_scram_secret accepts, so anything
// passing here is stored as a verifier rather than hashed again as a password.
// The floors on the iteration count and the salt are the values
// SCRAMPasswordVerifier writes, so a caller's verifier is no weaker than one
// computed here.
func checkSCRAMVerifier(v string) error {
	rest, ok := strings.CutPrefix(v, scramPrefix)
	if !ok {
		return errors.New("not a SCRAM-SHA-256 verifier")
	}
	if len(v) > maxSCRAMVerifierLen {
		return fmt.Errorf("a SCRAM-SHA-256 verifier must be at most %d characters", maxSCRAMVerifierLen)
	}
	malformed := fmt.Errorf("a SCRAM-SHA-256 verifier must be %s<iterations>:<salt>$<StoredKey>:<ServerKey>", scramPrefix)
	// Base64 decoding skips CR and LF, so without this a verifier with a line
	// break in it would pass and reach the Secret and the pod's env verbatim.
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return malformed
		}
	}
	params, keys, ok := strings.Cut(rest, "$")
	if !ok {
		return malformed
	}
	iterStr, saltB64, ok := strings.Cut(params, ":")
	if !ok || iterStr == "" || strings.Trim(iterStr, "0123456789") != "" {
		return malformed
	}
	// Postgres reads the count into an int, so a larger one would be truncated
	// into a different verifier rather than refused.
	iterations, err := strconv.ParseInt(iterStr, 10, 64)
	if err != nil || iterations > math.MaxInt32 {
		return errors.New("SCRAM-SHA-256 verifier iteration count is out of range")
	}
	if iterations < scramIterations {
		return fmt.Errorf("SCRAM-SHA-256 verifier iteration count must be at least %d", scramIterations)
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return errors.New("SCRAM-SHA-256 verifier salt is not valid base64")
	}
	if len(salt) < scramSaltLen {
		return fmt.Errorf("SCRAM-SHA-256 verifier salt must be at least %d bytes", scramSaltLen)
	}
	storedB64, serverB64, ok := strings.Cut(keys, ":")
	if !ok {
		return malformed
	}
	for _, k := range []struct{ name, b64 string }{{"StoredKey", storedB64}, {"ServerKey", serverB64}} {
		key, err := base64.StdEncoding.DecodeString(k.b64)
		if err != nil || len(key) != sha256.Size {
			return fmt.Errorf("SCRAM-SHA-256 verifier %s must be %d bytes, base64-encoded", k.name, sha256.Size)
		}
	}
	return nil
}

func scramHMAC(key []byte, msg string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return mac.Sum(nil)
}
