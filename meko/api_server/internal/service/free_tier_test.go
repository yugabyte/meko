package service_test

import (
	"strings"
	"testing"

	"meko/api_server/internal/service"
)

const (
	mib = 1024 * 1024
	gib = 1024 * 1024 * 1024
)

// LimitsFor is the single source the presigned KB upload flow reads its
// per-file and total-storage caps from, and the docs quote its values. These
// assertions pin the defaults so a drift here surfaces as a failed test
// rather than a silently wrong cap.
func TestLimitsFor_PerTierCaps(t *testing.T) {
	cases := []struct {
		userType    string
		wantFile    int64
		wantStorage int64
	}{
		{"free", 5 * mib, 100 * mib},
		{"standard", 50 * mib, 5 * gib},
		// Pro/enterprise: storage unlimited; per-file caps at 60 MB
		// (ProTierMaxKBFileBytes), just above Standard.
		{"pro", 60 * mib, service.TierUnlimited},
		{"enterprise", 60 * mib, service.TierUnlimited},
		// Unknown tier fails open EXCEPT per-file size → top-tier 60 MB fallback.
		{"", 60 * mib, service.TierUnlimited},
	}
	for _, tc := range cases {
		got := service.LimitsFor(tc.userType)
		if got.MaxKBFileBytes != tc.wantFile {
			t.Errorf("LimitsFor(%q).MaxKBFileBytes = %d, want %d",
				tc.userType, got.MaxKBFileBytes, tc.wantFile)
		}
		if got.MaxKBStorageBytes != tc.wantStorage {
			t.Errorf("LimitsFor(%q).MaxKBStorageBytes = %d, want %d",
				tc.userType, got.MaxKBStorageBytes, tc.wantStorage)
		}
	}
}

// The presigned upload flow gates its total-storage check on IsGatedUserType,
// so pro/enterprise (unlimited) skip it while free/standard don't.
func TestIsGatedUserType(t *testing.T) {
	for _, ut := range []string{"free", "standard"} {
		if !service.IsGatedUserType(ut) {
			t.Errorf("IsGatedUserType(%q) = false, want true", ut)
		}
	}
	for _, ut := range []string{"pro", "enterprise", ""} {
		if service.IsGatedUserType(ut) {
			t.Errorf("IsGatedUserType(%q) = true, want false", ut)
		}
	}
}

// The "file too large" copy must name the tier's own cap so a Standard user
// isn't told the Free 5 MB limit.
func TestUpgradeMessageFileSize(t *testing.T) {
	if msg := service.UpgradeMessageFileSize("standard"); !strings.Contains(msg, "50 MB") {
		t.Errorf("standard file-size message = %q, want it to mention 50 MB", msg)
	}
	if msg := service.UpgradeMessageFileSize("free"); !strings.Contains(msg, "5 MB") {
		t.Errorf("free file-size message = %q, want it to mention 5 MB", msg)
	}
	// Pro is top-tier and caps per-file at 60 MB, stated with no upgrade pitch.
	proMsg := service.UpgradeMessageFileSize("pro")
	if !strings.Contains(proMsg, "60 MB") {
		t.Errorf("pro file-size message = %q, want it to mention 60 MB", proMsg)
	}
	if strings.Contains(proMsg, "Upgrade") {
		t.Errorf("pro file-size message = %q, should not pitch an upgrade", proMsg)
	}
}

// The multipart buffering ceiling is not a tier limit and must not leak into
// LimitsFor (the presigned flow reads that); it defaults to 50 MB.
