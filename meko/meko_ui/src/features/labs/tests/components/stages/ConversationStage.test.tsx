import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render as rtlRender, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { deleteMemory, keepAsMemory } from "@/lib/api/mekoApi";
import { postChat } from "@/lib/inferenceGateway";
import type { useLabRunnerSession } from "@/features/labs/hooks/useLabRunnerSession";
import { ConversationStage } from "@/features/labs/components/stages/ConversationStage";

// The extracted-memory tiles poll GET /memories; the decisions they offer
// are real API calls, so both are mocked.
vi.mock("@/lib/api/mekoApi", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api/mekoApi")>();
  return {
    ...actual,
    listMemories: vi.fn().mockResolvedValue({
      memories: [{ id: "mem-1", data: "Always parameterize SQL.", conversation_id: "conv-1" }],
      total: 1,
      criteria: {},
      top_k: 1,
      page: 1,
      page_size: 50,
      total_pages: 1,
    }),
    deleteMemory: vi.fn().mockResolvedValue({ id: "mem-1", data: "" }),
    keepAsMemory: vi.fn().mockResolvedValue({ updated: 1 }),
  };
});

vi.mock("@/lib/inferenceGateway", () => ({
  postChat: vi.fn().mockResolvedValue({
    message: "Got it, I'll remember that.",
    conversation_id: "conv-1",
    message_id: "m1",
    processing_steps: [
      { name: "memory_search", duration_ms: 80, status: "ok" },
      { name: "conversation_add_message", duration_ms: 30, status: "ok" },
    ],
  }),
  getInferenceGatewayErrorMessage: (err: unknown) => (err instanceof Error ? err.message : "error"),
}));

const mockPostChat = vi.mocked(postChat);

// The stage polls GET /memories for what the turn taught Meko, so it needs a
// client. Retries off so a failing fetch surfaces immediately instead of
// holding the test open.
function render(ui: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return rtlRender(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

function fakeSession(overrides: Partial<ReturnType<typeof useLabRunnerSession>> = {}) {
  return {
    userId: "user-1",
    currentUser: null,
    datapackId: "dp-1",
    datapackName: "test-datapack",
    accessibleDatapacks: [],
    datapacksLoading: false,
    selectDatapack: vi.fn(),
    createAndSelectDatapack: vi.fn(),
    creating: false,
    userToken: "mko_tkn_fake",
    loading: false,
    error: null,
    refreshTick: 0,
    bumpRefreshTick: vi.fn(),
    lastLessonTrace: null,
    recordLessonTrace: vi.fn(),
    ...overrides,
  } as ReturnType<typeof useLabRunnerSession>;
}

afterEach(cleanup);
beforeEach(() => vi.clearAllMocks());

/** The sample the lesson ships — clicking it is the only way to send, so
 *  every test that needs a turn goes through it. */
const SAMPLE = "We always write a test before fixing a bug, not after.";

describe("ConversationStage", () => {
  it("shows the degrade prompt when no datapack is selected", () => {
    render(<ConversationStage session={fakeSession({ datapackId: null })} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.getByText(/choose or create a datapack first/i)).toBeTruthy();
  });


  // The lesson is what Meko does with a conversation, not what you can type
  // into one: a free-text box invites a sentence that extracts nothing, and
  // the lesson dead-ends with no memory to show.
  it("offers samples to send and no free-text box", () => {
    render(<ConversationStage session={fakeSession()} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.getByText(SAMPLE)).toBeTruthy();
  });

  it("clicking a sample sends it straight away", async () => {
    render(<ConversationStage session={fakeSession()} onGoToCreateDatapack={vi.fn()} />);
    fireEvent.click(screen.getByText(SAMPLE));

    await waitFor(() => expect(mockPostChat).toHaveBeenCalledTimes(1));
    expect(mockPostChat.mock.calls[0][0].message).toBe(SAMPLE);
  });

  it("sending a chat message calls postChat with no persona, and bumps the refresh tick", async () => {
    const bumpRefreshTick = vi.fn();
    render(
      <ConversationStage session={fakeSession({ bumpRefreshTick })} onGoToCreateDatapack={vi.fn()} />,
    );

    fireEvent.click(screen.getByText(SAMPLE));

    await waitFor(() => expect(mockPostChat).toHaveBeenCalledTimes(1));
    const call = mockPostChat.mock.calls[0][0];
    expect(call.datapack_id).toBe("dp-1");
    expect(call.user_id).toBe("user-1");
    expect(call.user_token).toBe("mko_tkn_fake");
    expect(call.persona).toBeUndefined();
    expect(bumpRefreshTick).toHaveBeenCalled();
  });

  it("records the turn's real MCP tool calls for X-Ray", async () => {
    const recordLessonTrace = vi.fn();
    render(
      <ConversationStage session={fakeSession({ recordLessonTrace })} onGoToCreateDatapack={vi.fn()} />,
    );

    fireEvent.click(screen.getByText(SAMPLE));

    await waitFor(() => expect(mockPostChat).toHaveBeenCalledTimes(1));
    expect(recordLessonTrace).toHaveBeenCalledWith({
      lessonTitle: "Chat about a preference",
      calls: [
        { name: "memory_search", durationMs: 80, status: "ok", detail: undefined },
        { name: "conversation_add_message", durationMs: 30, status: "ok", detail: undefined },
      ],
    });
  });
});


// What the lesson claims -- "Meko reads the conversation and writes the memory
// itself" -- is only visible if the extracted memory reaches the screen.
describe("ConversationStage extracted memories", () => {
  async function sendATurn() {
    render(<ConversationStage session={fakeSession()} onGoToCreateDatapack={vi.fn()} />);
    fireEvent.click(screen.getByText(SAMPLE));
    return screen.findByText("Always parameterize SQL.");
  }

  it("shows the memory this conversation produced", async () => {
    await sendATurn();
    expect(screen.getByText(/memory extracted/i)).toBeTruthy();
  });

  // Shown, not put to a vote. Keep/discard turned this into a triage screen,
  // when the point is simply that Meko wrote a memory nobody asked for —
  // Chapter 4 is where a memory gets a human decision.
  it("asks for no decision, and touches neither the delete nor the keep API", async () => {
    await sendATurn();

    expect(screen.queryByRole("button", { name: /keep it/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /not useful/i })).toBeNull();
    expect(vi.mocked(deleteMemory)).not.toHaveBeenCalled();
    expect(vi.mocked(keepAsMemory)).not.toHaveBeenCalled();
  });

  it("lands the lesson's point once a memory exists", async () => {
    await sendATurn();
    expect(
      await screen.findByText(/the trace remembers which sentence produced it/i),
    ).toBeTruthy();
  });
});
