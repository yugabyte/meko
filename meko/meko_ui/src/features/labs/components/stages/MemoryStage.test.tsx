import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { addMemory } from "@/lib/inferenceGateway";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import { MemoryStage } from "./MemoryStage";

vi.mock("@/lib/inferenceGateway", () => ({
  addMemory: vi.fn().mockResolvedValue({ ok: true, memory_id: "mem-1" }),
  getInferenceGatewayErrorMessage: (err: unknown, fallback: string) =>
    err instanceof Error ? err.message : fallback,
}));

const mockAddMemory = vi.mocked(addMemory);

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

describe("MemoryStage", () => {
  it("shows the degrade prompt when no datapack is selected", () => {
    render(<MemoryStage session={fakeSession({ datapackId: null })} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.getByText(/choose or create a datapack first/i)).toBeTruthy();
  });

  it("shows the context source tracker with Knowledge Base marked done", () => {
    const { container } = render(<MemoryStage session={fakeSession()} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.getByText("Memory")).toBeTruthy();
    // Knowledge Base precedes Memory in the fixed lesson order -- one done.
    expect(container.querySelectorAll("svg.lucide-check")).toHaveLength(1);
  });

  it("Save memory is disabled until text is entered", () => {
    render(<MemoryStage session={fakeSession()} onGoToCreateDatapack={vi.fn()} />);
    const saveButton = screen.getByRole("button", { name: "Save memory" }) as HTMLButtonElement;
    expect(saveButton.disabled).toBe(true);

    fireEvent.change(screen.getByPlaceholderText(/squash-merge/i), {
      target: { value: "We prefer explicit typing." },
    });
    expect(saveButton.disabled).toBe(false);
  });

  it("clicking an example preference fills the textarea", () => {
    render(<MemoryStage session={fakeSession()} onGoToCreateDatapack={vi.fn()} />);
    fireEvent.click(screen.getByText("Prefer composition over inheritance in new services."));

    const textarea = screen.getByPlaceholderText(/squash-merge/i) as HTMLTextAreaElement;
    expect(textarea.value).toBe("Prefer composition over inheritance in new services.");
  });

  it("Save memory calls addMemory with the right args, shows Done, and bumps the refresh tick", async () => {
    const bumpRefreshTick = vi.fn();
    const recordLessonTrace = vi.fn();
    render(
      <MemoryStage
        session={fakeSession({ bumpRefreshTick, recordLessonTrace })}
        onGoToCreateDatapack={vi.fn()}
      />,
    );

    fireEvent.change(screen.getByPlaceholderText(/squash-merge/i), {
      target: { value: "We prefer explicit typing." },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save memory" }));

    await waitFor(() => expect(mockAddMemory).toHaveBeenCalledTimes(1));
    expect(mockAddMemory).toHaveBeenCalledWith({
      text: "We prefer explicit typing.",
      user_token: "mko_tkn_fake",
      datapack_id: "dp-1",
    });
    expect(await screen.findByText(/check private memory/i)).toBeTruthy();
    expect(bumpRefreshTick).toHaveBeenCalled();

    // X-Ray shows the real MCP tool name, not the REST route.
    expect(recordLessonTrace).toHaveBeenCalledWith({
      lessonTitle: "Add a memory",
      calls: [expect.objectContaining({ name: "memory_add", status: "ok" })],
    });
  });

  it("shows an error message when addMemory fails, and still records a (failed) X-Ray call", async () => {
    mockAddMemory.mockRejectedValueOnce(new Error("quota exceeded"));
    const recordLessonTrace = vi.fn();
    render(<MemoryStage session={fakeSession({ recordLessonTrace })} onGoToCreateDatapack={vi.fn()} />);

    fireEvent.change(screen.getByPlaceholderText(/squash-merge/i), { target: { value: "hi" } });
    fireEvent.click(screen.getByRole("button", { name: "Save memory" }));

    expect(await screen.findByText("quota exceeded")).toBeTruthy();
    expect(recordLessonTrace).toHaveBeenCalledWith({
      lessonTitle: "Add a memory",
      calls: [expect.objectContaining({ name: "memory_add", status: "error", detail: "quota exceeded" })],
    });
  });
});
