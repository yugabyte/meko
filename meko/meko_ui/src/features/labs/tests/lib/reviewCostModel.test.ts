import { describe, expect, it } from "vitest";

import { MODEL_OPTIONS } from "@/features/labs/components/demo-apps/ReviewAgentConfigPanel";
import {
  coldContextTokens,
  contextTokens,
  costMultiple,
  FRONTIER_MODEL_ID,
  LAB_TOKENS_PER_CHUNK,
  reviewCost,
  splitInputTokens,
  TYPICAL_MATCHING_CHUNKS,
} from "@/features/labs/lib/reviewCostModel";

const SLM = MODEL_OPTIONS[0].value;

describe("reviewCost", () => {
  it("is the datapack, not the model, that moves the input side", () => {
    const kbChunks = 120;
    const withPack = reviewCost(SLM, true, kbChunks);
    const withoutPack = reviewCost(SLM, false, kbChunks);
    expect(withPack.inputTokens).toBe(TYPICAL_MATCHING_CHUNKS * LAB_TOKENS_PER_CHUNK);
    expect(withoutPack.inputTokens).toBe(kbChunks * LAB_TOKENS_PER_CHUNK);
    // Same model either side, so the only difference is what got sent.
    expect(withoutPack.inputTokens).toBeGreaterThan(withPack.inputTokens * 5);
  });

  it("prices every model the picker offers, so no card renders a blank cost", () => {
    for (const option of MODEL_OPTIONS) {
      expect(reviewCost(option.value, true).dollars, option.label).not.toBeNull();
    }
    expect(reviewCost(FRONTIER_MODEL_ID, true).dollars).not.toBeNull();
  });

  it("returns no cost for a model with no published rate, rather than zero", () => {
    // Zero would render as a free review, which is worse than saying nothing.
    expect(reviewCost("some.unlisted-model", true).dollars).toBeNull();
  });

  it("makes the lesson's claim true: an SLM with context beats the frontier without it", () => {
    const slmWithPack = reviewCost(SLM, true);
    const frontierWithoutPack = reviewCost(FRONTIER_MODEL_ID, false);

    expect(slmWithPack.dollars!).toBeLessThan(frontierWithoutPack.dollars!);
    expect(slmWithPack.quality).toBe("high");
    expect(frontierWithoutPack.quality).toBe("low");
  });

  it("reports the multiple between two costs, and nothing when it can't", () => {
    const cheap = reviewCost(SLM, true);
    const dear = reviewCost(FRONTIER_MODEL_ID, false);
    expect(costMultiple(dear, cheap)).toBeGreaterThan(1);
    expect(costMultiple(dear, reviewCost("some.unlisted-model", true))).toBeNull();
  });
});

describe("coldContextTokens", () => {
  // The whole knowledge base, not the part that matched: a review with no
  // datapack to query cannot send a subset.
  it("prices every chunk in the knowledge base, not the retrieved ones", () => {
    expect(coldContextTokens(4)).toBe(4 * LAB_TOKENS_PER_CHUNK);
  });

  it("returns nothing when the chunk count is unknown, rather than zero", () => {
    expect(coldContextTokens(null)).toBeNull();
    expect(coldContextTokens(0)).toBeNull();
  });
});

describe("splitInputTokens", () => {
  // The two bars sit directly under the stat tile reporting the same total,
  // so a split that loses a token to rounding reads as a bug.
  it("adds back up to the run's measured input", () => {
    const split = splitInputTokens(659, 2, 1);
    expect(split.kb + split.memory).toBe(659);
  });

  it("gives a chunk more of the input than a memory", () => {
    const split = splitInputTokens(659, 1, 1);
    expect(split.kb).toBeGreaterThan(split.memory);
  });

  it("hands everything to the side that retrieved anything at all", () => {
    expect(splitInputTokens(500, 0, 3)).toEqual({ kb: 0, memory: 500 });
    expect(splitInputTokens(500, 3, 0)).toEqual({ kb: 500, memory: 0 });
  });

  it("splits nothing when nothing was retrieved", () => {
    expect(splitInputTokens(659, 0, 0)).toEqual({ kb: 0, memory: 0 });
  });
});

describe("contextTokens", () => {
  // Both sides off the reader's own chunk count: a search sends what
  // matched, no search sends all of it. Four chunks is the lab's own sample
  // datapack today.
  it("prices the whole base against what one search matches", () => {
    const c = contextTokens(4);
    expect(c.withoutDatapack).toBe(4 * LAB_TOKENS_PER_CHUNK);
    expect(c.withDatapack).toBe(TYPICAL_MATCHING_CHUNKS * LAB_TOKENS_PER_CHUNK);
    expect(c.kbChunks).toBe(4);
  });

  // The matched side stays small as the base grows, which is what makes the
  // saving grow with it.
  it("grows the saving as the knowledge base grows", () => {
    const small = contextTokens(4);
    const large = contextTokens(400);
    expect(large.withDatapack).toBe(small.withDatapack);
    expect(large.withoutDatapack / large.withDatapack).toBeGreaterThan(
      small.withoutDatapack / small.withDatapack,
    );
  });

  // A datapack holding nothing sends nothing. Substituting a flattering
  // number here is what the reader would have to unlearn later.
  it("claims no more matches than the base holds", () => {
    for (const empty of [null, undefined, 0]) {
      const c = contextTokens(empty);
      expect(c.matched, String(empty)).toBe(0);
      expect(c.withDatapack, String(empty)).toBe(0);
      expect(c.withoutDatapack, String(empty)).toBe(0);
    }
  });
});
