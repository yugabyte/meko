package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckBearer(t *testing.T) {
	cases := []struct {
		header, token string
		want          bool
	}{
		{"Bearer abc", "abc", true},
		{"Bearer abd", "abc", false},
		{"abc", "abc", false},
		{"Basic abc", "abc", false},
		{"", "abc", false},
		{"Bearer ", "", false},
	}
	for _, tc := range cases {
		if got := CheckBearer(tc.header, tc.token); got != tc.want {
			t.Errorf("CheckBearer(%q, %q) = %v", tc.header, tc.token, got)
		}
	}
}

func TestTokenRoundTrip(t *testing.T) {
	tok, err := NewToken()
	if err != nil || len(tok) != 64 {
		t.Fatalf("token %q, %v", tok, err)
	}
	path := filepath.Join(t.TempDir(), "admin.token")
	if err := WriteToken(path, tok); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if got, err := ReadToken(path); err != nil || got != tok {
		t.Fatalf("read %q, %v", got, err)
	}
}
