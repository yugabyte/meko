// Tier limit thresholds + helper that converts a `/auth/me/usage`
// response into "has hit limit" booleans for whichever tier the user
// is on. Single source of truth for the caps — every gated surface
// imports from here so tuning is a one-line env change.

import { MB, GB } from "@/lib/fileSize";
import { readRuntimeEnv } from "@/lib/runtimeEnv";
import type { AccountUsage } from "@/lib/api/mekoApi";

function envInt(name: string, def: number): number {
  const raw = readRuntimeEnv(name);
  if (!raw) return def;
  const n = Number(raw);
  return Number.isFinite(n) && n >= 0 ? n : def;
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
} as const;

/** Standard tier ($25/mo via AWS Marketplace) — same env-override pattern. */
export const STANDARD_TIER_LIMITS = {
  datapacks: envInt("MEKO_STANDARD_TIER_DATAPACKS", 50),
  kbStorageBytes: envInt("MEKO_STANDARD_TIER_KB_BYTES", GB(5)),
  kbFileBytes: envInt("MEKO_STANDARD_TIER_KB_FILE_BYTES", MB(50)),
  conversations: envInt("MEKO_STANDARD_TIER_CONVERSATIONS", 10_000),
  tokenBurn: envInt("MEKO_STANDARD_TIER_TOKEN_BURN", 50_000_000),
} as const;

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

export type TierLimits = typeof FREE_TIER_LIMITS;

/** Cap set for the given tier. Unknown / pro / enterprise → Infinity per field. */
export function limitsFor(userType?: string | null): TierLimits {
  const t = resolveTier(userType);
  if (t === "free") return FREE_TIER_LIMITS;
  if (t === "standard") return STANDARD_TIER_LIMITS;
  return {
    datapacks: Number.POSITIVE_INFINITY,
    kbStorageBytes: Number.POSITIVE_INFINITY,
    kbFileBytes: Number.POSITIVE_INFINITY,
    conversations: Number.POSITIVE_INFINITY,
    tokenBurn: Number.POSITIVE_INFINITY,
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

/** localStorage key testers set to opt IN to seeing the Standard tier
 *  tile while `MEKO_ENABLE_STANDARD_TIER=true` on the pod. Kept as a
 *  named export so future callers use one canonical string.
 *  TODO(GA): remove this key + the AND-gate below once Standard is
 *  live for all users. */
export const STANDARD_TIER_LOCAL_OPT_IN_KEY = "meko_show_standard_tier";

/** Pre-GA gate for the Standard tier. When false the Settings page
 *  hides the Standard column entirely (2-card Free + Pro layout).
 *
 *  Gated on BOTH:
 *    - runtime env: `MEKO_ENABLE_STANDARD_TIER=true` on the pod, AND
 *    - per-browser localStorage opt-in: window.localStorage.setItem(
 *      STANDARD_TIER_LOCAL_OPT_IN_KEY, "true").
 *
 *  This lets ops flip the env flag on prod for validation without the
 *  tile being visible to real users -- only browsers that explicitly
 *  set the localStorage key see the Standard tile + Upgrade button.
 *  Once Standard goes GA, drop the localStorage requirement (delete
 *  the key + the AND-gate below) so the env flag alone controls
 *  visibility.
 *
 *  TODO(GA): remove the localOptIn check + STANDARD_TIER_LOCAL_OPT_IN_KEY. */
export const STANDARD_TIER_ENABLED: boolean = (() => {
  const raw = readRuntimeEnv("MEKO_ENABLE_STANDARD_TIER") ?? "";
  const envEnabled = ["1", "true", "yes"].includes(raw.trim().toLowerCase());
  if (!envEnabled) return false;
  // SSR / non-browser guard: no localStorage exists, so we can't
  // check the opt-in. Fall back to env-only in that context (matches
  // the historical behavior before the AND-gate was added).
  if (typeof window === "undefined" || typeof window.localStorage === "undefined") {
    return true;
  }
  return window.localStorage.getItem(STANDARD_TIER_LOCAL_OPT_IN_KEY) === "true";
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
