package service

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// IsFreeUserType reports whether the given `meko_system.users.user_type`
// is the free tier — the ONLY tier on which free-tier caps apply. Pro,
// Enterprise, empty, or any other value returns false (no cap). This is
// fail-open by design: only an explicit "free" triggers gating.
func IsFreeUserType(userType string) bool {
	return strings.EqualFold(strings.TrimSpace(userType), "free")
}

// Free-tier caps. Mirror of `meko_ui/src/lib/freeTier.ts` — both sides
// read the same env vars (MEKO_FREE_TIER_*) with these defaults, so
// tuning a cap is a one-line env change. Storage is bytes.
var (
	FreeTierMaxDatapacks      = envInt64("MEKO_FREE_TIER_DATAPACKS", 10)
	FreeTierMaxKBStorageBytes = envInt64("MEKO_FREE_TIER_KB_BYTES", 100*1024*1024) // 100 MiB

	// Conversations / token-burn caps live here for documentation
	// only — chat goes through `inference_gateway`, not api_server,
	// so server-side enforcement is FE-only today.
	FreeTierMaxConversations = envInt64("MEKO_FREE_TIER_CONVERSATIONS", 1_000)
	FreeTierMaxTokenBurn     = envInt64("MEKO_FREE_TIER_TOKEN_BURN", 10_000_000)
)

// Sentinel errors. Handlers translate to HTTP 402 + a FREE_TIER_* code.
var (
	ErrFreeTierDatapackLimit  = errors.New("free tier datapack limit reached")
	ErrFreeTierKBStorageLimit = errors.New("free tier kb storage limit reached")
)

// Canonical user-facing upgrade messages.
const (
	FreeTierUpgradeMessageDatapack = "Upgrade to Pro to create a new datapack."
	FreeTierUpgradeMessageKBUpload = "Upgrade to Pro to upload new documents."
	// FreeTierUpgradeMessageFileSize is shown when a single file exceeds the
	// 5 MB per-file cap. The cap itself is currently the same for every
	// tier (see maxFileSizeBytes in knowledge_base_handler.go) -- this copy
	// steers the user toward upgrading without promising a bigger limit
	// today, since Pro doesn't get a higher cap yet.
	FreeTierUpgradeMessageFileSize = "File too large; maximum 5 MB per file. Upgrade to Pro for larger file uploads."
)

// envInt64 reads an int64 from env; falls back to def when unset, empty,
// or unparseable. Negative values are treated as unset.
func envInt64(key string, def int64) int64 {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return def
	}
	return n
}
