package core

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The published SCRAM-SHA-256 test vector (RFC 7677 §3): password `pencil`,
// this salt, 4096 iterations. Pinning it checks the derivation against the
// standard rather than against itself — a verifier built from our own formula
// and compared to our own formula would pass however wrong both were, and the
// failure mode here is silent (a role with a password nobody can authenticate
// with, which no test short of a live connection would notice).
const (
	rfc7677Password  = "pencil"
	rfc7677SaltB64   = "W22ZaJ0SNY7soEsUEjb6gQ=="
	rfc7677StoredKey = "WG5d8oPm3OtcPnkdi4Uo7BkeZkBFzpcXkuLmtbsT4qY="
	rfc7677ServerKey = "wfPLwcE6nTWhTAmQ7tl2KeoiWGPlZqQxSrmfPwDl2dU="
)

func TestSCRAMPasswordVerifierMatchesTheRFCVector(t *testing.T) {
	salt, err := base64.StdEncoding.DecodeString(rfc7677SaltB64)
	if err != nil {
		t.Fatalf("decode the vector's salt: %v", err)
	}
	got, err := scramVerifierWithSalt(rfc7677Password, salt, 4096)
	if err != nil {
		t.Fatalf("scramVerifierWithSalt: %v", err)
	}
	want := fmt.Sprintf("SCRAM-SHA-256$4096:%s$%s:%s",
		rfc7677SaltB64, rfc7677StoredKey, rfc7677ServerKey)
	if got != want {
		t.Errorf("verifier =\n  %q\nwant\n  %q", got, want)
	}
}

// The shape is the contract with Postgres: it stores a string of this form
// verbatim, and silently hashes anything else AS the password — so a malformed
// verifier does not fail, it makes the verifier itself the password. Everything
// this checks is what `parse_scram_secret` checks on the other side.
func TestSCRAMPasswordVerifierShape(t *testing.T) {
	v, err := SCRAMPasswordVerifier("hunter2hunter2")
	if err != nil {
		t.Fatalf("SCRAMPasswordVerifier: %v", err)
	}
	rest, ok := strings.CutPrefix(v, "SCRAM-SHA-256$")
	if !ok {
		t.Fatalf("verifier %q does not start with SCRAM-SHA-256$", v)
	}
	params, keys, ok := strings.Cut(rest, "$")
	if !ok {
		t.Fatalf("verifier %q has no $ between parameters and keys", v)
	}
	iterStr, saltB64, ok := strings.Cut(params, ":")
	if !ok {
		t.Fatalf("verifier %q has no <iterations>:<salt>", v)
	}
	if iters, err := strconv.Atoi(iterStr); err != nil || iters != scramIterations {
		t.Errorf("iterations = %q, want %d", iterStr, scramIterations)
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil || len(salt) != scramSaltLen {
		t.Errorf("salt %q: decoded %d bytes (err %v), want %d", saltB64, len(salt), err, scramSaltLen)
	}
	storedB64, serverB64, ok := strings.Cut(keys, ":")
	if !ok {
		t.Fatalf("verifier %q has no <StoredKey>:<ServerKey>", v)
	}
	for name, b64 := range map[string]string{"StoredKey": storedB64, "ServerKey": serverB64} {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(raw) != 32 {
			t.Errorf("%s %q: decoded %d bytes (err %v), want 32", name, b64, len(raw), err)
		}
	}
}

// Salted randomly, not by user name as md5 was. Two consequences worth pinning:
// a verifier cannot be recomputed from the password alone (so nothing may
// compare one to a freshly derived one), and the same password deployed twice
// leaks nothing about the repeat.
func TestSCRAMPasswordVerifierSaltsRandomly(t *testing.T) {
	a, err := SCRAMPasswordVerifier("hunter2hunter2")
	if err != nil {
		t.Fatalf("SCRAMPasswordVerifier: %v", err)
	}
	b, err := SCRAMPasswordVerifier("hunter2hunter2")
	if err != nil {
		t.Fatalf("SCRAMPasswordVerifier: %v", err)
	}
	if a == b {
		t.Error("two verifiers for one password are identical; the salt is not random")
	}
}

// The plaintext must not survive into what the control plane keeps: this string
// reaches the create task's step row and the release Secret.
func TestSCRAMPasswordVerifierDoesNotCarryThePlaintext(t *testing.T) {
	const password = "correct-horse-battery"
	v, err := SCRAMPasswordVerifier(password)
	if err != nil {
		t.Fatalf("SCRAMPasswordVerifier: %v", err)
	}
	if strings.Contains(v, password) {
		t.Errorf("verifier %q contains the plaintext", v)
	}
}

// The name has to stay equal to the pageserver's DEFAULT_SUPERUSER (what initdb
// ran with) and the supervisor's boot::DEFAULT_USER. Under md5 this constant was
// also an input to the verifier; under SCRAM it is not, and the only thing that
// still depends on it is remote_auth's `ALTER ROLE cloud_admin`, which names the
// role literally.
func TestComputeDefaultUserIsTheInitdbSuperuser(t *testing.T) {
	if ComputeDefaultUser != "cloud_admin" {
		t.Errorf("ComputeDefaultUser = %q; it must match pageserver_api's DEFAULT_SUPERUSER "+
			"and endpoint_supervisor's boot::DEFAULT_USER, which are both cloud_admin", ComputeDefaultUser)
	}
}

// The control plane mints no password of its own — the caller must bring one —
// so there is deliberately no generator in this package. A generated credential
// could only be handed back on the create's 202, which is a response some
// callers never read (the cluster wizard discarded it; a YCP-driven create has
// no reader at all), leaving an endpoint nobody can log into.
//
// This used to assert that `crypto/rand` was absent, as a proxy. SCRAM salts
// randomly, so the proxy would now fail for the wrong reason and the name itself
// is what is checked.
func TestNoPasswordGenerator(t *testing.T) {
	src, err := os.ReadFile("endpointcreds.go")
	if err != nil {
		t.Fatalf("read endpointcreds.go: %v", err)
	}
	for _, forbidden := range []string{"GenerateEndpointPassword", "func generatePassword"} {
		if strings.Contains(string(src), forbidden) {
			t.Errorf("endpointcreds.go references %q — the controller must not mint "+
				"endpoint passwords; a caller supplies one or the create is a 400", forbidden)
		}
	}
}

func TestValidateEndpointPassword(t *testing.T) {
	if err := ValidateEndpointPassword(strings.Repeat("a", MinEndpointPasswordLen)); err != nil {
		t.Errorf("a password at the minimum length was rejected: %v", err)
	}
	if err := ValidateEndpointPassword(strings.Repeat("a", MaxEndpointPasswordLen)); err != nil {
		t.Errorf("a password at the maximum length was rejected: %v", err)
	}
	// A quote, a space and a backslash all reach Postgres as bytes of a verifier
	// input, never as SQL, so none of them is a reason to refuse a password.
	if err := ValidateEndpointPassword(`p'a ss\"w0rd`); err != nil {
		t.Errorf("an awkward but printable password was rejected: %v", err)
	}
	for _, bad := range []string{
		"",
		strings.Repeat("a", MinEndpointPasswordLen-1),
		strings.Repeat("a", MaxEndpointPasswordLen+1),
		"has\na newline",
		"has\ta tab",
		"has\x00a nul",
		// Non-ASCII is refused because SASLprep would normalize it on the client
		// and this package does not, so the verifier would authenticate nobody.
		"pässwort-nonascii",
		"пароль-cyrillic",
		"emoji-\U0001F511-key",
		"nbsp space",
	} {
		if err := ValidateEndpointPassword(bad); err == nil {
			t.Errorf("ValidateEndpointPassword(%q) = nil, want an error", bad)
		}
	}
}
