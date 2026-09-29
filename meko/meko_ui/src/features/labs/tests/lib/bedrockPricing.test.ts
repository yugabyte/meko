import { describe, expect, it } from "vitest";

import { computeSavingsPct, estimateCost } from "@/features/labs/lib/bedrockPricing";

describe("estimateCost", () => {
  it("computes real cost from real token counts and the model's published rate", () => {
    // Ministral 14B: $0.20 / $0.20 per 1M tokens.
    const cost = estimateCost("mistral.ministral-3-14b-instruct", {
      input_tokens: 1_000_000,
      output_tokens: 1_000_000,
    });
    expect(cost).toBeCloseTo(0.4, 5);
  });

  it("weighs input and output tokens by their own rate, not a blended one", () => {
    // Claude Sonnet 4.5: $3.00 input / $15.00 per 1M -- output costs 5x input.
    const inputOnly = estimateCost("anthropic.claude-sonnet-4-5-20250929-v1:0", {
      input_tokens: 1_000_000,
      output_tokens: 0,
    });
    const outputOnly = estimateCost("anthropic.claude-sonnet-4-5-20250929-v1:0", {
      input_tokens: 0,
      output_tokens: 1_000_000,
    });
    expect(inputOnly).toBeCloseTo(3.0, 5);
    expect(outputOnly).toBeCloseTo(15.0, 5);
  });

  it("returns null for a model with no known rate, rather than treating it as free", () => {
    expect(estimateCost("some.unlisted-model-v1:0", { input_tokens: 1000, output_tokens: 1000 })).toBeNull();
  });
});

describe("computeSavingsPct", () => {
  it("computes a real percentage from two real costs", () => {
    expect(computeSavingsPct(0.4, 4.0)).toBe(90);
  });

  it("returns null, not Infinity or NaN, when the reference cost is exactly zero", () => {
    // The scenario that actually matters here: a not-yet-populated reference
    // fixture (see anthropicReferenceReview.ts's ANTHROPIC_REFERENCE_IS_POPULATED)
    // reports 0 usage, so estimateCost returns 0 -- dividing by it must not
    // render a bogus savings percentage. Exercised directly against this
    // function (not the fixture's current placeholder value) so this stays
    // covered once the fixture is populated with real, non-zero numbers.
    expect(computeSavingsPct(0.4, 0)).toBeNull();
  });

  it("returns null when either cost is unknown", () => {
    expect(computeSavingsPct(null, 4.0)).toBeNull();
    expect(computeSavingsPct(0.4, null)).toBeNull();
  });
});
