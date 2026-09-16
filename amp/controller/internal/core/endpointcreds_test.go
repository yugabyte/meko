package core

import (
	"crypto/md5"
	"fmt"
	"os"
	"strings"
	"testing"
)

// The verifier is the contract between this package and the endpoint
// supervisor's remote_auth, which hands it to Postgres as an already-hashed
// password. Postgres' own construction is md5(password || username), prefixed
// `md5` — get it wrong and the role comes up with a password nobody can
// authenticate with, which no test short of a live connection would notice.
func TestMD5PasswordVerifierMatchesPostgres(t *testing.T) {
	got := MD5PasswordVerifier("hunter2hunter2", ComputeDefaultUser)
	want := fmt.Sprintf("md5%x", md5.Sum([]byte("hunter2hunter2"+ComputeDefaultUser)))
	if got != want {
		t.Errorf("MD5PasswordVerifier = %q, want %q", got, want)
	}
	// The same vector md5_role_password_matches_postgres pins on the Rust side,
	// so the two implementations are checked against one literal rather than
	// against each other's formula.
	if got, want := MD5PasswordVerifier("cloud_admin", "cloud_admin"), "md5b093c0d3b281ba6da1eacc608620abd8"; got != want {
		t.Errorf("MD5PasswordVerifier(cloud_admin, cloud_admin) = %q, want %q", got, want)
	}
	v := MD5PasswordVerifier("anything", ComputeDefaultUser)
	if !strings.HasPrefix(v, "md5") || len(v) != 35 {
		t.Errorf("verifier %q is not md5 + 32 hex digits", v)
	}
}

// The verifier is SALTED with the user name, so this constant is not
// documentation — computing against the wrong name yields a password that
// authenticates nobody. It has to stay equal to the pageserver's
// DEFAULT_SUPERUSER (what initdb ran with) and the supervisor's
// boot::DEFAULT_USER.
func TestComputeDefaultUserIsTheInitdbSuperuser(t *testing.T) {
	if ComputeDefaultUser != "cloud_admin" {
		t.Errorf("ComputeDefaultUser = %q; it must match pageserver_api's DEFAULT_SUPERUSER "+
			"and endpoint_supervisor's boot::DEFAULT_USER, which are both cloud_admin", ComputeDefaultUser)
	}
	if MD5PasswordVerifier("pw", "a") == MD5PasswordVerifier("pw", "b") {
		t.Error("the verifier is not salted by the user name; the constant above would not matter")
	}
}

// The control plane mints no password of its own — the caller must bring one —
// so there is deliberately no generator in this package. A generated credential
// could only be handed back on the create's 202, which is a response some
// callers never read (the cluster wizard discarded it; a YCP-driven create has
// no reader at all), leaving an endpoint nobody can log into.
func TestNoPasswordGenerator(t *testing.T) {
	src, err := os.ReadFile("endpointcreds.go")
	if err != nil {
		t.Fatalf("read endpointcreds.go: %v", err)
	}
	for _, forbidden := range []string{"GenerateEndpointPassword", "crypto/rand"} {
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
	} {
		if err := ValidateEndpointPassword(bad); err == nil {
			t.Errorf("ValidateEndpointPassword(%q) = nil, want an error", bad)
		}
	}
}
