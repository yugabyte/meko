// Free-tier limit thresholds + helper that converts a `/auth/me/usage`
// response into the four "has hit limit" booleans the UI gates on.
//
// Single source of truth for the thresholds — every gated surface
// (Add Datapack button, KB upload buttons, WorkbenchLM chat composer)
// imports from here so tuning a cap is a one-line edit.

import { MB } from "@/lib/fileSize";
import type { AccountUsage } from "@/lib/api/mekoApi";

/**
 * Hard caps the Free tier is allowed to consume before the UI starts
 * disabling creation actions. Defaults match
 * `api_server/internal/service/free_tier.go`; the same MEKO_FREE_TIER_*
 * env vars override both sides at build/start time. Storage is bytes.
 *   - 3 datapacks       — enough to evaluate; not enough for a real workspace.
 *   - 100 MB KB storage — a few hundred small docs / a couple of PDFs.
 *   - 1 000 conversations / 10 M tokens for WorkbenchLM — playground
 *                         budget; hard cap before unbounded spend.
 *
 * Adjustments to these numbers should be paired with a Pricing
 * section update in Settings so the displayed table stays accurate.
 */
function envInt(name: string, def: number): number {
  const raw = (import.meta.env as Record<string, string | undefined>)[name];
  if (!raw) return def;
  const n = Number(raw);
  return Number.isFinite(n) && n >= 0 ? n : def;
}

/**
 * Master switch — mirrors `MEKO_FREE_TIER_QUOTA_ENABLED` on the BE/MCP
 * (PR #151). When `false`, the FE skips the /auth/me/usage fetch
 * entirely, treats every user as un-gated, and hides the WorkbenchLM
 * token-burn counter. Off by default so existing deployments are
 * unaffected until explicitly opted in.
 */
export const FREE_TIER_QUOTA_ENABLED: boolean = (() => {
  const raw = (import.meta.env as Record<string, string | undefined>)[
    "MEKO_FREE_TIER_QUOTA_ENABLED"
  ];
  return ["1", "true", "yes"].includes((raw ?? "").trim().toLowerCase());
})();

export const FREE_TIER_LIMITS = {
  datapacks: envInt("MEKO_FREE_TIER_DATAPACKS", 3),
  kbStorageBytes: envInt("MEKO_FREE_TIER_KB_BYTES", MB(100)),
  conversations: envInt("MEKO_FREE_TIER_CONVERSATIONS", 1000),
  tokenBurn: envInt("MEKO_FREE_TIER_TOKEN_BURN", 10_000_000),
} as const;

export type FreeTierStatus = {
  hasHitDatapackCount: boolean;
  hasHitKbStorageFileSize: boolean;
  hasHitNumConversations: boolean;
  hasHitTokenBurn: boolean;
  /** True when any of the above flags are set — convenience for surfaces
   *  that only care about "is the user blocked somewhere." */
  hasHitAny: boolean;
};

/**
 * Returns true ONLY when the account is explicitly on the free tier.
 * Every other value (pro / enterprise / empty / unknown) returns false,
 * so caps apply only to a user the BE has marked "free". Fail-open by
 * design — mirrors the BE's `IsFreeUserType`.
 */
export function isFreeTier(userType?: string | null): boolean {
  return (userType ?? "").trim().toLowerCase() === "free";
}

/**
 * Maps the raw counters from `/auth/me/usage` into hit/not-hit flags
 * against `FREE_TIER_LIMITS`. Pure function so call sites can call it
 * inline without memoization — the inputs are 5 primitives.
 *
 * Returns all-false when:
 *   - `usage` is undefined (react-query still loading on first paint), OR
 *   - `userType` is "pro" / "enterprise" — paid tiers have no caps.
 * The loading-state trade-off is one tiny window during which a user
 * could click before the gates engage; we accept it — the BE re-enforces
 * creation caps and "the button briefly enables, then re-disables on
 * data arrival" is worse UX than the inverse.
 */
export function getFreeTierStatus(
  usage: AccountUsage | undefined,
  userType?: string | null,
): FreeTierStatus {
  if (!FREE_TIER_QUOTA_ENABLED || !usage || !isFreeTier(userType)) {
    return {
      hasHitDatapackCount: false,
      hasHitKbStorageFileSize: false,
      hasHitNumConversations: false,
      hasHitTokenBurn: false,
      hasHitAny: false,
    };
  }
  const hasHitDatapackCount = usage.datapack_count >= FREE_TIER_LIMITS.datapacks;
  const hasHitKbStorageFileSize = usage.kb_storage_file_size >= FREE_TIER_LIMITS.kbStorageBytes;
  const hasHitNumConversations = usage.num_conversations >= FREE_TIER_LIMITS.conversations;
  const hasHitTokenBurn = usage.token_burn >= FREE_TIER_LIMITS.tokenBurn;
  return {
    hasHitDatapackCount,
    hasHitKbStorageFileSize,
    hasHitNumConversations,
    hasHitTokenBurn,
    hasHitAny:
      hasHitDatapackCount || hasHitKbStorageFileSize || hasHitNumConversations || hasHitTokenBurn,
  };
}

/**
 * Standard tooltip / blocked-reason copy used by every gated surface.
 * Centralised so the wording stays consistent everywhere (and so
 * marketing can update it in one place when they want a different
 * upgrade CTA).
 */
export const FREE_TIER_BLOCKED_MESSAGES = {
  datapack: "You have hit the free tier limit. Upgrade to Pro to create a new datapack.",
  knowledgeUpload: "You have hit the free tier limit. Upgrade to Pro to upload further files.",
  conversations:
    "You have hit the free tier limit on conversations. Upgrade to Pro to keep using AI tools with Meko.",
  tokenBurn:
    "You have hit the free tier limit on WorkbenchLM token usage. Upgrade to Pro to keep chatting.",
} as const;

/** External marketing contact page for the "Upgrade to Pro" CTA on the
 *  banner + the PricingSection card. Single point of update when sales
 *  prefers a different URL (Cal.com booking, HubSpot form, etc.). */
export const SALES_CONTACT_URL = "https://info.yugabyte.com/meko-request-access";

/** Long-form banner copy shown at the top of gated pages — more
 *  prominent than the tooltip/under-input message. Each surface picks
 *  the matching key based on which cap was hit. The numbers are baked
 *  in to keep the banner self-explanatory without forcing the user to
 *  hunt for the limit in Settings → Pricing. */
export const FREE_TIER_BANNER_MESSAGES = {
  datapack:
    "You have reached the free tier limit on datapack creation. To create a new datapack, upgrade to Pro by contacting our sales team.",
  kbStorage:
    "You have reached the free tier limit on knowledge-base storage. To upload more files, upgrade to Pro by contacting our sales team.",
  conversations:
    "You have reached the free tier limit on conversations. To continue chatting, upgrade to Pro by contacting our sales team.",
  tokenBurn:
    "You have reached the free tier limit on WorkbenchLM token usage. To continue chatting, upgrade to Pro by contacting our sales team.",
} as const;
