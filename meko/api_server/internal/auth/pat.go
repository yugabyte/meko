package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strings"
)

// PAT format constants.
//
// The plaintext token has the shape:
//
//	mko_tkn_<base32-of-24-random-bytes>
//
// 24 random bytes -> 38.4 chars of base32 (no padding) gives ~192
// bits of entropy, which is well past collision-attack territory.
//
// PATPrefixDisplayLen is the leading slice we store in
// `token_prefix` and surface in the UI. 16 chars = "mko_tkn_" + 8
// random base32 chars, enough for visual identification without
// risking pre-image reconstruction (it's still SHA-256 hashed
// end-to-end).
const (
	// PATScheme is the prefix that identifies a meko personal access
	// token. The MCP server can fast-reject any bearer that doesn't
	// start with this without hitting the DB.
	PATScheme = "mko_tkn_"

	// PATRandomBytes is the entropy budget per token before base32
	// encoding.
	PATRandomBytes = 24

	// PATPrefixDisplayLen is the length of the human-visible prefix
	// stored alongside the hash. Must be >= len(PATScheme).
	PATPrefixDisplayLen = 16

	// PATHashLen is the length in hex chars of the SHA-256 digest
	// stored in token_hash. Used as a sanity check on inputs from the
	// repo layer.
	PATHashLen = 64
)

// patEncoding is unpadded base32 (lowercase, with underscore as the
// padding placeholder swapped out). We use the no-padding RFC 4648
// alphabet so the resulting token has no '=' chars and is URL/header
// safe without escaping.
var patEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// GeneratePAT mints a new opaque personal access token.
//
// Returns:
//   - plaintext: the full token to hand to the user once. Format
//     "mko_tkn_<base32>".
//   - prefix: the leading PATPrefixDisplayLen chars of plaintext for
//     display/audit purposes.
//   - hash: the lowercase hex SHA-256 of plaintext for storage in
//     mcp_personal_access_tokens.token_hash.
//
// The plaintext must NEVER be persisted -- the caller is expected to
// drop it from memory after returning the create-once response.
func GeneratePAT() (plaintext, prefix, hash string, err error) {
	buf := make([]byte, PATRandomBytes)
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", fmt.Errorf("pat: read random: %w", err)
	}

	body := strings.ToLower(patEncoding.EncodeToString(buf))
	plaintext = PATScheme + body

	if len(plaintext) < PATPrefixDisplayLen {
		// Should be impossible given PATRandomBytes >= 16, but guard
		// against a future tweak silently truncating the prefix.
		return "", "", "", fmt.Errorf(
			"pat: generated token shorter than prefix (%d < %d)",
			len(plaintext), PATPrefixDisplayLen,
		)
	}
	prefix = plaintext[:PATPrefixDisplayLen]
	hash = HashPAT(plaintext)
	return plaintext, prefix, hash, nil
}

// HashPAT returns the lowercase hex SHA-256 digest of plaintext. The
// MCP server uses this exact function on the inbound bearer header to
// produce the lookup key for mcp_pats_token_hash_idx.
func HashPAT(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// IsPATFormat returns true if s syntactically looks like a meko PAT.
// This is a pre-DB filter only -- it tells callers "don't bother
// hashing and querying" for tokens that obviously aren't PATs (e.g. a
// stray Cognito JWT).
func IsPATFormat(s string) bool {
	if !strings.HasPrefix(s, PATScheme) {
		return false
	}
	body := s[len(PATScheme):]
	if body == "" {
		return false
	}
	// base32 alphabet with the StdEncoding ruleset, lowercased.
	for _, r := range body {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '2' && r <= '7':
		default:
			return false
		}
	}
	return true
}

// SecureCompareHashes compares two hex-encoded SHA-256 digests in
// constant time. Provided for the MCP server team -- the api_server
// itself relies on the indexed equality lookup in `WHERE token_hash
// = $1`, but any code path that needs to compare two hex hashes in
// memory MUST use this helper to avoid timing leaks.
func SecureCompareHashes(a, b string) bool {
	ab := []byte(a)
	bb := []byte(b)
	if len(ab) != len(bb) {
		return false
	}
	return subtle.ConstantTimeCompare(ab, bb) == 1
}
