package auth

import (
	"strings"
	"testing"
)

func TestGeneratePAT_FormatAndShape(t *testing.T) {
	plaintext, prefix, hash, err := GeneratePAT()
	if err != nil {
		t.Fatalf("GeneratePAT: unexpected error: %v", err)
	}

	if !strings.HasPrefix(plaintext, PATScheme) {
		t.Errorf("plaintext %q missing scheme %q", plaintext, PATScheme)
	}
	if len(prefix) != PATPrefixDisplayLen {
		t.Errorf("prefix len = %d, want %d", len(prefix), PATPrefixDisplayLen)
	}
	if !strings.HasPrefix(plaintext, prefix) {
		t.Errorf("prefix %q is not a leading slice of plaintext %q", prefix, plaintext)
	}
	if len(hash) != PATHashLen {
		t.Errorf("hash len = %d, want %d", len(hash), PATHashLen)
	}
	if !IsPATFormat(plaintext) {
		t.Errorf("IsPATFormat(%q) = false, want true", plaintext)
	}
}

func TestGeneratePAT_IsRandom(t *testing.T) {
	const n = 500
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		plaintext, _, _, err := GeneratePAT()
		if err != nil {
			t.Fatalf("GeneratePAT iter %d: %v", i, err)
		}
		if _, dup := seen[plaintext]; dup {
			t.Fatalf("duplicate plaintext at iter %d: %s", i, plaintext)
		}
		seen[plaintext] = struct{}{}
	}
}

func TestHashPAT_Deterministic(t *testing.T) {
	plaintext, _, hash, err := GeneratePAT()
	if err != nil {
		t.Fatalf("GeneratePAT: %v", err)
	}
	if got := HashPAT(plaintext); got != hash {
		t.Errorf("HashPAT mismatch: got %q want %q", got, hash)
	}
}

func TestHashPAT_KnownVector(t *testing.T) {
	// Smoke test: SHA-256("") = e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
	const empty = ""
	const want = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := HashPAT(empty); got != want {
		t.Errorf("HashPAT(empty) = %q, want %q", got, want)
	}
}

func TestIsPATFormat_Valid(t *testing.T) {
	plaintext, _, _, err := GeneratePAT()
	if err != nil {
		t.Fatalf("GeneratePAT: %v", err)
	}
	if !IsPATFormat(plaintext) {
		t.Errorf("IsPATFormat(%q) = false, want true", plaintext)
	}
}

func TestIsPATFormat_Invalid(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"scheme only", PATScheme},
		{"wrong scheme", "github_pat_abcdef"},
		{"jwt-ish", "eyJhbGciOiJIUzI1NiJ9.foo.bar"},
		{"uppercase body", PATScheme + "ABCDEFGH"},
		{"non-base32 char", PATScheme + "abc!def"},
		{"digit out of range", PATScheme + "abc1def"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if IsPATFormat(tc.token) {
				t.Errorf("IsPATFormat(%q) = true, want false", tc.token)
			}
		})
	}
}

func TestSecureCompareHashes(t *testing.T) {
	if !SecureCompareHashes("abc", "abc") {
		t.Error("SecureCompareHashes(equal) = false, want true")
	}
	if SecureCompareHashes("abc", "abd") {
		t.Error("SecureCompareHashes(diff) = true, want false")
	}
	if SecureCompareHashes("abc", "abcd") {
		t.Error("SecureCompareHashes(diff len) = true, want false")
	}
	if SecureCompareHashes("", "") != true {
		t.Error("SecureCompareHashes(empty,empty) = false, want true")
	}
}
