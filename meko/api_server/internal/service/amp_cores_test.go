package service

import "testing"

// ParseMilliCores must read Kubernetes CPU quantities and fall back to AMP's
// 2-core default for empty/unparseable input (so a tier check never
// under-counts compute).
func TestParseMilliCores(t *testing.T) {
	cases := map[string]int64{
		"2":       2000,
		"1":       1000,
		"0.5":     500,
		"500m":    500,
		"1500m":   1500,
		"":        2000, // AMP default writer size
		"  2  ":   2000,
		"garbage": 2000, // unparseable -> default
	}
	for in, want := range cases {
		if got := ParseMilliCores(in); got != want {
			t.Errorf("ParseMilliCores(%q) = %d, want %d", in, got, want)
		}
	}
}

// The per-tier vCPU caps: Free 0.5 (500m), Standard 2 (2000m), Pro 5 (5000m);
// only Enterprise (and unknown tiers, fail-open) are unlimited.
func TestLimitsFor_MaxMilliCores(t *testing.T) {
	cases := map[string]int64{
		"free":       500,
		"standard":   2000,
		"pro":        5000,
		"enterprise": TierUnlimited,
		"":           TierUnlimited, // unknown -> fail open
	}
	for tier, want := range cases {
		if got := LimitsFor(tier).MaxMilliCores; got != want {
			t.Errorf("LimitsFor(%q).MaxMilliCores = %d, want %d", tier, got, want)
		}
	}
}

// CoreLimitResponse steers a gated user one tier up (free->Standard,
// standard->Pro) with the matching wire code.
func TestCoreLimitResponse(t *testing.T) {
	free := CoreLimitResponse("free")
	if free.Code != CodeFreeTierCoreLimit {
		t.Errorf("free code = %q, want %q", free.Code, CodeFreeTierCoreLimit)
	}
	if !contains(free.Message, "Standard") {
		t.Errorf("free message should steer to Standard: %q", free.Message)
	}
	std := CoreLimitResponse("standard")
	if std.Code != CodeStandardTierCoreLimit {
		t.Errorf("standard code = %q, want %q", std.Code, CodeStandardTierCoreLimit)
	}
	if !contains(std.Message, "Pro") {
		t.Errorf("standard message should steer to Pro: %q", std.Message)
	}
	pro := CoreLimitResponse("pro")
	if pro.Code != CodeProTierCoreLimit {
		t.Errorf("pro code = %q, want %q", pro.Code, CodeProTierCoreLimit)
	}
	if !contains(pro.Message, "Enterprise") {
		t.Errorf("pro message should steer to Enterprise: %q", pro.Message)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
