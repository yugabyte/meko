import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import type { ReactElement } from "react";

import type { PrReviewRunState } from "@/features/labs/components/demo-apps/PrReviewDemoApp";
import { MODEL_OPTIONS } from "@/features/labs/components/demo-apps/ReviewAgentConfigPanel";
import { ReviewCostStage } from "@/features/labs/components/stages/ReviewCostStage";

// The Metrics tab renders the product's own Analytics section, so the stage
// needs a query client, a router (its cards link to the learnings page) and
// the datapack endpoints behind both.
vi.mock("@/lib/api/mekoApi", async (o) => ({
  ...(await o<typeof import("@/lib/api/mekoApi")>()),
  getDatapack: vi.fn().mockResolvedValue({
    datapack_id: "dp-1",
    datapack_name: "org-standards",
    knowledge_count: 1,
    memory_count: 2,
    collective_memory_count: 0,
    learnings_count: 0,
  }),
  getDatapackMetrics: vi.fn().mockResolvedValue({
    total_requests: 0,
    kb_chunk_count: 0,
    memory_chunk_count: 0,
    conversation_chunk_count: 0,
    kb_bytes: 0,
    memory_bytes: 0,
    conversation_bytes: 0,
    score_high_total: 0,
    score_mid_total: 0,
    score_low_total: 0,
    daily_activity: [],
  }),
}));

function renderStage(ui: ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

afterEach(cleanup);

function runWith(overrides: Partial<PrReviewRunState> = {}): PrReviewRunState {
  return {
    messages: [],
    diffTitle: null,
    diffContent: null,
    isSample: true,
    sourceUrl: null,
    conversationId: "conv-1",
    reviewText: "Looks good.",
    reviewUsage: { input_tokens: 512, output_tokens: 128 },
    contextStats: null,
    reviewFindings: [],
    initialReviewMessageId: null,
    selectedModelId: MODEL_OPTIONS[0].value,
    reviewPromptMessageId: null,
    sendError: null,
    durationMs: null,
    ...overrides,
  };
}

const RETRIEVED = {
  kb_chunks_retrieved: 2,
  kb_chunks_total: 12,
  kb_tokens_estimated: 200,
  memories_retrieved: 1,
  memories_total: 4,
  memory_tokens_estimated: 40,
};

describe("ReviewCostStage", () => {
  it("sends the user back to the review rather than showing numbers nobody measured", () => {
    const onGoToRunReview = vi.fn();
    renderStage(
      <ReviewCostStage
        run={null}
        datapackId="dp-1"
        onGoToRunReview={onGoToRunReview}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: /go to run the review/i }));
    expect(onGoToRunReview).toHaveBeenCalled();
  });

  // review.py sends `findings: []` for a clean diff AND for a response it could
  // not parse. Rendering "0 issues found" in the good tone over the second is
  // the same false-clean claim this chapter exists to remove, two steps later.
  it("does not report zero issues found when the findings list is empty", () => {
    renderStage(
      <ReviewCostStage
        run={runWith({ reviewFindings: [] })}
        datapackId="dp-1"
        onGoToRunReview={vi.fn()}
      />,
    );

    expect(screen.queryByText(/0 issues found/i)).toBeNull();
    // And not "Answered in prose" either: an empty list is usually a parse
    // the agent could not do, or a clean diff, neither of which is prose.
    expect(screen.queryByText(/Answered in prose/i)).toBeNull();
    expect(screen.getByText(/No count to report/i)).toBeTruthy();
  });

  it("shows the chosen model's real token usage from the run", () => {
    renderStage(
      <ReviewCostStage
        run={runWith()}
        datapackId="dp-1"
        onGoToRunReview={vi.fn()}
      />,
    );

    // Twice now: the comparison tile quotes the same measured input the stat
    // grid does, which is the point of dropping the invented figure.
    expect(screen.getAllByText("512")).toHaveLength(2);
    expect(screen.getByText("128")).toBeTruthy();
    expect(screen.getAllByText(new RegExp(MODEL_OPTIONS[0].label)).length).toBeGreaterThan(0);
  });

  it("shows real knowledgebase_search/memory_search retrieval counts from the run", () => {
    renderStage(
      <ReviewCostStage
        run={runWith({ contextStats: { ...RETRIEVED, kb_chunks_retrieved: 10, kb_chunks_total: 70 } })}
        datapackId="dp-1"
        onGoToRunReview={vi.fn()}
      />,
    );

    expect(screen.getByText(/10 of 70 chunks/)).toBeTruthy();
    expect(screen.getByText(/1 of 4 memories/)).toBeTruthy();
  });

  // The with-datapack side is the run's measured input; the cold side is an
  // estimate of the WHOLE knowledge base, not a multiple of what matched.
  it("quotes the measured input against the whole knowledge base", () => {
    renderStage(
      <ReviewCostStage
        run={runWith({ contextStats: RETRIEVED })}
        datapackId="dp-1"
        onGoToRunReview={vi.fn()}
      />,
    );

    // 512 measured, against 12 chunks in the KB at 1,000 tokens each.
    expect(screen.getAllByText("512").length).toBeGreaterThan(0);
    expect(screen.getByText("12,000 est.")).toBeTruthy();
  });

  // The bars sit directly under the stat tile reporting the same total, so
  // they have to add up to it.
  it("divides the measured input between the two searches", () => {
    renderStage(
      <ReviewCostStage
        run={runWith({ contextStats: RETRIEVED })}
        datapackId="dp-1"
        onGoToRunReview={vi.fn()}
      />,
    );

    // 2 chunks (weight 4) against 1 memory (weight 1), of 512.
    expect(screen.getByText("~410 tokens")).toBeTruthy();
    expect(screen.getByText("~102 tokens")).toBeTruthy();
  });

  it("names both models and their published rates, so the tiles compare like for like", () => {
    renderStage(
      <ReviewCostStage
        run={runWith({ contextStats: RETRIEVED })}
        datapackId="dp-1"
        onGoToRunReview={vi.fn()}
      />,
    );

    expect(screen.getByText("Claude Sonnet 4.5")).toBeTruthy();
    expect(screen.getByText("$3.00 / $15.00")).toBeTruthy();
    expect(screen.getAllByText("Model cost (in / out per M)")).toHaveLength(2);
  });

  it("shows no cost-savings card while the Claude reference has no real captured tokens yet", () => {
    // ANTHROPIC_REFERENCE_INPUT_TOKENS/OUTPUT_TOKENS are 0 until
    // gen_reference_review.py's real output is pasted into the fixture --
    // dividing by a zero reference cost must not render a bogus "Infinity%"
    // or "NaN%" savings line.
    renderStage(
      <ReviewCostStage
        run={runWith()}
        datapackId="dp-1"
        onGoToRunReview={vi.fn()}
      />,
    );

    expect(screen.queryByText(/less, from this run/)).toBeNull();
  });

  // The datapack rollup is the half that outlives the session, so it gets its
  // own tab rather than sitting under this run's numbers.
  it("keeps the datapack's own analytics behind the Metrics tab", () => {
    renderStage(
      <ReviewCostStage
        run={runWith()}
        datapackId="dp-1"
        onGoToRunReview={vi.fn()}
      />,
    );
    expect(screen.getByRole("tab", { name: "Cost" })).toBeTruthy();
    expect(screen.queryByText("Context Size - by token count")).toBeNull();

    // Radix activates a tab on mousedown, not click.
    fireEvent.mouseDown(screen.getByRole("tab", { name: "Metrics" }));
    expect(screen.getByText("Context Size - by token count")).toBeTruthy();
  });
});
