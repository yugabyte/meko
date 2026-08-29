import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { postChat } from "@/lib/inferenceGateway";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import { SeedContextStage } from "./SeedContextStage";

// Scope this test to SeedContextStage's own composition, not FileUpload's
// internals (which have no existing tests of their own to lean on either).
vi.mock("@/features/datapacks/components/FileUpload", () => ({
  default: () => <div data-testid="file-upload-mock" />,
}));
vi.mock("@/lib/inferenceGateway", () => ({
  postChat: vi.fn().mockResolvedValue({
    message: "Got it, I'll remember that.",
    conversation_id: "conv-1",
    message_id: "m1",
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
    ...overrides,
  } as ReturnType<typeof useLabRunnerSession>;
}

afterEach(cleanup);
beforeEach(() => vi.clearAllMocks());

describe("SeedContextStage", () => {
  it("shows the degrade prompt when no datapack is selected", () => {
    render(<SeedContextStage session={fakeSession({ datapackId: null })} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.getByText(/choose or create a datapack first/i)).toBeTruthy();
    expect(screen.queryByTestId("file-upload-mock")).toBeNull();
  });

  it("renders the upload widget and the chat once a datapack is selected", () => {
    render(<SeedContextStage session={fakeSession()} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.getByTestId("file-upload-mock")).toBeTruthy();
    expect(screen.getByText(/tell meko what to remember/i)).toBeTruthy();
  });

  it("sending a chat message calls postChat with no persona, and bumps the refresh tick", async () => {
    const bumpRefreshTick = vi.fn();
    render(
      <SeedContextStage session={fakeSession({ bumpRefreshTick })} onGoToCreateDatapack={vi.fn()} />,
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
});
