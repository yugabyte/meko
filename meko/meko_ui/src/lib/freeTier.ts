// Tier limit thresholds + helper that converts a `/auth/me/usage`
// response into "has hit limit" booleans for whichever tier the user
// is on. Single source of truth for the caps — every gated surface
// imports from here so tuning is a one-line env change.

import { MB, GB } from "@/lib/fileSize";
import { readRuntimeEnv } from "@/lib/runtimeEnv";
import type { AccountUsage } from "@/lib/api/mekoApi";

function envInt(name: string, def: number): number {
  // Canonical override syntax shared with api_server (envInt64) and the MCP
  // server (_int_env): trimmed base-10 digits only. Number() alone would also
  // accept exponents ("1e3"), hex, and floats that the backends reject.
  const raw = readRuntimeEnv(name)?.trim();
  if (!raw || !/^\d+$/.test(raw)) return def;
  return Number(raw);
}

/**
 * Master switch — mirrors `MEKO_FREE_TIER_QUOTA_ENABLED` on the BE/MCP.
 * When `false`, the FE skips the /auth/me/usage fetch entirely, treats
 * every user as un-gated, and hides the WorkbenchLM token-burn counter.
 * Off by default so existing deployments are unaffected until opted in.
 */
export const FREE_TIER_QUOTA_ENABLED: boolean = (() => {
  const raw = readRuntimeEnv("MEKO_FREE_TIER_QUOTA_ENABLED");
  return ["1", "true", "yes"].includes((raw ?? "").trim().toLowerCase());
})();

/** Free tier — defaults mirror `api_server/internal/service/free_tier.go`. */
export const FREE_TIER_LIMITS = {
  datapacks: envInt("MEKO_FREE_TIER_DATAPACKS", 10),
  kbStorageBytes: envInt("MEKO_FREE_TIER_KB_BYTES", MB(100)),
  kbFileBytes: envInt("MEKO_FREE_TIER_KB_FILE_BYTES", MB(5)),
  conversations: envInt("MEKO_FREE_TIER_CONVERSATIONS", 1000),
  tokenBurn: envInt("MEKO_FREE_TIER_TOKEN_BURN", 10_000_000),
  // MCP-side retrieval / write caps. Enforced by meko-mcp-server
  // against `meko_system.free_tier_usage`; api_server + UI only read
  // them for the Settings usage tracker. Env names shared with the
  // MCP process so one variable tunes both sides.
  memorySearches: envInt("MEKO_FREE_TIER_MEMORY_SEARCH", 1000),
  kbSearches: envInt("MEKO_FREE_TIER_KB_SEARCH", 1000),
  memoryAdds: envInt("MEKO_FREE_TIER_MEMORY_ADD", 10_000),
} as const;

/** Standard tier ($25/mo via AWS Marketplace) — same env-override pattern. */
export const STANDARD_TIER_LIMITS = {
  datapacks: envInt("MEKO_STANDARD_TIER_DATAPACKS", 50),
  kbStorageBytes: envInt("MEKO_STANDARD_TIER_KB_BYTES", GB(5)),
  kbFileBytes: envInt("MEKO_STANDARD_TIER_KB_FILE_BYTES", MB(50)),
  conversations: envInt("MEKO_STANDARD_TIER_CONVERSATIONS", 10_000),
  tokenBurn: envInt("MEKO_STANDARD_TIER_TOKEN_BURN", 50_000_000),
  memorySearches: envInt("MEKO_STANDARD_TIER_MEMORY_SEARCH", 5000),
  kbSearches: envInt("MEKO_STANDARD_TIER_KB_SEARCH", 5000),
  memoryAdds: envInt("MEKO_STANDARD_TIER_MEMORY_ADD", 30_000),
} as const;

/** Row label shared by the Pricing card and the Usage tracker so the
 *  two surfaces read as a matched pair. "Total" distinguishes the
 *  account-wide storage cap from the per-file "Max KB file size" row. */
export const KB_STORAGE_LIMIT_LABEL = "Total KB file storage";

export type TierName = "free" | "standard" | "pro" | "enterprise" | "unknown";

/** Normalise raw user_type strings from /auth/me into a known TierName. */
export function resolveTier(userType?: string | null): TierName {
  const t = (userType ?? "").trim().toLowerCase();
  if (t === "free" || t === "standard" || t === "pro" || t === "enterprise") {
    return t;
  }
  return "unknown";
}

/** Convenience — Free is the only tier that used to have gates before Standard. */
export function isFreeTier(userType?: string | null): boolean {
  return resolveTier(userType) === "free";
}

/** True when the tier has cap enforcement (free + standard). */
export function isGatedTier(userType?: string | null): boolean {
  const t = resolveTier(userType);
  return t === "free" || t === "standard";
}

/** True for any paid tier (standard and above) — used to gate deploying a
 *  dedicated agent instance in Labs, which free-tier users can't do (they
 *  can still use a lab's shared instance, which needs no deploy at all). */
export function canDeployDedicatedAgent(userType?: string | null): boolean {
  return !isFreeTier(userType);
}

export type TierLimits = typeof FREE_TIER_LIMITS;

/** Cap set for the given tier. Pro / enterprise / unknown → Infinity per field,
 *  EXCEPT per-file upload size, which caps at 60 MB (matching api_server's
 *  ProTierMaxKBFileBytes — just above Standard's 50 MB). Display + enforcement
 *  read the same env name api_server does, so they stay in lockstep. */
export function limitsFor(userType?: string | null): TierLimits {
  const t = resolveTier(userType);
  if (t === "free") return FREE_TIER_LIMITS;
  if (t === "standard") return STANDARD_TIER_LIMITS;
  return {
    datapacks: Number.POSITIVE_INFINITY,
    kbStorageBytes: Number.POSITIVE_INFINITY,
    kbFileBytes: envInt("MEKO_PRO_TIER_KB_FILE_BYTES", MB(60)),
    conversations: Number.POSITIVE_INFINITY,
    tokenBurn: Number.POSITIVE_INFINITY,
    memorySearches: Number.POSITIVE_INFINITY,
    kbSearches: Number.POSITIVE_INFINITY,
    memoryAdds: Number.POSITIVE_INFINITY,
  };
}

export type TierStatus = {
  hasHitDatapackCount: boolean;
  hasHitKbStorageFileSize: boolean;
  hasHitNumConversations: boolean;
  hasHitTokenBurn: boolean;
  hasHitAny: boolean;
};

/**
 * Maps `/auth/me/usage` counters into hit/not-hit flags against the
 * caller's tier limits. Returns all-false for pro / enterprise / when
 * usage is loading — the BE re-enforces creation caps, so a brief
 * un-gated window before data lands is preferable to the inverse
 * (button flashes disabled then enables).
 */
export function getTierStatus(
  usage: AccountUsage | undefined,
  userType?: string | null,
): TierStatus {
  if (!FREE_TIER_QUOTA_ENABLED || !usage || !isGatedTier(userType)) {
    return {
      hasHitDatapackCount: false,
      hasHitKbStorageFileSize: false,
      hasHitNumConversations: false,
      hasHitTokenBurn: false,
      hasHitAny: false,
    };
  }
  const limits = limitsFor(userType);
  // LIFETIME creations (never decremented on delete) gate datapacks so
  // delete-and-recreate can't reset the quota. Take max with live count
  // so a zero counter (stale BE / legacy user) can't under-gate.
  const datapackCreations = Math.max(usage.datapack_creations ?? 0, usage.datapack_count);
  const hasHitDatapackCount = datapackCreations >= limits.datapacks;
  const hasHitKbStorageFileSize = usage.kb_storage_file_size >= limits.kbStorageBytes;
  const hasHitNumConversations = usage.num_conversations >= limits.conversations;
  const hasHitTokenBurn = usage.token_burn >= limits.tokenBurn;
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
 * Back-compat alias. Existing callers pass Free users only, so behavior
 * matches getTierStatus with userType="free". Prefer getTierStatus in
 * new code.
 */
export const getFreeTierStatus = getTierStatus;

/**
 * Suggested next tier for the "Upgrade" CTA — Free hits recommend
 * Standard, Standard hits recommend Pro. Anything else falls back to
 * Pro so we never say "Upgrade to Free."
 */
export function nextTierFor(userType?: string | null): "standard" | "pro" {
  return resolveTier(userType) === "free" ? "standard" : "pro";
}

/**
 * Short tooltip / disabled-reason copy for gated buttons. Picked per
 * (target tier × dimension); target tier comes from nextTierFor().
 */
export const TIER_BLOCKED_MESSAGES = {
  standard: {
    datapack: "You've hit the Free tier limit. Upgrade to Standard to create more datapacks.",
    knowledgeUpload: "You've hit the Free tier limit. Upgrade to Standard to upload more files.",
    conversations:
      "You've hit the Free tier limit on conversations. Upgrade to Standard to keep using AI tools with Meko.",
    tokenBurn:
      "You've hit the Free tier limit on WorkbenchLM tokens. Upgrade to Standard to keep chatting.",
  },
  pro: {
    datapack: "You've hit the Standard tier limit. Upgrade to Pro to create more datapacks.",
    knowledgeUpload: "You've hit the Standard tier limit. Upgrade to Pro to upload more files.",
    conversations:
      "You've hit the Standard tier limit on conversations. Upgrade to Pro to keep using AI tools with Meko.",
    tokenBurn:
      "You've hit the Standard tier limit on WorkbenchLM tokens. Upgrade to Pro to keep chatting.",
  },
} as const;

export type BlockedMessageKind = keyof (typeof TIER_BLOCKED_MESSAGES)["standard"];

/** Legacy alias — existing callers used FREE_TIER_BLOCKED_MESSAGES which
 *  always spoke Free→Pro. New callers should use `blockedMessage(kind, userType)`. */
export const FREE_TIER_BLOCKED_MESSAGES = TIER_BLOCKED_MESSAGES.standard;

/** Pick the right blocked-message copy for the caller's tier. */
export function blockedMessage(kind: BlockedMessageKind, userType?: string | null): string {
  return TIER_BLOCKED_MESSAGES[nextTierFor(userType)][kind];
}

/** External marketing contact page for the Pro "Talk to our team" CTA. */
export const SALES_CONTACT_URL = "https://share-na2.hsforms.com/16H0_N54LT0WVNmcALDOFIwbutl6";

/** AWS Marketplace listing URL used by the Standard tile's Upgrade
 *  button. Runtime var so ops can point at Limited vs Public listings
 *  per-deployment without a rebuild. Independent of the tile's
 *  visibility -- see STANDARD_TIER_ENABLED for that. */
export const AWS_MARKETPLACE_LISTING_URL: string =
  readRuntimeEnv("MEKO_AWS_MARKETPLACE_URL") ?? "";

/** Pre-GA gate for the Standard tier. When false the Settings page
 *  hides the Standard column entirely (2-card Free + Pro layout).
 *  Runtime var; set `MEKO_ENABLE_STANDARD_TIER=true` on the pod (or
 *  in local dev's .env) to enable. */
export const STANDARD_TIER_ENABLED: boolean = (() => {
  const raw = readRuntimeEnv("MEKO_ENABLE_STANDARD_TIER") ?? "";
  return ["1", "true", "yes"].includes(raw.trim().toLowerCase());
})();

/** Long-form banner copy shown at the top of gated pages. Keyed by
 *  (target tier × dimension) so the banner reflects which upgrade
 *  actually helps the user. */
export const TIER_BANNER_MESSAGES = {
  standard: {
    datapack:
      "You've reached the Free tier limit on datapack creation. Upgrade to Standard for higher limits.",
    kbStorage:
      "You've reached the Free tier limit on knowledge-base storage. Upgrade to Standard for higher limits.",
    conversations:
      "You've reached the Free tier limit on conversations. Upgrade to Standard for higher limits.",
    tokenBurn:
      "You've reached the Free tier limit on WorkbenchLM token usage. Upgrade to Standard for higher limits.",
  },
  pro: {
    datapack:
      "You've reached the Standard tier limit on datapack creation. Upgrade to Pro for higher limits.",
    kbStorage:
      "You've reached the Standard tier limit on knowledge-base storage. Upgrade to Pro for higher limits.",
    conversations:
      "You've reached the Standard tier limit on conversations. Upgrade to Pro for higher limits.",
    tokenBurn:
      "You've reached the Standard tier limit on WorkbenchLM token usage. Upgrade to Pro for higher limits.",
  },
} as const;

export type TierBannerKind = keyof (typeof TIER_BANNER_MESSAGES)["standard"];

/** Legacy alias — used by callers before Standard existed. Still speaks the
 *  Free→Standard variant so behaviour matches for pure-Free callers. */
export const FREE_TIER_BANNER_MESSAGES = TIER_BANNER_MESSAGES.standard;
