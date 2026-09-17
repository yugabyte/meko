import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render as rtlRender, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { deleteMemory, keepAsMemory } from "@/lib/api/mekoApi";
import { postChat } from "@/lib/inferenceGateway";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import { ConversationStage } from "./ConversationStage";

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

describe("ConversationStage", () => {
  it("shows the degrade prompt when no datapack is selected", () => {
    render(<ConversationStage session={fakeSession({ datapackId: null })} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.getByText(/choose or create a datapack first/i)).toBeTruthy();
  });


  it("renders the chat and example prompts once a datapack is selected", () => {
    render(<ConversationStage session={fakeSession()} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.getByRole("textbox")).toBeTruthy();
    expect(
      screen.getByText("We always write a test before fixing a bug, not after."),
    ).toBeTruthy();
  });

  it("clicking an example prompt prefills the input without sending it", () => {
    render(<ConversationStage session={fakeSession()} onGoToCreateDatapack={vi.fn()} />);
    fireEvent.click(screen.getByText("We always write a test before fixing a bug, not after."));

    const textarea = screen.getByRole("textbox") as HTMLTextAreaElement;
    expect(textarea.value).toBe("We always write a test before fixing a bug, not after.");
    expect(mockPostChat).not.toHaveBeenCalled();
  });

  it("sending a chat message calls postChat with no persona, and bumps the refresh tick", async () => {
    const bumpRefreshTick = vi.fn();
    render(
      <ConversationStage session={fakeSession({ bumpRefreshTick })} onGoToCreateDatapack={vi.fn()} />,
    );

    const textarea = screen.getByRole("textbox");
    fireEvent.change(textarea, { target: { value: "We use trunk-based development." } });
    fireEvent.keyDown(textarea, { key: "Enter", code: "Enter" });

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

    const textarea = screen.getByRole("textbox");
    fireEvent.change(textarea, { target: { value: "We use trunk-based development." } });
    fireEvent.keyDown(textarea, { key: "Enter", code: "Enter" });

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
    fireEvent.change(screen.getByPlaceholderText(/ask anything|message|type/i), {
      target: { value: "Never concatenate raw input into a SQL query." },
    });
    fireEvent.submit(screen.getByRole("textbox").closest("form") as HTMLFormElement);
    return screen.findByText("Always parameterize SQL.");
  }

  it("shows the memory this conversation produced, with both decisions", async () => {
    await sendATurn();

    expect(screen.getByText("memory extracted")).toBeTruthy();
    expect(screen.getByRole("button", { name: /keep it/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /not useful/i })).toBeTruthy();
  });

  // The card has to stay put and say what happened: deleting the row removes
  // it from the listing, so reading straight from the query made the tile
  // vanish on click with no word on why.
  it("discards the memory on Not useful, and settles the card in place", async () => {
    await sendATurn();

    fireEvent.click(screen.getByRole("button", { name: /not useful/i }));

    await waitFor(() => expect(vi.mocked(deleteMemory)).toHaveBeenCalledWith("mem-1", "dp-1"));
    expect(vi.mocked(keepAsMemory)).not.toHaveBeenCalled();

    expect(await screen.findByText(/discarded\. it's out of the datapack/i)).toBeTruthy();
    expect(screen.getByText("Always parameterize SQL.")).toBeTruthy();
    // Declining must never read as though the memory was kept.
    expect(screen.queryByRole("button", { name: /^kept$/i })).toBeNull();
  });

  // Keep it is deliberately local. The obvious API, keep-as-memory, stamps a
  // one-way opt-OUT of promotion that PromoteMemoriesRepo then excludes -- so
  // affirming here would make Chapter 4 unable to promote the very memory
  // this card says Chapter 4 will promote.
  it("keeps the memory locally, touching no API that would block promotion", async () => {
    await sendATurn();

    fireEvent.click(screen.getByRole("button", { name: /keep it/i }));

    expect(await screen.findByText(/kept\. it's private to you/i)).toBeTruthy();
    expect(vi.mocked(keepAsMemory)).not.toHaveBeenCalled();
    expect(vi.mocked(deleteMemory)).not.toHaveBeenCalled();
  });
});
