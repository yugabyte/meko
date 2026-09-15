import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactElement } from "react";

import type { PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
import { MODEL_OPTIONS } from "../demo-apps/ReviewAgentConfigPanel";
import { ReviewCostStage } from "./ReviewCostStage";

// The datapack rollup appended to this page reads token savings and
// conversations, so the stage now needs a query client and those endpoints.
vi.mock("@/lib/api/mekoApi", async (o) => ({
  ...(await o<typeof import("@/lib/api/mekoApi")>()),
  listDatapackConversations: vi.fn().mockResolvedValue({ conversations: [] }),
  getDatapackConversation: vi.fn().mockResolvedValue({
    conversation: { thread: [{ message_id: "m-1" }] },
  }),
  getDatapackMessageDecisionTrace: vi.fn().mockResolvedValue({
    decision_trace: {
      message_id: "m-1",
      latency_ms: 0,
      total_tokens: 0,
      llm_calls: 1,
      memories_extracted: 0,
      kb_hits: [
        { document_id: "d1", document_name: "org-coding-standards.md", chunk_text: "Never build SQL by concatenation.", score: 0.8 },
      ],
      memory_hits: [],
    },
  }),
  getDatapackTokenSavings: vi.fn().mockResolvedValue({
    total_requests: 0,
    total_tokens_saved: 0,
    tokens_saved_by_source: {},
    by_datapack: [],
  }),
}));

function renderStage(ui: ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
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
    ...overrides,
  };
}

describe("ReviewCostStage", () => {
  it("sends the user back to the review rather than showing numbers nobody measured", () => {
    const onGoToRunReview = vi.fn();
    renderStage(<ReviewCostStage run={null} datapackId="dp-1" onGoToRunReview={onGoToRunReview} />);

    fireEvent.click(screen.getByRole("button", { name: /go to run the review/i }));
    expect(onGoToRunReview).toHaveBeenCalled();
  });

  it("shows the chosen model's real token usage from the run", () => {
    renderStage(<ReviewCostStage run={runWith()} datapackId="dp-1" onGoToRunReview={vi.fn()} />);

    expect(screen.getByText("512")).toBeTruthy();
    expect(screen.getByText("128")).toBeTruthy();
    expect(screen.getByText(new RegExp(MODEL_OPTIONS[0].label))).toBeTruthy();
  });

  it("shows real knowledgebase_search/memory_search retrieval counts from the run", () => {
    const run = runWith({
      contextStats: {
        kb_chunks_retrieved: 10,
        kb_chunks_total: 70,
        kb_tokens_estimated: 410,
        memories_retrieved: 3,
        memories_total: 24,
        memory_tokens_estimated: 85,
      },
    });
    renderStage(<ReviewCostStage run={run} datapackId="dp-1" onGoToRunReview={vi.fn()} />);

    expect(screen.getByText(/10 of 70 chunks/)).toBeTruthy();
    expect(screen.getByText(/3 of 24 memories/)).toBeTruthy();
  });

  it("shows no cost-savings card while the Claude reference has no real captured tokens yet", () => {
    // ANTHROPIC_REFERENCE_INPUT_TOKENS/OUTPUT_TOKENS are 0 until
    // gen_reference_review.py's real output is pasted into the fixture --
    // dividing by a zero reference cost must not render a bogus "Infinity%"
    // or "NaN%" savings line.
    renderStage(<ReviewCostStage run={runWith()} datapackId="dp-1" onGoToRunReview={vi.fn()} />);

    expect(screen.queryByText(/less, from this run/)).toBeNull();
  });

  it("hides the paste comparison when no retrieval happened, leaving nothing to compare", () => {
    // Both search tools off -> no retrieved tokens -> a second bar would be
    // a claim about a review that never queried anything.
    renderStage(<ReviewCostStage run={runWith()} datapackId="dp-1" onGoToRunReview={vi.fn()} />);

    expect(screen.queryByText(/Input tokens per review/)).toBeNull();
  });

  it("compares against a paste baseline once retrieval actually contributed tokens", () => {
    const run = runWith({
      contextStats: {
        kb_chunks_retrieved: 2,
        kb_chunks_total: 12,
        kb_tokens_estimated: 200,
        memories_retrieved: 1,
        memories_total: 4,
        memory_tokens_estimated: 40,
      },
    });
    renderStage(<ReviewCostStage run={run} datapackId="dp-1" onGoToRunReview={vi.fn()} />);

    expect(screen.getByText(/Input tokens per review/)).toBeTruthy();
    expect(screen.getByText(/Paste the whole standards doc/)).toBeTruthy();
    // The measured run must be the smaller of the two bars, or the lesson
    // this stage teaches is backwards.
    expect(screen.getByText(/fewer input tokens on this review/)).toBeTruthy();
  });

  // The datapack rollup is the half that outlives the session, so it gets its
  // own tab rather than sitting under this run's numbers.
  it("keeps the datapack rollup behind the Metrics tab", () => {
    renderStage(<ReviewCostStage run={runWith()} datapackId="dp-1" onGoToRunReview={vi.fn()} />);
    expect(screen.getByRole("tab", { name: "Cost" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Metrics" })).toBeTruthy();
    expect(screen.queryByText(/What the datapack has built up/)).toBeNull();

    // Radix activates a tab on mousedown, not click.
    fireEvent.mouseDown(screen.getByRole("tab", { name: "Metrics" }));
    expect(screen.getByText(/What the datapack has built up/)).toBeTruthy();
  });
});
