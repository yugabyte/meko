import { describe, it, expect } from "vitest";
import type { TokenSavingsSummary } from "@/lib/api/mekoApi";
import {
  tokensIfRaw,
  tokensSent,
  tokenBurnFraction,
  tokenBurnPercent,
  dollarsSavedByModel,
  BASELINE_TOKENS_PER_REQUEST,
  PREMIER_MODELS,
} from "@/lib/tokenSavings";

const summary = (over: Partial<TokenSavingsSummary> = {}): TokenSavingsSummary =>
  ({ total_requests: 0, total_tokens_saved: 0, ...over }) as TokenSavingsSummary;

describe("tokensIfRaw", () => {
  it("multiplies requests by the per-request baseline", () => {
    expect(tokensIfRaw(1000)).toBe(1000 * BASELINE_TOKENS_PER_REQUEST);
  });
  it("floors negatives / non-numbers at 0", () => {
    expect(tokensIfRaw(-5)).toBe(0);
    expect(tokensIfRaw(NaN as unknown as number)).toBe(0);
  });
});

describe("tokensSent", () => {
  it("is baseline minus saved", () => {
    expect(tokensSent(summary({ total_requests: 1000, total_tokens_saved: 6_000_000 }))).toBe(4_000_000);
  });
  it("never goes negative when saved exceeds baseline", () => {
    expect(tokensSent(summary({ total_requests: 1, total_tokens_saved: 999_999_999 }))).toBe(0);
  });
});

describe("tokenBurnFraction / tokenBurnPercent", () => {
  it("is saved / baseline, clamped to 0..1", () => {
    expect(tokenBurnFraction(summary({ total_requests: 1000, total_tokens_saved: 6_000_000 }))).toBeCloseTo(0.6);
    expect(tokenBurnPercent(summary({ total_requests: 1000, total_tokens_saved: 6_000_000 }))).toBe(60);
  });
  it("returns 0 when there are no requests (divide-by-zero guard)", () => {
    expect(tokenBurnFraction(summary({ total_requests: 0, total_tokens_saved: 5000 }))).toBe(0);
    expect(tokenBurnPercent(summary())).toBe(0);
  });
  it("clamps an over-100% row to 1", () => {
    expect(tokenBurnFraction(summary({ total_requests: 1, total_tokens_saved: 1_000_000 }))).toBe(1);
  });
});

describe("dollarsSavedByModel", () => {
  it("returns one entry per premier model, in order, matching the worked example", () => {
    const rows = dollarsSavedByModel(summary({ total_requests: 1000, total_tokens_saved: 6_000_000 }));
    expect(rows.map((r) => r.key)).toEqual(PREMIER_MODELS.map((m) => m.key));
    // (10M×15 − 4M×0.10)/1e6 = 149.6 for Opus (see module worked example).
    expect(rows[0].dollars).toBeCloseTo(149.6);
  });
  it("never reports negative dollars", () => {
    const rows = dollarsSavedByModel(summary({ total_requests: 0, total_tokens_saved: 0 }));
    for (const r of rows) expect(r.dollars).toBeGreaterThanOrEqual(0);
  });
});
