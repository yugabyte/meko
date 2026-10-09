import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { SAMPLE_DIFF_CONTENT } from "@/features/labs/data/adlcReviewAgentLab/sampleDiff";
import type { PrReviewRunState } from "@/features/labs/components/demo-apps/PrReviewDemoApp";
import { MODEL_OPTIONS } from "@/features/labs/components/demo-apps/ReviewAgentConfigPanel";
import { ReviewFindingsStage } from "@/features/labs/components/stages/ReviewFindingsStage";

const KB_HIT = {
  document_id: "d1",
  document_name: "org-coding-standards.md",
  chunk_text: "Never build a SQL string by concatenation. Use parameterized queries.",
  score: 0.82,
};
const MEMORY_HIT = {
  id: "m1",
  text: "We got burned by a SQL injection in the search endpoint last quarter.",
  kind: "private",
  score: 0.7,
};

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
    diffContent: SAMPLE_DIFF_CONTENT,
    isSample: true,
    sourceUrl: null,
    conversationId: "conv-1",
    reviewText: "Looks good.",
    reviewUsage: null,
    contextStats: null,
    reviewFindings: [
      {
        severity: "critical",
        location: "/api/spend.py:19",
        comment:
          "SQL injection: team_id is concatenated straight into the query. Use a parameterized query instead.",
      },
    ],
    initialReviewMessageId: null,
    selectedModelId: MODEL_OPTIONS[0].value,
    reviewPromptMessageId: null,
    sendError: null,
    durationMs: null,
    ...overrides,
  };
}

afterEach(cleanup);

describe("ReviewFindingsStage", () => {
  // THE bug path. review.py returns `findings: []` both for a clean diff and
  // for a response it could not parse, and recoverReviewFindings has already
  // tried to tell them apart. What is left is unresolvable, so claiming "No
  // blocking issues found" over the model's own words is how a diff with a
  // SQL injection in it read as clean.
  it("shows the response rather than claiming clean when findings are empty", async () => {
    okTrace();
    render(
      <ReviewFindingsStage
        run={runWith({
          reviewFindings: [],
          reviewText: "The team_id parameter is concatenated straight into the SQL string.",
        })}
        datapackId="dp-1"
        onGoToRunReview={vi.fn()}
      />,
    );

    expect(await screen.findByText(/concatenated straight into the SQL string/)).toBeTruthy();
    expect(screen.queryByText(/No blocking issues found/i)).toBeNull();
    expect(screen.queryByText(/Run the review first/i)).toBeNull();
  });

  // "unstructured" is the other route to the same screen: a deployed image
  // whose done event carries no findings key at all.
  it("shows the response when the agent reported no structured findings", async () => {
    okTrace();
    render(
      <ReviewFindingsStage
        run={runWith({
          reviewFindings: "unstructured",
          reviewText: "The team_id parameter is concatenated straight into the SQL string.",
        })}
        datapackId="dp-1"
        onGoToRunReview={vi.fn()}
      />,
    );

    expect(await screen.findByText(/concatenated straight into the SQL string/)).toBeTruthy();
    expect(screen.queryByText(/Run the review first/i)).toBeNull();
  });

  // `run` is non-null from the moment the run stage mounts (its effect
  // reports state up immediately), and stage completion outlives the run
  // itself, so a reload lands here with reviewFindings still null. That is
  // "go and run one", not "the agent answered in prose".
  it("sends the user back to the run step when no review has completed", () => {
    okTrace();
    render(
      <ReviewFindingsStage
        run={runWith({ reviewFindings: null, reviewText: null })}
        datapackId="dp-1"
        onGoToRunReview={vi.fn()}
      />,
    );

    expect(screen.getByText(/Run the review first/i)).toBeTruthy();
    expect(screen.queryByText(/What the review said/i)).toBeNull();
  });

  // The whole claim of this step is that the comments came from rules the
  // user loaded, so the rules sit on the comment they explain.
  it("attaches the rules a comment echoes to that comment", async () => {
    okTrace();
    render(<ReviewFindingsStage run={runWith()} datapackId="dp-1" onGoToRunReview={vi.fn()} />);

    expect(await screen.findByText(/Never build a SQL string by concatenation/)).toBeTruthy();
    expect(screen.getByText("org-coding-standards.md")).toBeTruthy();
    expect(screen.getByText(/got burned by a SQL injection/)).toBeTruthy();
  });

  // The pairing is computed from word overlap, not reported by the backend.
  // Saying so is the difference between a hint and a false citation.
  it("says the pairing is a match, not something the trace recorded", async () => {
    okTrace();
    render(<ReviewFindingsStage run={runWith()} datapackId="dp-1" onGoToRunReview={vi.fn()} />);

    expect(await screen.findByText(/not which chunk produced which comment/i)).toBeTruthy();
  });

  it("shows the flagged line of the diff, numbered in the file's own frame", async () => {
    okTrace();
    render(<ReviewFindingsStage run={runWith()} datapackId="dp-1" onGoToRunReview={vi.fn()} />);

    expect(await screen.findByText("api/spend.py")).toBeTruthy();
    // Line 19 is the concatenation the finding points at.
    expect(screen.getByText("19")).toBeTruthy();
    expect(screen.getByText(/\+ team_id/)).toBeTruthy();
  });

  // The agent annotates most of its locations. Before this was handled, the
  // annotation made the location unparseable and no code rendered at all.
  it("still finds the code when the location carries a trailing note", async () => {
    okTrace();
    render(
      <ReviewFindingsStage
        run={runWith({
          reviewFindings: [
            {
              severity: "critical",
              location: "spend.py:19 (string concatenation in SQL)",
              comment: "SQL injection.",
            },
          ],
        })}
        datapackId="dp-1"
        onGoToRunReview={vi.fn()}
      />,
    );

    expect(await screen.findByText("api/spend.py")).toBeTruthy();
    expect(screen.getByText(/\+ team_id/)).toBeTruthy();
  });

  // The agent writes its comments with markdown emphasis. Rendered raw, the
  // heading read `**No input validation**` with the asterisks visible.
  it("renders the comment's markdown rather than printing its syntax", async () => {
    okTrace();
    render(
      <ReviewFindingsStage
        run={runWith({
          reviewFindings: [
            {
              severity: "major",
              location: "spend.py:15",
              comment: "**No input validation** on `request.args.get`.",
            },
          ],
        })}
        datapackId="dp-1"
        onGoToRunReview={vi.fn()}
      />,
    );

    expect(await screen.findByText("No input validation")).toBeTruthy();
    expect(screen.getByText("request.args.get")).toBeTruthy();
    expect(screen.queryByText(/\*\*/)).toBeNull();
  });

  // A location the diff does not contain must not mark a line at random.
  it("falls back to the bare location when it resolves to no code", async () => {
    okTrace();
    render(
      <ReviewFindingsStage
        run={runWith({
          reviewFindings: [
            { severity: "major", location: "other/file.py:900", comment: "Something else." },
          ],
        })}
        datapackId="dp-1"
        onGoToRunReview={vi.fn()}
      />,
    );

    expect(await screen.findByText("other/file.py:900")).toBeTruthy();
    expect(screen.queryByText("api/spend.py")).toBeNull();
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
