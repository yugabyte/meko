package auth

import (
	"strings"
	"testing"
	"time"
)

func TestPasswords(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "scrypt$32768$8$1$") {
		t.Fatalf("hash %q isn't in AgentK's format", h)
	}
	if !VerifyPassword("correct horse battery", h) || VerifyPassword("wrong horse battery", h) {
		t.Fatal("verify doesn't tell the right password from a wrong one")
	}
	// AgentK's hashPassword for the same password, with the salt "saltsaltsaltsalt" (Node's scryptSync).
	agentk := "scrypt$32768$8$1$c2FsdHNhbHRzYWx0c2FsdA==$" +
		"t9ODFNcd9svkYfWIY4gY5D+lLBfnc9PU7ifx9zL1jGUaKlMvxPU2IZjXLDn8MfAwjsilujzRtORzCpwL0XVUUQ=="
	if !VerifyPassword("correct horse battery", agentk) {
		t.Fatal("an AgentK hash doesn't verify")
	}
	// Past twice the 32 MiB a check takes (N=2^20, r=8 would be 1 GiB), or not a power of two.
	tooBig := "scrypt$1048576$8$1$c2FsdA==$aGFzaGhhc2hoYXNoaGFzaA=="
	notPow2 := "scrypt$30000$8$1$c2FsdA==$aGFzaGhhc2hoYXNoaGFzaA=="
	for _, bad := range []string{"", "bcrypt$x", "scrypt$abc$8$1$c2FsdA==$aGFzaA==", tooBig, notPow2} {
		if VerifyPassword("x", bad) {
			t.Errorf("%q verified", bad)
		}
	}
	BurnPasswordCheck("anything") // must not panic
}

func TestCredentialRules(t *testing.T) {
	cases := []struct {
		username, password string
		userOK, passOK     bool
	}{
		{"owner", "twelve chars", true, true},
		{"first.last@example.com", "short", true, false},
		{"first+tag@example.com", "a long enough one", false, true},
		{"", "a long enough one", false, true},
		{strings.Repeat("a", 65), "a long enough one", false, true},
	}
	for _, c := range cases {
		if got := CheckUsername(c.username) == ""; got != c.userOK {
			t.Errorf("username %q ok = %v", c.username, got)
		}
		if got := CheckPassword(c.password) == ""; got != c.passOK {
			t.Errorf("password %q ok = %v", c.password, got)
		}
	}
}

func TestTOTP(t *testing.T) {
	// RFC 6238's SHA-1 test vector: the secret "12345678901234567890", in base32.
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" //nolint:gosec // G101: RFC 6238's published test secret
	at := time.Unix(59, 0)
	if got := TOTPCode(secret, at); got != "287082" {
		t.Fatalf("code at 59 s = %s, want 287082", got)
	}
	step, ok := TOTPMatch(secret, "287 082", at.Add(30*time.Second))
	if !ok || step != 1 {
		t.Fatalf("a code one step old: step %d ok %v", step, ok)
	}
	if _, ok = TOTPMatch(secret, "287082", at.Add(90*time.Second)); ok {
		t.Fatal("a code three steps old matched")
	}
	for _, bad := range []string{"", "12345", "abcdef", "1234567"} {
		if _, ok = TOTPMatch(secret, bad, at); ok {
			t.Errorf("%q matched", bad)
		}
	}
	s, err := NewTOTPSecret()
	if err != nil || len(s) != 32 || TOTPCode(s, at) == "" {
		t.Fatalf("new secret %q: %v", s, err)
	}
	if u := OTPAuthURL("Meko", "first last", s); !strings.HasPrefix(u, "otpauth://totp/Meko:first%20last?secret="+s) {
		t.Fatalf("otpauth URL %q", u)
	}
}

func TestLoginLimiter(t *testing.T) {
	l := NewLoginLimiter()
	now := time.Unix(1_000_000, 0)
	for range 5 {
		if l.Blocked("10.0.0.1", now) {
			t.Fatal("blocked before 5 failures")
		}
		l.Fail("10.0.0.1", now)
	}
	if !l.Blocked("::ffff:10.0.0.1", now) {
		t.Fatal("5 failures didn't block (the IPv4-mapped form is the same client)")
	}
	if l.Blocked("10.0.0.2", now) {
		t.Fatal("another client is blocked")
	}
	if l.Blocked("10.0.0.1", now.Add(15*time.Minute)) {
		t.Fatal("still blocked after the window")
	}
	// One IPv6 client holds a /64: its addresses share a count.
	for i := range 5 {
		l.Fail("2001:db8::"+string(rune('1'+i)), now)
	}
	if !l.Blocked("2001:db8::ffff", now) {
		t.Fatal("a /64 spread its failures over addresses")
	}
	l.Reset("2001:db8::1")
	if l.Blocked("2001:db8::ffff", now) {
		t.Fatal("reset didn't clear the client")
	}
	// Take counts an attempt before it's checked, and Refund takes it back.
	l2 := NewLoginLimiter()
	for range 5 {
		if !l2.Take("10.0.0.9", now) {
			t.Fatal("Take refused before the limit")
		}
	}
	if l2.Take("10.0.0.9", now) {
		t.Fatal("a sixth Take went through")
	}
	l2.Refund("10.0.0.9", now)
	if !l2.Take("10.0.0.9", now) {
		t.Fatal("Refund didn't give the attempt back")
	}
	// Past the site-wide limit everyone waits.
	for i := range 50 {
		l.Fail("192.0.2."+string(rune('0'+i%10))+string(rune('0'+i/10)), now)
	}
	if !l.Blocked("198.51.100.7", now) {
		t.Fatal("the site-wide limit didn't hold")
	}
}
