import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { ANTHROPIC_REFERENCE_MODEL_LABEL } from "../../data/adlcReviewAgentLab/anthropicReferenceReview";
import { AnthropicReferenceCard } from "./AnthropicReferenceCard";

afterEach(cleanup);

describe("AnthropicReferenceCard", () => {
  // The real fixture is unpopulated today (ANTHROPIC_REFERENCE_IS_POPULATED
  // is false until someone runs gen_reference_review.py) -- this exercises
  // that actual current state, not a mock, so it fails loudly if the guard
  // ever regresses.
  it("shows a 'not captured yet' placeholder while the fixture is unpopulated, not a false result", () => {
    render(<AnthropicReferenceCard />);

    expect(screen.getByText(/hasn't been captured/i)).toBeTruthy();
    expect(screen.queryByText(/no issues found/i)).toBeNull();
    expect(screen.queryByText(/0 in.*0 out/)).toBeNull();
  });

  // The populated branch can't be exercised against the real (still-empty)
  // fixture, so this mocks it with a filled-in shape instead -- decoupled
  // from the fixture's current placeholder values, so this stays covered
  // once the real capture lands (see bedrockPricing.test.ts's
  // computeSavingsPct tests for the same "don't test against the
  // placeholder" lesson applied elsewhere in this feature).
  it("shows the real findings and token counts once the fixture is populated", async () => {
    vi.resetModules();
    vi.doMock("../../data/adlcReviewAgentLab/anthropicReferenceReview", () => ({
      ANTHROPIC_REFERENCE_MODEL_LABEL: "Claude Sonnet 4.5",
      ANTHROPIC_REFERENCE_IS_POPULATED: true,
      ANTHROPIC_REFERENCE_FINDINGS: [{ severity: "critical", location: "api/spend.py:16", comment: "SQL injection." }],
      ANTHROPIC_REFERENCE_INPUT_TOKENS: 1500,
      ANTHROPIC_REFERENCE_OUTPUT_TOKENS: 300,
      ANTHROPIC_REFERENCE_CONTEXT_STATS: {
        kbChunksRetrieved: 4,
        kbChunksTotal: 12,
        kbTokensEstimated: 800,
        memoriesRetrieved: 0,
        memoriesTotal: 0,
        memoryTokensEstimated: 0,
      },
    }));
    const { AnthropicReferenceCard: MockedCard } = await import("./AnthropicReferenceCard");
    render(<MockedCard />);

    expect(screen.getAllByText(new RegExp(ANTHROPIC_REFERENCE_MODEL_LABEL)).length).toBeGreaterThan(0);
    expect(screen.getByText("SQL injection.")).toBeTruthy();
    expect(screen.getByText(/1500 in/)).toBeTruthy();
    expect(screen.getByText(/300 out/)).toBeTruthy();
    vi.doUnmock("../../data/adlcReviewAgentLab/anthropicReferenceReview");
    vi.resetModules();
  });
});
