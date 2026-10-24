package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // G505: RFC 6238 TOTP is HMAC-SHA1, which authenticator apps use
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP is RFC 6238 with SHA-1, 30-second steps and 6 digits, as authenticator apps use, with
// AgentK's base32 secrets (no padding), so an imported one keeps working.
const totpStep = 30

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a new 160-bit secret in base32.
func NewTOTPSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b32.EncodeToString(b), nil
}

// TOTPCode is the code for secret at t, or "" when the secret isn't base32.
func TOTPCode(secret string, t time.Time) string {
	return totpAt(secret, t.Unix()/totpStep)
}

func totpAt(secret string, counter int64) string {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimRight(strings.ReplaceAll(secret, " ", ""), "=")))
	if err != nil {
		return ""
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter)) //nolint:gosec // G115: a Unix time step is positive
	h := hmac.New(sha1.New, key)
	h.Write(msg[:])
	sum := h.Sum(nil)
	off := sum[len(sum)-1] & 15
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", bin%1_000_000)
}

// TOTPMatch returns the time step a code matches at t, allowing one step of clock drift either way,
// and whether it matched. Callers refuse a step at or before the last one accepted (TOTP replay).
func TOTPMatch(secret, code string, t time.Time) (int64, bool) {
	code = strings.ReplaceAll(code, " ", "")
	if len(code) != 6 || strings.Trim(code, "0123456789") != "" {
		return 0, false
	}
	now := t.Unix() / totpStep
	for _, step := range []int64{now - 1, now, now + 1} {
		if c := totpAt(secret, step); c != "" && hmac.Equal([]byte(c), []byte(code)) {
			return step, true
		}
	}
	return 0, false
}

// OTPAuthURL is the link an authenticator app reads from the enrolment QR code.
func OTPAuthURL(issuer, username, secret string) string {
	return fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s", url.PathEscape(issuer), url.PathEscape(username),
		secret, url.QueryEscape(issuer))
}
