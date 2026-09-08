import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { invokeDeployment, invokeSharedDeployment, type ReviewStreamEvent } from "@/lib/api/deploymentInvokeApi";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDemoAppConfig } from "../../types";
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

const DEFAULT_STREAM: ReviewStreamEvent[] = [
  { type: "meta", conversation_id: "conv-1" },
  { type: "text_delta", text: "Looks good." },
  { type: "done", review: "Looks good." },
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
  it("defaults both switches on and sends them with the sample diff run", async () => {
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
  });

  it("forwards a disabled switch and a focus note to invokeDeployment", async () => {
    render(<PrReviewDemoApp demoApp={DEMO_APP} session={fakeSession()} />);

    // First switch rendered is "Use org knowledge base" (before "Use my
    // memories") — queried by role rather than label text since the Label
    // wraps both a title and a description span, so its accessible name
    // isn't just the title alone.
    fireEvent.click(screen.getAllByRole("switch")[0]);
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

  it("streams the review text incrementally into the assistant bubble", async () => {
    mockInvokeDeployment.mockReturnValue(
      fakeStream([
        { type: "meta", conversation_id: "conv-1" },
        { type: "text_delta", text: "Looks " },
        { type: "text_delta", text: "good." },
        { type: "done", review: "Looks good." },
      ]),
    );
    render(<PrReviewDemoApp demoApp={DEMO_APP} session={fakeSession()} />);

    fireEvent.click(screen.getByRole("button", { name: /run review/i }));

    await waitFor(() => expect(screen.getByText("Looks good.")).toBeTruthy());
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
});
