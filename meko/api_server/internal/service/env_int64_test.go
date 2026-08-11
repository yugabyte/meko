package service

import "testing"

// envInt64 must accept exactly what the MCP server's _int_env and the UI's
// envInt accept: trimmed base-10 digits. Anything looser lets one env value
// enforce one limit here and display another elsewhere.
func TestEnvInt64CanonicalParsing(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int64
	}{
		{"plain", "1000", 1000},
		{"surrounding whitespace", " 1000 ", 1000},
		{"zero", "0", 0},
		{"empty", "", 77},
		{"whitespace only", "   ", 77},
		{"exponent", "1e3", 77},
		{"underscore", "1_000", 77},
		{"leading sign", "+1000", 77},
		{"negative", "-5", 77},
		{"hex", "0x10", 77},
		{"float", "1000.5", 77},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MEKO_TEST_INT", tc.raw)
			if got := envInt64("MEKO_TEST_INT", 77); got != tc.want {
				t.Fatalf("envInt64(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
	t.Run("unset", func(t *testing.T) {
		if got := envInt64("MEKO_TEST_INT_UNSET", 77); got != 77 {
			t.Fatalf("envInt64(unset) = %d, want 77", got)
		}
	})
}
