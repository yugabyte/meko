import { describe, it, expect, vi } from "vitest";

// The quota master-switch (FREE_TIER_QUOTA_ENABLED) and limits are read from
// runtime env at module import, so mock readRuntimeEnv to enable the quota;
// unknown keys fall back to defaults (FREE_TIER_LIMITS).
vi.mock("@/lib/runtimeEnv", () => ({
  readRuntimeEnv: (name: string) =>
    name === "MEKO_FREE_TIER_QUOTA_ENABLED" ? "true" : undefined,
}));

import { isFreeTier, getFreeTierStatus, FREE_TIER_LIMITS } from "@/lib/freeTier";
import type { AccountUsage } from "@/lib/api/mekoApi";

const usage = (over: Partial<AccountUsage> = {}): AccountUsage =>
  ({
    datapack_count: 0,
    datapack_creations: 0,
    kb_storage_file_size: 0,
    num_conversations: 0,
    token_burn: 0,
    ...over,
  }) as AccountUsage;

describe("isFreeTier", () => {
  it("is true only for the literal 'free' tier (case/space-insensitive)", () => {
    expect(isFreeTier("free")).toBe(true);
    expect(isFreeTier("  FREE ")).toBe(true);
  });
  it("is false for every other value", () => {
    expect(isFreeTier("pro")).toBe(false);
    expect(isFreeTier("enterprise")).toBe(false);
    expect(isFreeTier("")).toBe(false);
    expect(isFreeTier(null)).toBe(false);
    expect(isFreeTier(undefined)).toBe(false);
  });
});

describe("getFreeTierStatus", () => {
  it("returns all-false for a non-free (paid) user", () => {
    const s = getFreeTierStatus(usage({ datapack_count: 9999 }), "pro");
    expect(s.hasHitAny).toBe(false);
  });

  it("returns all-false while usage is still loading (undefined)", () => {
    expect(getFreeTierStatus(undefined, "free").hasHitAny).toBe(false);
  });

  it("flags nothing when a free user is under every limit", () => {
    const s = getFreeTierStatus(usage(), "free");
    expect(s).toMatchObject({
      hasHitDatapackCount: false,
      hasHitKbStorageFileSize: false,
      hasHitNumConversations: false,
      hasHitTokenBurn: false,
      hasHitAny: false,
    });
  });

  it("flags each limit at/over its threshold", () => {
    const s = getFreeTierStatus(
      usage({
        datapack_count: FREE_TIER_LIMITS.datapacks,
        kb_storage_file_size: FREE_TIER_LIMITS.kbStorageBytes,
        num_conversations: FREE_TIER_LIMITS.conversations,
        token_burn: FREE_TIER_LIMITS.tokenBurn,
      }),
      "free",
    );
    expect(s).toMatchObject({
      hasHitDatapackCount: true,
      hasHitKbStorageFileSize: true,
      hasHitNumConversations: true,
      hasHitTokenBurn: true,
      hasHitAny: true,
    });
  });

  it("gates datapacks on the max of lifetime creations and live count", () => {
    // Live count under limit, but lifetime creations at the cap → blocked.
    const s = getFreeTierStatus(
      usage({ datapack_count: 0, datapack_creations: FREE_TIER_LIMITS.datapacks }),
      "free",
    );
    expect(s.hasHitDatapackCount).toBe(true);
  });
});
