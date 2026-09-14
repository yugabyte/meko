import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import type { PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
import { MODEL_OPTIONS } from "../demo-apps/ReviewAgentConfigPanel";
import { ReviewFindingsStage } from "./ReviewFindingsStage";

const KB_HIT = {
  document_id: "d1",
  document_name: "org-coding-standards.md",
  chunk_text: "Never build a SQL string by concatenation.",
  score: 0.82,
};
const MEMORY_HIT = { id: "m1", text: "We got burned by SQL injection last quarter.", kind: "private", score: 0.7 };

const trace = vi.fn();

vi.mock("@/lib/api/mekoApi", async (o) => ({
  ...(await o<typeof import("@/lib/api/mekoApi")>()),
  getDatapackConversation: vi.fn().mockResolvedValue({
    conversation: { thread: [{ message_id: "msg-1" }] },
  }),
  getDatapackMessageDecisionTrace: (...args: unknown[]) => trace(...args),
}));

function okTrace(kbHits: unknown[] = [KB_HIT], memoryHits: unknown[] = [MEMORY_HIT]) {
  trace.mockResolvedValue({
    decision_trace: {
      message_id: "msg-1",
      latency_ms: 0,
      total_tokens: 0,
      llm_calls: 1,
      memories_extracted: 0,
      kb_hits: kbHits,
      memory_hits: memoryHits,
    },
  });
}

function runWith(overrides: Partial<PrReviewRunState> = {}): PrReviewRunState {
  return {
    messages: [],
    diffTitle: null,
    diffContent: null,
    isSample: true,
    sourceUrl: null,
    conversationId: "conv-1",
    reviewText: "Looks good.",
    reviewUsage: null,
    contextStats: null,
    reviewFindings: [
      { severity: "critical", location: "usage.py:22", comment: "SQL built by concatenation." },
    ],
    initialReviewMessageId: null,
    selectedModelId: MODEL_OPTIONS[0].value,
    reviewPromptMessageId: null,
    sendError: null,
    ...overrides,
  };
}

afterEach(cleanup);

describe("ReviewFindingsStage", () => {
  // The whole claim of this step is that the comments came from rules the
  // user loaded, so the rules have to be readable here and not a page later.
  it("quotes the chunks and memories the review retrieved", async () => {
    okTrace();
    render(<ReviewFindingsStage run={runWith()} datapackId="dp-1" onGoToRunReview={vi.fn()} />);

    expect(await screen.findByText(/Never build a SQL string by concatenation/)).toBeTruthy();
    expect(screen.getByText("org-coding-standards.md")).toBeTruthy();
    expect(screen.getByText(/got burned by SQL injection/)).toBeTruthy();
    expect(screen.getByText("private memory")).toBeTruthy();
  });

  // Retrieving nothing is a result, not a blank space: it means these
  // comments are the model on its own.
  it("says so when the review matched nothing in the datapack", async () => {
    okTrace([], []);
    render(<ReviewFindingsStage run={runWith()} datapackId="dp-1" onGoToRunReview={vi.fn()} />);

    expect(await screen.findByText(/every comment above is the model on its own/i)).toBeTruthy();
  });

  it("sends the user back to the review when nothing has run yet", () => {
    const onGoToRunReview = vi.fn();
    render(<ReviewFindingsStage run={null} datapackId="dp-1" onGoToRunReview={onGoToRunReview} />);
    screen.getByRole("button", { name: /go to run the review/i }).click();
    expect(onGoToRunReview).toHaveBeenCalled();
  });
});
