// Promotion is the one action in the lab that changes what other people's
// agents read, so what is pinned here is that it only happens on the explicit
// button, and that declining it stays local.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { promoteMemories, type Memory } from "@/lib/api/mekoApi";
import type { LabContextFeed } from "../../hooks/useLabContextFeed";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import { PromoteLearningStage } from "./PromoteLearningStage";

vi.mock("@/lib/api/mekoApi", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api/mekoApi")>();
  return { ...actual, promoteMemories: vi
      .fn()
      .mockResolvedValue({ inserted_ids: ["mem-1"], updated_ids: [], not_found_ids: [] }) };
});

const mockPromote = vi.mocked(promoteMemories);

afterEach(() => {
  cleanup();
  mockPromote.mockClear();
});

const MEMORY: Memory = { id: "mem-1", data: "Always parameterize SQL.", user_id: "user-1" };

function feedWith(memories: Memory[]): LabContextFeed {
  return {
    knowledge: [],
    knowledgeTotal: 0,
    memories,
    memoriesTotal: memories.length,
    collective: [],
    collectiveTotal: 0,
    conversationsTotal: 0,
      chunksTotal: null,
    chunkSize: null,
    loading: false,
  };
}

function fakeSession() {
  return {
    userId: "user-1",
    datapackId: "dp-1",
    bumpRefreshTick: vi.fn(),
  } as unknown as ReturnType<typeof useLabRunnerSession>;
}

function renderStage(memories: Memory[] = [MEMORY]) {
  const session = fakeSession();
  render(
    <PromoteLearningStage
      session={session}
      feed={feedWith(memories)}
      onGoToCreateDatapack={vi.fn()}
    />,
  );
  return { session };
}

describe("PromoteLearningStage", () => {
  it("promotes only the memory whose button was pressed", async () => {
    renderStage([MEMORY, { id: "mem-2", data: "Write the test first." }]);

    fireEvent.click(screen.getAllByRole("button", { name: /promote to collective memory/i })[0]);

    await waitFor(() => expect(mockPromote).toHaveBeenCalledTimes(1));
    expect(mockPromote).toHaveBeenCalledWith({
      user_id: "user-1",
      datapack_id: "dp-1",
      memory_ids: ["mem-1"],
    });
  });

  it("says what promotion changed, once it has actually landed", async () => {
    renderStage();
    expect(screen.queryByText(/every agent on this datapack/i)).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: /promote to collective memory/i }));

    expect(await screen.findByText(/every agent on this datapack/i)).toBeTruthy();
  });

  // Declining has to be a real answer, not a dead button — and it must not
  // reach the API, since nothing outside this datapack should change.
  it("keeps a memory private without calling promote, and never labels it promoted", () => {
    renderStage();

    fireEvent.click(screen.getByRole("button", { name: /keep private/i }));

    expect(mockPromote).not.toHaveBeenCalled();
    expect(screen.getByText(/left private/i)).toBeTruthy();
    // Regression: declining drove the same state as promoting, so the
    // primary button read "Promoted" for a memory nobody promoted.
    expect(screen.queryByRole("button", { name: /^promoted$/i })).toBeNull();
    expect(screen.getByRole("button", { name: /promote to collective memory/i })).toBeTruthy();
  });

  // Regression: every card read the same record key, so one decision settled
  // all of them.
  it("settles only the card whose button was pressed", () => {
    renderStage([MEMORY, { id: "mem-2", data: "Write the test first." }]);

    fireEvent.click(screen.getAllByRole("button", { name: /keep private/i })[0]);

    expect(screen.getAllByText(/left private/i)).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: /promote to collective memory/i })).toHaveLength(2);
  });

  // A 200 with nothing inserted means the row was skipped -- another
  // member's, or stamped keep_as_memory. Claiming success there would tell
  // the user their team can read something no agent will ever retrieve.
  it("refuses to claim success when nothing was inserted", async () => {
    mockPromote.mockResolvedValueOnce({ inserted_ids: [], updated_ids: [], not_found_ids: ["mem-1"] });
    renderStage();

    fireEvent.click(screen.getByRole("button", { name: /promote to collective memory/i }));

    expect(await screen.findByText(/could not be promoted/i)).toBeTruthy();
    expect(screen.queryByText(/every agent on this datapack/i)).toBeNull();
  });

  it("explains the empty case instead of rendering an empty step", () => {
    renderStage([]);
    expect(screen.getByText(/no private memories on this datapack yet/i)).toBeTruthy();
  });
});
