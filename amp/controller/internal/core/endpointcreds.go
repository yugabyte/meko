package core

import (
	"crypto/md5"
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
//
// It is load-bearing here and not merely informational: an md5 verifier is
// salted with the username, so computing one against the wrong name produces a
// password nobody can authenticate with.
const ComputeDefaultUser = "cloud_admin"

// Password length bounds, mirrored by CreateEndpointRequest.password's
// minLength/maxLength in the OpenAPI spec.
const (
	MinEndpointPasswordLen = 8
	MaxEndpointPasswordLen = 128
)

// ValidateEndpointPassword reports why password cannot be used, or nil if it can.
// Control characters are the only content restriction: the password reaches the
// compute as a verifier computed here, never as SQL and never as a shell word,
// so what is left to guard is that it survives an HTTP request and a process
// environment.
func ValidateEndpointPassword(password string) error {
	if n := len(password); n < MinEndpointPasswordLen || n > MaxEndpointPasswordLen {
		return fmt.Errorf("password must be %d-%d characters", MinEndpointPasswordLen, MaxEndpointPasswordLen)
	}
	for _, r := range password {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("password must not contain control characters")
		}
	}
	return nil
}

// MD5PasswordVerifier is the password in the form `pg_authid` stores it under
// `password_encryption = md5`, which the endpoint's compute spec sets: `md5` +
// md5(password || username). Postgres recognizes a string of that shape as
// already hashed and stores it verbatim, which is why this — and not the
// plaintext — is what travels from the request to the pod.
//
// md5 is the wrong algorithm to be using and is tracked as yugabyte/meko#1351;
// the shape of this function (compute a verifier here, hand it to the pod) is
// what that change swaps out.
func MD5PasswordVerifier(password, username string) string {
	return fmt.Sprintf("md5%x", md5.Sum([]byte(password+username)))
}
