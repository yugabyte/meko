import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { invokeDeployment, invokeSharedDeployment, type ReviewStreamEvent } from "@/lib/api/deploymentInvokeApi";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDemoAppConfig } from "../../types";
import { MODEL_OPTIONS } from "./ReviewAgentConfigPanel";
import { PrReviewDemoApp } from "./PrReviewDemoApp";

// The wiring contract this test locks: whatever the "Configure your
// reviewer" panel is set to must reach the deployed-agent invocation
// untouched — these are real settings the agent acts on, not decorative UI.
vi.mock("@/lib/api/deploymentInvokeApi", () => ({
  invokeDeployment: vi.fn(),
  invokeSharedDeployment: vi.fn(),
  InvokeDeploymentError: class InvokeDeploymentError extends Error {},
}));

const mockInvokeDeployment = vi.mocked(invokeDeployment);
const mockInvokeSharedDeployment = vi.mocked(invokeSharedDeployment);

async function* fakeStream(events: ReviewStreamEvent[]) {
  for (const event of events) yield event;
}

// The initial review's content now comes from `findings`, not `review`/
// text_delta -- review.py's response is structured JSON, never streamed
// character-by-character (see ReviewStreamEvent's own doc comment). Tests
// below that just need *some* review content to appear use this single
// finding's comment text ("Looks good.") as that content.
const DEFAULT_FINDINGS: NonNullable<Extract<ReviewStreamEvent, { type: "done" }>["findings"]> = [
  { severity: "minor", location: "", comment: "Looks good." },
];

const DEFAULT_STREAM: ReviewStreamEvent[] = [
  { type: "meta", conversation_id: "conv-1" },
  { type: "done", review: "Looks good.", findings: DEFAULT_FINDINGS },
];

const DEMO_APP: LabDemoAppConfig = { kind: "pr-review-chat", agentDisplayName: "AgentK" };
const DEMO_APP_WITH_DEPLOYABLE: LabDemoAppConfig = {
  kind: "pr-review-chat",
  agentDisplayName: "AgentK",
  deployable: { agentSlug: "adlc-review-agent", sourceRepoUrl: "https://github.com/yugabyte/canned-agents" },
};

function fakeSession(
  overrides: Partial<ReturnType<typeof useLabRunnerSession>> = {},
): ReturnType<typeof useLabRunnerSession> {
  return {
    userId: "user-1",
    currentUser: {
      account_id: "user-1",
      email: "test@example.com",
      first_name: "Test",
      last_name: "User",
      has_completed_onboarding: true,
    },
    datapackId: "dp-1",
    datapackName: "test-datapack",
    datapackGrant: null,
    accessibleDatapacks: [],
    datapacksLoading: false,
    selectDatapack: vi.fn(),
    createAndSelectDatapack: vi.fn(),
    creating: false,
    hasHitDatapackCount: false,
    userToken: "mko_tkn_fake",
    loading: false,
    error: null,
    refreshTick: 0,
    bumpRefreshTick: vi.fn(),
    lastLessonTrace: null,
    recordLessonTrace: vi.fn(),
    deploymentId: "dep-1",
    selectDeployment: vi.fn(),
    clearDeployment: vi.fn(),
    deploymentMode: "dedicated" as const,
    selectDeploymentMode: vi.fn(),
    deployment: { id: "dep-1", status: "active" } as ReturnType<typeof useLabRunnerSession>["deployment"],
    deploymentActive: true,
    ampClusterId: null,
    ampBranchId: null,
    selectAmpCluster: vi.fn(),
    clearAmpCluster: vi.fn(),
    ...overrides,
  };
}

afterEach(cleanup);
beforeEach(() => {
  vi.clearAllMocks();
  mockInvokeDeployment.mockReturnValue(fakeStream(DEFAULT_STREAM));
  mockInvokeSharedDeployment.mockReturnValue(fakeStream(DEFAULT_STREAM));
});

describe("PrReviewDemoApp guided-customization wiring", () => {
  it("shows the config panel and Run review button immediately, before running anything", () => {
    render(<PrReviewDemoApp demoApp={DEMO_APP} session={fakeSession()} />);

    expect(screen.getByRole("button", { name: /run review/i })).toBeTruthy();
    expect(mockInvokeDeployment).not.toHaveBeenCalled();
  });

  it("shows no diff preview before running when using the bundled sample (it has its own earlier stage)", () => {
    render(<PrReviewDemoApp demoApp={DEMO_APP} session={fakeSession()} />);
    expect(screen.queryByText(/view on github/i)).toBeNull();
  });

  it("previews a real PR's title/diff and links to it before running, when given a prSource", () => {
    render(
      <PrReviewDemoApp
        demoApp={DEMO_APP}
        session={fakeSession()}
        prSource={{
          title: "Fix the thing",
          diff: "diff --git a/x b/x\n+fixed\n",
          htmlUrl: "https://github.com/acme/widgets/pull/42",
        }}
      />,
    );
    expect(screen.getByText("Fix the thing")).toBeTruthy();
    expect(screen.getByText("+fixed")).toBeTruthy();
    const link = screen.getByRole("link", { name: /view on github/i }) as HTMLAnchorElement;
    expect(link.href).toBe("https://github.com/acme/widgets/pull/42");
  });

  it("runs the review with the prSource's title/diff, not the bundled sample", async () => {
    render(
      <PrReviewDemoApp
        demoApp={DEMO_APP}
        session={fakeSession()}
        prSource={{ title: "Fix the thing", diff: "diff --git a/x b/x\n+fixed\n" }}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /run review/i }));

    await waitFor(() => expect(mockInvokeDeployment).toHaveBeenCalledTimes(1));
    const [, payload] = mockInvokeDeployment.mock.calls[0];
    expect(payload.pr_title).toBe("Fix the thing");
    expect(payload.pr_diff).toBe("diff --git a/x b/x\n+fixed\n");
  });

  it("defaults both switches on and sends them, plus the default model, with the sample diff run", async () => {
    render(<PrReviewDemoApp demoApp={DEMO_APP} session={fakeSession()} />);

    fireEvent.click(screen.getByRole("button", { name: /run review/i }));

    await waitFor(() => expect(mockInvokeDeployment).toHaveBeenCalledTimes(1));
    const [deploymentId, payload] = mockInvokeDeployment.mock.calls[0];
    expect(deploymentId).toBe("dep-1");
    expect(payload.enable_knowledge_base).toBe(true);
    expect(payload.enable_memory_search).toBe(true);
    expect(payload.focus).toBeUndefined();
    expect(payload.tone).toBeUndefined();
    expect(payload.strictness).toBeUndefined();
    expect(payload.model).toBe(MODEL_OPTIONS[0].value);
  });




  it("shows no usage line when the deployed image predates this feature (no usage on done)", async () => {
    render(<PrReviewDemoApp demoApp={DEMO_APP} session={fakeSession()} />);

    fireEvent.click(screen.getByRole("button", { name: /run review/i }));

    await waitFor(() => expect(screen.getByText("Looks good.")).toBeTruthy());
    expect(screen.queryByText(/in \/.*out tokens/)).toBeNull();
  });

  it("forwards a disabled switch and a focus note to invokeDeployment", async () => {
    render(<PrReviewDemoApp demoApp={DEMO_APP} session={fakeSession()} />);

    // First switch rendered is the knowledgebase_search toggle chip (before
    // memory_search) — queried by role, not label text.
    fireEvent.click(screen.getAllByRole("switch")[0]);
    // Focus lives behind the "+ Tone, strictness, focus" disclosure,
    // collapsed by default.
    fireEvent.click(screen.getByText("+ Tone, strictness, focus"));
    fireEvent.change(screen.getByPlaceholderText(/pay extra attention/i), {
      target: { value: "Check for SQL injection." },
    });
    fireEvent.click(screen.getByRole("button", { name: /run review/i }));

    await waitFor(() => expect(mockInvokeDeployment).toHaveBeenCalledTimes(1));
    const [, payload] = mockInvokeDeployment.mock.calls[0];
    expect(payload.enable_knowledge_base).toBe(false);
    expect(payload.enable_memory_search).toBe(true);
    expect(payload.focus).toBe("Check for SQL injection.");
  });

  it("shows a loading state, then the parsed findings atomically once done", async () => {
    // The initial review's response is structured JSON (review.py's
    // REVIEW_SYSTEM_PROMPT) -- never streamed character-by-character, so
    // there's nothing to render incrementally; only a loading state until
    // the single "done" event arrives with `findings` already parsed.
    mockInvokeDeployment.mockReturnValue(
      fakeStream([
        { type: "meta", conversation_id: "conv-1" },
        {
          type: "done",
          review: "**CRITICAL** — SQL injection.",
          findings: [{ severity: "critical", location: "api/spend.py:16", comment: "SQL injection." }],
        },
      ]),
    );
    render(<PrReviewDemoApp demoApp={DEMO_APP} session={fakeSession()} />);

    fireEvent.click(screen.getByRole("button", { name: /run review/i }));

    expect(screen.getByText(/reviewing/i)).toBeTruthy();
    await waitFor(() => expect(screen.getByText("SQL injection.")).toBeTruthy());
    expect(screen.getByText("api/spend.py:16")).toBeTruthy();
    expect(screen.getByText("critical")).toBeTruthy();
  });

  it("falls back to the streamed prose when the deployed image predates structured findings", async () => {
    // A pre-upgrade deployed image streams free-text_delta prose and never
    // sends a "findings" key at all on "done" (not even an empty array) --
    // this must fall back to the accumulated prose, not get stuck showing
    // "Reviewing…" forever because reviewFindings never left its initial
    // (pending) state. Regression test for a real bug found by hand: an
    // actually-stale deployed AgentK returned exactly this shape live.
    mockInvokeDeployment.mockReturnValue(
      fakeStream([
        { type: "meta", conversation_id: "conv-1" },
        { type: "text_delta", text: "Looks " },
        { type: "text_delta", text: "fine, no issues." },
        { type: "done", review: "Looks fine, no issues." },
      ]),
    );
    render(<PrReviewDemoApp demoApp={DEMO_APP} session={fakeSession()} />);

    fireEvent.click(screen.getByRole("button", { name: /run review/i }));

    await waitFor(() => expect(screen.getByText("Looks fine, no issues.")).toBeTruthy());
    expect(screen.queryByText(/reviewing/i)).toBeNull();
    expect(screen.queryByText(/no issues found/i)).toBeNull();
  });

  it("shows the raw response, not a false 'no issues found', when the model's output didn't parse as JSON", async () => {
    // review.py degrades a malformed response to findings=[] rather than
    // failing the review, but the raw text it couldn't parse is still in
    // `review` -- an empty findings array alone can't distinguish this from
    // a genuine "no issues" result, so the UI must show the actual text
    // instead of confidently (and falsely) claiming "No issues found."
    mockInvokeDeployment.mockReturnValue(
      fakeStream([
        { type: "meta", conversation_id: "conv-1" },
        { type: "done", review: "Sure, here's my review: looks fine!", findings: [] },
      ]),
    );
    render(<PrReviewDemoApp demoApp={DEMO_APP} session={fakeSession()} />);

    fireEvent.click(screen.getByRole("button", { name: /run review/i }));

    await waitFor(() => expect(screen.getByText("Sure, here's my review: looks fine!")).toBeTruthy());
    expect(screen.queryByText(/no issues found/i)).toBeNull();
  });

  it("shows 'No issues found.' when the model's response is genuinely empty", async () => {
    // Distinct from the malformed-response case above: here review.py
    // parsed valid JSON with an empty findings array, so its rendered
    // markdown text is the real "No issues found." message.
    mockInvokeDeployment.mockReturnValue(
      fakeStream([
        { type: "meta", conversation_id: "conv-1" },
        { type: "done", review: "No issues found.", findings: [] },
      ]),
    );
    render(<PrReviewDemoApp demoApp={DEMO_APP} session={fakeSession()} />);

    fireEvent.click(screen.getByRole("button", { name: /run review/i }));

    await waitFor(() => expect(screen.getByText(/no issues found/i)).toBeTruthy());
  });

  it("never renders a Deploy panel, even for a deployable lab, even after a review completes", async () => {
    render(<PrReviewDemoApp demoApp={DEMO_APP_WITH_DEPLOYABLE} session={fakeSession()} />);

    fireEvent.click(screen.getByRole("button", { name: /run review/i }));
    await waitFor(() => expect(mockInvokeDeployment).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.getByText("Looks good.")).toBeTruthy());

    expect(screen.queryByText(/deploy this agent/i)).toBeNull();
  });

  it("sends a follow-up question against the same conversation once the review is done", async () => {
    render(<PrReviewDemoApp demoApp={DEMO_APP} session={fakeSession()} />);

    fireEvent.click(screen.getByRole("button", { name: /run review/i }));
    await waitFor(() => expect(screen.getByText("Looks good.")).toBeTruthy());

    mockInvokeDeployment.mockReturnValue(
      fakeStream([
        { type: "meta", conversation_id: "conv-1" },
        { type: "text_delta", text: "It's a false positive." },
        { type: "done", review: "It's a false positive." },
      ]),
    );

    const input = screen.getByPlaceholderText(/message|ask|type/i);
    fireEvent.change(input, { target: { value: "Why is that a problem?" } });
    fireEvent.keyDown(input, { key: "Enter" });

    await waitFor(() => expect(mockInvokeDeployment).toHaveBeenCalledTimes(2));
    const [, followUpPayload] = mockInvokeDeployment.mock.calls[1];
    expect(followUpPayload.conversation_id).toBe("conv-1");
    expect(followUpPayload.question).toBe("Why is that a problem?");
    expect(followUpPayload.prior_review_text).toBe("Looks good.");
  });

  it("shared mode calls invokeSharedDeployment instead, with no meko_pat in the payload", async () => {
    render(
      <PrReviewDemoApp
        demoApp={DEMO_APP}
        session={fakeSession({ deploymentMode: "shared", deploymentId: null, userToken: null })}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: /run review/i }));

    await waitFor(() => expect(mockInvokeSharedDeployment).toHaveBeenCalledTimes(1));
    expect(mockInvokeDeployment).not.toHaveBeenCalled();
    const [, payload] = mockInvokeSharedDeployment.mock.calls[0];
    expect(payload.datapack_id).toBe("dp-1");
    expect(payload.meko_pat).toBeUndefined();
    expect("meko_pat" in payload).toBe(false);
  });

  it("aborts the in-flight invokeDeployment call on unmount", async () => {
    async function* hangingStream(): AsyncGenerator<ReviewStreamEvent> {
      yield { type: "meta", conversation_id: "conv-1" };
      await new Promise(() => {}); // never resolves -- still "sending" at unmount
    }
    mockInvokeDeployment.mockReturnValue(hangingStream());

    const { unmount } = render(<PrReviewDemoApp demoApp={DEMO_APP} session={fakeSession()} />);
    fireEvent.click(screen.getByRole("button", { name: /run review/i }));

    await waitFor(() => expect(mockInvokeDeployment).toHaveBeenCalledTimes(1));
    const [, , signal] = mockInvokeDeployment.mock.calls[0];
    expect(signal?.aborted).toBe(false);

    unmount();

    expect(signal?.aborted).toBe(true);
  });

  it("reports the completed review's results via onRunChange", async () => {
    const onRunChange = vi.fn();
    render(<PrReviewDemoApp demoApp={DEMO_APP} session={fakeSession()} onRunChange={onRunChange} />);

    fireEvent.click(screen.getByRole("button", { name: /run review/i }));

    await waitFor(() => expect(screen.getByText("Looks good.")).toBeTruthy());
    const lastCall = onRunChange.mock.calls.at(-1)?.[0];
    expect(lastCall.diffContent).toBeTruthy();
    expect(lastCall.conversationId).toBe("conv-1");
    expect(lastCall.reviewFindings).toEqual(DEFAULT_FINDINGS);
    expect(lastCall.isSample).toBe(true);
    expect(lastCall.sourceUrl).toBeNull();
  });

  it("reports isSample: false and the real sourceUrl via onRunChange when run with a prSource", async () => {
    const onRunChange = vi.fn();
    render(
      <PrReviewDemoApp
        demoApp={DEMO_APP}
        session={fakeSession()}
        prSource={{ title: "Fix the thing", diff: "diff --git a/x b/x\n", htmlUrl: "https://github.com/acme/widgets/pull/42" }}
        onRunChange={onRunChange}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: /run review/i }));

    await waitFor(() => expect(screen.getByText("Looks good.")).toBeTruthy());
    const lastCall = onRunChange.mock.calls.at(-1)?.[0];
    expect(lastCall.isSample).toBe(false);
    expect(lastCall.sourceUrl).toBe("https://github.com/acme/widgets/pull/42");
  });

  it("restores a previously-completed review from savedRun instead of starting over", () => {
    const savedRun = {
      messages: [
        { id: "user-1", role: "user" as const, content: "Review this pull request..." },
        { id: "assistant-1", role: "assistant" as const, content: "Looks good." },
      ],
      diffTitle: "Add per-team spend aggregation endpoint",
      diffContent: "diff --git a/api/spend.py b/api/spend.py\n",
      isSample: true,
      sourceUrl: null,
      conversationId: "conv-1",
      reviewText: "Looks good.",
      reviewUsage: { input_tokens: 512, output_tokens: 128 },
      contextStats: null,
      reviewFindings: DEFAULT_FINDINGS,
      initialReviewMessageId: "assistant-1",
      selectedModelId: MODEL_OPTIONS[0].value,
      reviewPromptMessageId: "user-1",
      sendError: null,
    };
    render(<PrReviewDemoApp demoApp={DEMO_APP} session={fakeSession()} savedRun={savedRun} />);

    // The pre-review config panel/Run review button must NOT reappear --
    // this is what "navigate away and back" needs to avoid, since it would
    // otherwise look like the review never happened.
    expect(screen.queryByRole("button", { name: /run review/i })).toBeNull();
    expect(screen.getByText("Looks good.")).toBeTruthy();
    expect(mockInvokeDeployment).not.toHaveBeenCalled();
  });

  it("restores a real (non-sample) PR's badge and GitHub link from savedRun, without prSource -- regression test", () => {
    // prSource is BringYourOwnPrStage's own local state, which resets to
    // undefined on every remount (stage navigation) -- a restored run must
    // not fall back to looking like the bundled sample just because
    // prSource is absent this time. isSample/sourceUrl are what PrReviewRunState
    // carries specifically so this doesn't happen.
    const savedRun = {
      messages: [{ id: "assistant-1", role: "assistant" as const, content: "Looks good." }],
      diffTitle: "Fix the thing",
      diffContent: "diff --git a/x b/x\n",
      isSample: false,
      sourceUrl: "https://github.com/acme/widgets/pull/42",
      conversationId: "conv-1",
      reviewText: "Looks good.",
      reviewUsage: null,
      contextStats: null,
      reviewFindings: DEFAULT_FINDINGS,
      initialReviewMessageId: "assistant-1",
      selectedModelId: MODEL_OPTIONS[0].value,
      reviewPromptMessageId: null,
      sendError: null,
    };
    // No `prSource` prop passed -- exactly the remounted-after-navigation case.
    render(<PrReviewDemoApp demoApp={DEMO_APP} session={fakeSession()} savedRun={savedRun} />);

    expect(screen.queryByText("Sample PR")).toBeNull();
    const link = screen.getByRole("link", { name: /view on github/i }) as HTMLAnchorElement;
    expect(link.href).toBe("https://github.com/acme/widgets/pull/42");
  });
});
