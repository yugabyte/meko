package service

import (
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
)

// User-type helpers. Free / standard are the two gated tiers; pro and
// enterprise skip caps entirely (fail-open on unknown values too).

// IsFreeUserType reports whether the given `meko_system.users.user_type`
// is the free tier. Kept for legacy call sites that only care about the
// free-vs-not distinction; new code should prefer IsGatedUserType +
// TierLimits so Standard gating slots in cleanly.
func IsFreeUserType(userType string) bool {
	return normalizeTier(userType) == "free"
}

// IsStandardUserType reports whether the user is on the paid self-serve
// Standard tier (AWS Marketplace $25/mo).
func IsStandardUserType(userType string) bool {
	return normalizeTier(userType) == "standard"
}

// IsGatedUserType reports whether the user's tier has cap enforcement.
// Only free and standard are gated; pro / enterprise / unknown skip.
func IsGatedUserType(userType string) bool {
	t := normalizeTier(userType)
	return t == "free" || t == "standard"
}

func normalizeTier(userType string) string {
	return strings.ToLower(strings.TrimSpace(userType))
}

// Free tier caps. Defaults mirror `meko_ui/src/lib/freeTier.ts`; both
// sides read the same env vars (MEKO_FREE_TIER_*) so tuning is a
// one-line env change. Storage is bytes.
var (
	FreeTierMaxDatapacks      = envInt64("MEKO_FREE_TIER_DATAPACKS", 10)
	FreeTierMaxKBStorageBytes = envInt64("MEKO_FREE_TIER_KB_BYTES", 100*1024*1024) // 100 MiB
	FreeTierMaxKBFileBytes    = envInt64("MEKO_FREE_TIER_KB_FILE_BYTES", 5*1024*1024)

	// Conversations / token-burn caps live here for documentation
	// only — chat goes through `inference_gateway`, not api_server,
	// so server-side enforcement is FE-only today.
	FreeTierMaxConversations = envInt64("MEKO_FREE_TIER_CONVERSATIONS", 1_000)
	FreeTierMaxTokenBurn     = envInt64("MEKO_FREE_TIER_TOKEN_BURN", 10_000_000)

	// Conversation-cache embedding cap: one unit per turn embedded into
	// datapacks.conversation_history_search (live index_for_search=True
	// or conversation_index_history backfill), per account across
	// datapacks. Enforced by meko-mcp-server against
	// meko_system.free_tier_usage (metric conversation_search_embedding);
	// this copy exists so api_server/UI read the same env name.
	FreeTierMaxConversationEmbeddings = envInt64("MEKO_FREE_TIER_CONVERSATION_EMBEDDINGS", 2_000)
)

// Standard tier caps (AWS Marketplace $25/mo). Roughly 5-10× Free per
// the pricing table on the Settings page; overridable via MEKO_STANDARD_TIER_*.
var (
	StandardTierMaxDatapacks      = envInt64("MEKO_STANDARD_TIER_DATAPACKS", 50)
	StandardTierMaxKBStorageBytes = envInt64("MEKO_STANDARD_TIER_KB_BYTES", 5*1024*1024*1024) // 5 GiB
	StandardTierMaxKBFileBytes    = envInt64("MEKO_STANDARD_TIER_KB_FILE_BYTES", 50*1024*1024)
	StandardTierMaxConversations  = envInt64("MEKO_STANDARD_TIER_CONVERSATIONS", 10_000)
	StandardTierMaxTokenBurn      = envInt64("MEKO_STANDARD_TIER_TOKEN_BURN", 50_000_000)
)

// TierLimits is the per-tier cap set. `Unlimited` for pro / enterprise
// (a sentinel > any realistic counter) so the same `n >= limit.X` check
// short-circuits without special-casing at every call site.
type TierLimits struct {
	MaxDatapacks      int64
	MaxKBStorageBytes int64
	MaxKBFileBytes    int64
	MaxConversations  int64
	MaxTokenBurn      int64
}

// TierUnlimited is int64's max; treated as "no cap" by callers.
const TierUnlimited int64 = math.MaxInt64

// LimitsFor returns the cap set for the given user_type. Unknown tiers
// fall back to unlimited (fail-open) so a mis-typed value can never
// tighten the gate below what the UI expects.
func LimitsFor(userType string) TierLimits {
	switch normalizeTier(userType) {
	case "free":
		return TierLimits{
			MaxDatapacks:      FreeTierMaxDatapacks,
			MaxKBStorageBytes: FreeTierMaxKBStorageBytes,
			MaxKBFileBytes:    FreeTierMaxKBFileBytes,
			MaxConversations:  FreeTierMaxConversations,
			MaxTokenBurn:      FreeTierMaxTokenBurn,
		}
	case "standard":
		return TierLimits{
			MaxDatapacks:      StandardTierMaxDatapacks,
			MaxKBStorageBytes: StandardTierMaxKBStorageBytes,
			MaxKBFileBytes:    StandardTierMaxKBFileBytes,
			MaxConversations:  StandardTierMaxConversations,
			MaxTokenBurn:      StandardTierMaxTokenBurn,
		}
	default:
		return TierLimits{
			MaxDatapacks:      TierUnlimited,
			MaxKBStorageBytes: TierUnlimited,
			MaxKBFileBytes:    TierUnlimited,
			MaxConversations:  TierUnlimited,
			MaxTokenBurn:      TierUnlimited,
		}
	}
}

// Sentinel errors. Handlers translate to HTTP 402 + a TIER_* code.
// Names kept ErrFreeTier* for backward-compat with handlers already
// checking for them; Standard tier hits produce the same errors so
// callers don't need to duplicate the switch.
var (
	ErrFreeTierDatapackLimit  = errors.New("tier datapack limit reached")
	ErrFreeTierKBStorageLimit = errors.New("tier kb storage limit reached")
)

// Canonical user-facing upgrade messages. The FE now owns the exact
// wording (see TIER_BANNER_MESSAGES in lib/freeTier.ts) and switches
// on the caller's tier; these BE strings are the fallback surfaced
// when the FE isn't in the loop (raw API clients, etc.).
const (
	// Free-tier hits point users at Standard (the AWS Marketplace $25/mo
	// self-serve tier); Standard hits point at Pro.
	FreeTierUpgradeMessageDatapack     = "Upgrade to Standard to create a new datapack."
	FreeTierUpgradeMessageKBUpload     = "Upgrade to Standard to upload new documents."
	StandardTierUpgradeMessageDatapack = "Upgrade to Pro to create a new datapack."
	StandardTierUpgradeMessageKBUpload = "Upgrade to Pro to upload new documents."
	// FreeTierUpgradeMessageFileSize / StandardTierUpgradeMessageFileSize
	// are the per-tier "your file is too big" copies. Free caps at 5 MB
	// per file and steers toward Standard; Standard caps at 50 MB and
	// steers toward Pro. Pro / enterprise are TierUnlimited on
	// MaxKBFileBytes and never hit these strings. Dispatched via
	// UpgradeMessageFileSize(userType).
	FreeTierUpgradeMessageFileSize     = "File too large; maximum 5 MB per file. Upgrade to Standard for larger file uploads."
	StandardTierUpgradeMessageFileSize = "File too large; maximum 50 MB per file. Upgrade to Pro for larger file uploads."
)

// UpgradeMessageFileSize picks the per-tier "file too large" copy so
// Standard users see the 50 MB cap in the error instead of the Free
// tier's 5 MB. Anything not-Standard (including Pro/Enterprise, which
// shouldn't hit this branch at all thanks to TierUnlimited) falls back
// to the Free copy so the response body is never empty.
func UpgradeMessageFileSize(userType string) string {
	if IsStandardUserType(userType) {
		return StandardTierUpgradeMessageFileSize
	}
	return FreeTierUpgradeMessageFileSize
}

// TierLimitResponse bundles the wire code + user-facing message a
// handler should return on a 402 tier-cap hit. Bundling the two keeps
// the (code, message) pair in sync — dispatch on tier once, use both
// halves at the call site.
type TierLimitResponse struct {
	Code    string
	Message string
}

// Wire codes for the two tier-cap paths. Kept as separate FREE_TIER_*
// / STANDARD_TIER_* strings so downstream analytics can attribute cap
// hits to a specific tier without parsing the message copy. The
// FREE_TIER_* codes match what shipped in v0.9.2 (see changelog), so
// existing consumers that hard-coded them keep working.
const (
	CodeFreeTierDatapackLimit     = "FREE_TIER_DATAPACK_LIMIT"
	CodeStandardTierDatapackLimit = "STANDARD_TIER_DATAPACK_LIMIT"
	CodeFreeTierKBStorageLimit    = "FREE_TIER_KB_STORAGE_LIMIT"
	CodeStandardTierKBStorageLimit = "STANDARD_TIER_KB_STORAGE_LIMIT"
)

// DatapackLimitResponse picks the (code, message) pair for a datapack
// cap hit. Free → move to Standard; Standard → move to Pro. Anything
// else falls back to the Free variant so we never surface an empty
// body — but note this path only runs for gated tiers today (see
// IsGatedUserType), so the fallback is a safety net, not a hot path.
func DatapackLimitResponse(userType string) TierLimitResponse {
	if IsStandardUserType(userType) {
		return TierLimitResponse{
			Code:    CodeStandardTierDatapackLimit,
			Message: StandardTierUpgradeMessageDatapack,
		}
	}
	return TierLimitResponse{
		Code:    CodeFreeTierDatapackLimit,
		Message: FreeTierUpgradeMessageDatapack,
	}
}

// KBStorageLimitResponse is the KB-storage counterpart of
// DatapackLimitResponse. Same dispatch rule.
func KBStorageLimitResponse(userType string) TierLimitResponse {
	if IsStandardUserType(userType) {
		return TierLimitResponse{
			Code:    CodeStandardTierKBStorageLimit,
			Message: StandardTierUpgradeMessageKBUpload,
		}
	}
	return TierLimitResponse{
		Code:    CodeFreeTierKBStorageLimit,
		Message: FreeTierUpgradeMessageKBUpload,
	}
}

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
