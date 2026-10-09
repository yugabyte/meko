// The lesson merges two decisions that used to sit on separate screens, so
// this pins the thing that merge was for: both choices reach the caller that
// owns the config, because the review a chapter later runs from it.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactElement } from "react";

import { getDatapackMetrics } from "@/lib/api/mekoApi";
import { formatTokens } from "@/lib/tokenSavings";

import {
  FRONTIER_MODEL_ID,
  LAB_TOKENS_PER_CHUNK,
  reviewCost,
  TYPICAL_MATCHING_CHUNKS,
} from "@/features/labs/lib/reviewCostModel";
import { HowAgentReviewsStage } from "@/features/labs/components/stages/HowAgentReviewsStage";

// The comparison is derived from the datapack's real chunk count, so the
// stage now reads metrics. Inlined rather than referenced: vi.mock is
// hoisted above every const in this file.
vi.mock("@/lib/api/mekoApi", async (o) => ({
  ...(await o<typeof import("@/lib/api/mekoApi")>()),
  getDatapackMetrics: vi.fn().mockResolvedValue({ kb_chunk_count: 120 }),
}));
const KB_CHUNKS = 120;

function renderWithClient(ui: ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}
import {
  DEFAULT_REVIEW_AGENT_CONFIG,
  MODEL_OPTIONS,
  type ReviewAgentConfig,
} from "@/features/labs/components/demo-apps/ReviewAgentConfigPanel";

afterEach(cleanup);

function renderStage(overrides: Partial<ReviewAgentConfig> = {}) {
  const onConfigChange = vi.fn();
  const onGoToCreateDatapack = vi.fn();
  renderWithClient(
    <HowAgentReviewsStage
      agentDisplayName="AgentK"
      datapackId="dp-1"
      config={{ ...DEFAULT_REVIEW_AGENT_CONFIG, ...overrides }}
      onConfigChange={onConfigChange}
      onGoToCreateDatapack={onGoToCreateDatapack}
    />,
  );
  return { onConfigChange, onGoToCreateDatapack };
}

describe("HowAgentReviewsStage", () => {
  it("asks for a datapack first, since both choices are about reading one", () => {
    renderWithClient(
      <HowAgentReviewsStage
        agentDisplayName="AgentK"
        datapackId={null}
        config={DEFAULT_REVIEW_AGENT_CONFIG}
        onConfigChange={vi.fn()}
        onGoToCreateDatapack={vi.fn()}
      />,
    );
    expect(screen.queryByText("Use datapack as context")).toBeNull();
  });

  // "Without datapack" has to turn BOTH search tools off. Leaving one on
  // would send the card's claim and the actual invoke payload in different
  // directions, and Chapter 3 runs the payload.
  it("turns both search tools off when the reader picks no datapack", () => {
    const { onConfigChange } = renderStage();
    fireEvent.click(screen.getByText("Without datapack"));
    expect(onConfigChange).toHaveBeenCalledWith(
      expect.objectContaining({ useKnowledgeBase: false, useMemories: false }),
    );
  });

  it("reports the chosen model to the owner of the config", () => {
    const { onConfigChange } = renderStage();
    fireEvent.click(screen.getByText(MODEL_OPTIONS[1].label));
    expect(onConfigChange).toHaveBeenCalledWith(
      expect.objectContaining({ model: MODEL_OPTIONS[1].value }),
    );
  });

  // The frontier card is the baseline the saving is quoted against. Letting
  // it be selected would change what Chapter 3 actually runs.
  it("offers the frontier model for comparison only", () => {
    const { onConfigChange } = renderStage();
    const frontier = screen.getByText("Claude Sonnet 4.5").closest("button");
    expect((frontier as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(frontier as HTMLButtonElement);
    expect(onConfigChange).not.toHaveBeenCalled();
  });

  // Both sides come off the reader's own chunk count. Anything hardcoded
  // here was the bug this replaced.
  it("prices both sides off the datapack's real chunk count", async () => {
    renderStage();
    expect(
      await screen.findByText(new RegExp(`${TYPICAL_MATCHING_CHUNKS} of your ${KB_CHUNKS} chunks`)),
    ).toBeTruthy();
    expect(screen.getByText(new RegExp(`all ${KB_CHUNKS} chunks ride along`))).toBeTruthy();
    // Twice: the strategy card and the frontier baseline card beside it,
    // which is the point -- both are the no-datapack case.
    expect(screen.getAllByText(formatTokens(KB_CHUNKS * LAB_TOKENS_PER_CHUNK))).toHaveLength(2);
  });

  // The two cards compare what a team actually does, not one model against
  // itself: with no context it can trust, it reaches for the frontier model.
  // Pricing both sides on the selected small model hid half the gap.
  it("prices the no-datapack card on the frontier model, whatever is selected", async () => {
    renderStage();
    await screen.findByText(new RegExp(`all ${KB_CHUNKS} chunks ride along`));

    const frontierCost = reviewCost(FRONTIER_MODEL_ID, false, KB_CHUNKS).dollars!;
    const selectedCost = reviewCost(MODEL_OPTIONS[0].value, false, KB_CHUNKS).dollars!;
    expect(frontierCost).toBeGreaterThan(selectedCost);
    expect(screen.getAllByText(`$${frontierCost.toFixed(4)}`).length).toBeGreaterThan(0);
    expect(screen.getByText(/Priced on Claude Sonnet 4\.5/)).toBeTruthy();
  });

  // The savings line divides by the frontier-with-no-datapack cost, so the
  // card labelled as that baseline has to show the same number. Pricing it
  // on the selected strategy put two different figures for one model on one
  // screen, with nothing saying why they differed.
  it("shows the same baseline the savings line divides by", async () => {
    renderStage();
    await screen.findByText(new RegExp(`all ${KB_CHUNKS} chunks ride along`));

    const baseline = reviewCost(FRONTIER_MODEL_ID, false, KB_CHUNKS).dollars!;
    // Strategy card and baseline model card.
    expect(screen.getAllByText(`$${baseline.toFixed(4)}`)).toHaveLength(2);
    expect(screen.getByText("no datapack")).toBeTruthy();
    // The with-datapack price for the same model is no longer on screen.
    const withPack = reviewCost(FRONTIER_MODEL_ID, true, KB_CHUNKS).dollars!;
    expect(screen.queryByText(`$${withPack.toFixed(4)}`)).toBeNull();
  });

  // A datapack holding less than one match sends all of it. That is the true
  // answer, not a reason to substitute a flattering one.
  it("never claims to match more chunks than the datapack holds", async () => {
    vi.mocked(getDatapackMetrics).mockResolvedValueOnce({ kb_chunk_count: 0 } as never);
    renderStage();
    expect(await screen.findByText(/0 of your 0 chunks/)).toBeTruthy();
  });

  it("reports a focus note, which lives behind a disclosure", () => {
    const { onConfigChange } = renderStage();
    fireEvent.click(screen.getByText("+ Tone, strictness, focus"));
    fireEvent.change(screen.getByPlaceholderText(/pay extra attention/i), {
      target: { value: "Check for SQL injection." },
    });
    expect(onConfigChange).toHaveBeenCalledWith(
      expect.objectContaining({ focus: "Check for SQL injection." }),
    );
  });
});
