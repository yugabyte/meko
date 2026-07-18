package core_test

import (
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func TestVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2025.1.0.0", "2025.1.0.0", 0},
		{"2025.1.0.0", "2025.2.0.0", -1},
		{"2025.2.0.0", "2025.1.0.0", 1},
		{"2.20.1.3", "2.20.1.3", 0},
		{"2.20.1.3-b123", "2.20.1.3-b124", -1},
		{"2.20.1.3-b124", "2.20.1.3-b123", 1},
		{"2.20.1.3-b123", "2.20.1.3", 1}, // build suffix > no build
		{"v1.2.3", "1.2.3", 0},           // leading v tolerated
		{"1.2", "1.2.0.0", 0},            // zero-padding
		{"2025.1.0.0-b999", "2025.1.0.1", -1},
	}
	for _, c := range cases {
		got, err := core.Compare(c.a, c.b)
		if err != nil {
			t.Fatalf("compare %s %s: %v", c.a, c.b, err)
		}
		if got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestVersionParseError(t *testing.T) {
	if _, err := core.ParseVersion(""); err == nil {
		t.Errorf("empty version should error")
	}
}

// TestVersionBaseAndString locks the single-string derivation: a build-suffixed
// version splits into the bare Base (release directory) and the full String
// (tarball filename / image tag / universe version), byte-identical to the
// live-green paths.
func TestVersionBaseAndString(t *testing.T) {
	cases := []struct {
		in, base, full string
	}{
		{"2025.2.1.0-b141", "2025.2.1.0", "2025.2.1.0-b141"},
		{"2.25.0.0-b512", "2.25.0.0", "2.25.0.0-b512"},
		{"2025.2.1.0", "2025.2.1.0", "2025.2.1.0"}, // no build suffix
		{"v1.2.3-b7", "1.2.3", "1.2.3-b7"},         // leading v tolerated
	}
	for _, c := range cases {
		v, err := core.ParseVersion(c.in)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", c.in, err)
		}
		if v.Base() != c.base {
			t.Errorf("ParseVersion(%q).Base() = %q, want %q", c.in, v.Base(), c.base)
		}
		if v.String() != c.full {
			t.Errorf("ParseVersion(%q).String() = %q, want %q", c.in, v.String(), c.full)
		}
		if got := core.BaseVersion(c.in); got != c.base {
			t.Errorf("BaseVersion(%q) = %q, want %q", c.in, got, c.base)
		}
	}
}

func TestVersionSpecQualifies(t *testing.T) {
	spec := core.VersionSpec{MinProduct: "2025.1.0.0", MaxProduct: "2025.3.0.0"}

	if ok, _ := spec.Qualifies(core.DetectedVersions{Product: "2025.2.0.0"}); !ok {
		t.Errorf("in-range product should qualify")
	}
	ok, reason := spec.Qualifies(core.DetectedVersions{Product: "2024.1.0.0"})
	if ok || reason == "" {
		t.Errorf("below-min product should skip with a reason, got ok=%v reason=%q", ok, reason)
	}
	ok, reason = spec.Qualifies(core.DetectedVersions{Product: "2026.1.0.0"})
	if ok || reason == "" {
		t.Errorf("above-max product should skip with a reason, got ok=%v reason=%q", ok, reason)
	}
}

func TestVersionSpecEmptyDetectedQualifies(t *testing.T) {
	spec := core.VersionSpec{MinProduct: "2025.1.0.0"}
	if ok, _ := spec.Qualifies(core.DetectedVersions{}); !ok {
		t.Errorf("unknown detected version should not be gated out")
	}
}

func TestVersionSpecYBBound(t *testing.T) {
	spec := core.VersionSpec{MinYB: "2.20.0.0"}
	if ok, _ := spec.Qualifies(core.DetectedVersions{YB: "2.20.1.0"}); !ok {
		t.Errorf("in-range YB should qualify")
	}
	if ok, _ := spec.Qualifies(core.DetectedVersions{YB: "2.18.0.0"}); ok {
		t.Errorf("below-min YB should skip")
	}
}

func TestLooksLikeVersion(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"2.21.1.0", true},
		{"2.21.1.0-b1", true},
		{"v2.21.1.0", true},
		{"18.4", true},
		{"", false},
		{"latest", false},
		{"PostgreSQL 15.4 on x86_64-pc-linux-gnu, compiled by gcc", false},
		{"2.21.1.0-bXX", false}, // build suffix must be numeric
		{"2.21.a.0", false},     // non-numeric dotted part
	}
	for _, c := range cases {
		if got := core.LooksLikeVersion(c.in); got != c.want {
			t.Errorf("LooksLikeVersion(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestExtractYBVersionMarker exercises the exact raw shape YSQL's SELECT
// version() returns — "PostgreSQL <pgver>-YB-<yb version> on ..." — verified
// against yugabyte-db's TestPgSelect.java#testPgsqlVersion, which asserts a
// live YSQL connection's response matches "PostgreSQL.*-YB-.*".
func TestExtractYBVersionMarker(t *testing.T) {
	raw := "PostgreSQL 11.2-YB-2.21.1.0-b1 on x86_64-pc-linux-gnu, compiled by gcc (Homebrew GCC 5.5.0_4) 5.5.0, 64-bit"
	v, ok := core.ExtractYBVersionMarker(raw)
	if !ok {
		t.Fatalf("ExtractYBVersionMarker(%q) failed to find the YB marker", raw)
	}
	if v != "2.21.1.0-b1" {
		t.Errorf("ExtractYBVersionMarker(%q) = %q, want 2.21.1.0-b1", raw, v)
	}
	// The extracted marker must itself be Compare-able, feeding straight into a
	// version gate.
	spec := core.VersionSpec{MinYB: "2.20.0.0", MaxYB: "2.25.0.0"}
	if ok, reason := spec.Qualifies(core.DetectedVersions{YB: v}); !ok {
		t.Errorf("extracted marker %q should qualify spec, got reason %q", v, reason)
	}
}

func TestExtractYBVersionMarkerNoMatch(t *testing.T) {
	cases := []string{
		"PostgreSQL 15.4 on x86_64-pc-linux-gnu, compiled by gcc, 64-bit", // no YB marker (e.g. a non-YSQL compute Postgres)
		"",
	}
	for _, raw := range cases {
		if v, ok := core.ExtractYBVersionMarker(raw); ok {
			t.Errorf("ExtractYBVersionMarker(%q) = %q, want no match", raw, v)
		}
	}
}
