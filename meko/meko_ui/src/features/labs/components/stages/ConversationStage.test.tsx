import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { postChat } from "@/lib/inferenceGateway";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import { ConversationStage } from "./ConversationStage";

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

  it("shows the context source tracker with Knowledge Base and Memory marked done", () => {
    const { container } = render(<ConversationStage session={fakeSession()} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.getByText("Conversation")).toBeTruthy();
    // Knowledge Base and Memory both precede Conversation -- two done.
    expect(container.querySelectorAll("svg.lucide-check")).toHaveLength(2);
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
