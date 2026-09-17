// The two pieces of logic here exist because two services disagree about a
// conversation id, and neither disagreement is visible from a type. Both are
// pinned with the formats they actually produce.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { listMemories, type Memory } from "@/lib/api/mekoApi";
import { useExtractedMemories } from "./useExtractedMemories";

vi.mock("@/lib/api/mekoApi", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api/mekoApi")>();
  return { ...actual, listMemories: vi.fn() };
});

const mockList = vi.mocked(listMemories);

afterEach(() => {
  cleanup();
  mockList.mockReset();
});

/** The gateway's form: a Langfuse trace id with the hyphens stripped. */
const TURN_ID = "1f319cf4aeae48af9d3c3c357056905f";
/** What mem0 stored for the same conversation. */
const HYPHENATED = "1f319cf4-aeae-48af-9d3c-3c357056905f";

function listing(memories: Partial<Memory>[]) {
  mockList.mockResolvedValue({
    memories: memories as Memory[],
    total: memories.length,
    criteria: {},
    top_k: memories.length,
    page: 1,
    page_size: 50,
    total_pages: 1,
  });
}

function wrapper({ children }: { children: ReactNode }) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

function render(startedAt: number | null = Date.now()) {
  return renderHook(
    () => useExtractedMemories({ datapackId: "dp-1", conversationId: TURN_ID, startedAt }),
    { wrapper },
  );
}

describe("useExtractedMemories", () => {
  // The bug this hook shipped with: the ids are the same, written differently.
  it("matches a hyphenated conversation id against the gateway's stripped one", async () => {
    listing([{ id: "m1", data: "kept", conversation_id: HYPHENATED }]);
    const { result } = render();
    await waitFor(() => expect(result.current.memories).toHaveLength(1));
    expect(result.current.memories[0].id).toBe("m1");
  });

  it("ignores a different conversation's memories", async () => {
    listing([{ id: "m1", data: "someone else's", conversation_id: "0000" }]);
    const { result } = render();
    await waitFor(() => expect(mockList).toHaveBeenCalled());
    expect(result.current.memories).toHaveLength(0);
  });

  // Falls back on time, not on the conversation, so a datapack whose rows
  // carry no conversation id still shows the lesson its own memory.
  it("falls back to rows written since the turn when none carry a conversation id", async () => {
    const startedAt = Date.parse("2026-09-17T10:00:00Z");
    listing([
      { id: "old", data: "from last week", created_at: "2026-09-10T10:00:00Z" },
      { id: "new", data: "from this turn", created_at: "2026-09-17T10:00:05Z" },
    ]);
    const { result } = render(startedAt);
    await waitFor(() => expect(result.current.memories).toHaveLength(1));
    expect(result.current.memories[0].id).toBe("new");
  });

  it("prefers a conversation match over the time fallback", async () => {
    const startedAt = Date.parse("2026-09-17T10:00:00Z");
    listing([
      { id: "matched", data: "this turn", conversation_id: HYPHENATED },
      { id: "recent", data: "also recent", created_at: "2026-09-17T10:00:05Z" },
    ]);
    const { result } = render(startedAt);
    await waitFor(() => expect(result.current.memories).toHaveLength(1));
    expect(result.current.memories[0].id).toBe("matched");
  });

  it("stops waiting past the deadline so a datapack with extraction off settles", async () => {
    listing([]);
    const { result } = render(Date.now() - 60_000);
    await waitFor(() => expect(mockList).toHaveBeenCalled());
    await waitFor(() => expect(result.current.waiting).toBe(false));
  });

  it("waits while nothing has arrived and the deadline is ahead", async () => {
    listing([]);
    const { result } = render();
    await waitFor(() => expect(mockList).toHaveBeenCalled());
    expect(result.current.waiting).toBe(true);
  });
});
