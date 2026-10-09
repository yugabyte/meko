import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactElement } from "react";

import { ampListClusters } from "@/lib/api/ampApi";
import { listDatapackConversations, listMemories } from "@/lib/api/mekoApi";
import type { useLabRunnerSession } from "@/features/labs/hooks/useLabRunnerSession";
import type { PrReviewRunState } from "@/features/labs/components/demo-apps/PrReviewDemoApp";
import { MODEL_OPTIONS } from "@/features/labs/components/demo-apps/ReviewAgentConfigPanel";
import { ReviewDashboardStage } from "@/features/labs/components/stages/ReviewDashboardStage";

// recharts' ResponsiveContainer needs a real ResizeObserver, which jsdom
// doesn't provide -- a no-op stub is enough since these tests check the
// numbers around the chart, not its layout.
beforeAll(() => {
  global.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  };
});

vi.mock("@/lib/api/ampApi", () => ({ ampListClusters: vi.fn() }));
vi.mock("@/lib/api/mekoApi", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api/mekoApi")>()),
  listDatapackConversations: vi.fn(),
  listMemories: vi.fn(),
  getDatapackTokenSavings: vi.fn(),
}));

const mockListClusters = vi.mocked(ampListClusters);
const mockListConversations = vi.mocked(listDatapackConversations);
const mockListMemories = vi.mocked(listMemories);

function renderStage(ui: ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

function fakeSession(datapackId: string | null) {
  return { datapackId, datapackName: "my-pack" } as unknown as ReturnType<
    typeof useLabRunnerSession
  >;
}

function completedRun(): PrReviewRunState {
  return {
    messages: [],
    diffTitle: null,
    diffContent: null,
    isSample: true,
    sourceUrl: null,
    conversationId: "conv-1",
    reviewText: "Looks good.",
    reviewUsage: { input_tokens: 512, output_tokens: 128 },
    contextStats: null,
    reviewFindings: [],
    initialReviewMessageId: null,
    selectedModelId: MODEL_OPTIONS[0].value,
    reviewPromptMessageId: null,
    sendError: null,
    durationMs: null,
  };
}

afterEach(cleanup);
beforeEach(() => {
  vi.clearAllMocks();
  mockListClusters.mockResolvedValue([]);
  mockListMemories.mockResolvedValue({ memories: [], total: 0 } as never);
  mockListConversations.mockResolvedValue({
    datapack_id: "dp-1",
    datapack_name: "my-pack",
    limit: 50,
    offset: 0,
    has_more: false,
    conversations: [],
  } as never);
});

describe("ReviewDashboardStage", () => {
  it("shows the degrade prompt when no datapack is selected", () => {
    renderStage(
      <ReviewDashboardStage
        session={fakeSession(null)}
        sessionRuns={[]}
        onGoToCreateDatapack={vi.fn()}
      />,
    );
    expect(screen.getByText(/choose or create a datapack first/i)).toBeTruthy();
  });

  // The dashboard used to sum each conversation's `memories_added`, which
  // ignores a fact saved straight through /memory/add -- so it read 0 while
  // the context pane beside it read 7.
  it("counts memories from the datapack's memory list, not from conversation attribution", async () => {
    mockListMemories.mockResolvedValue({ memories: [], total: 7 } as never);
    mockListConversations.mockResolvedValue({
      datapack_id: "dp-1",
      datapack_name: "my-pack",
      limit: 50,
      offset: 0,
      has_more: false,
      conversations: [
        { id: "c1", started_at: "2026-09-10T10:00:00Z", memories_added: 2, llm_calls: 4 },
        { id: "c2", started_at: "2026-09-11T10:00:00Z", memories_added: 3, llm_calls: 1 },
      ],
    } as never);

    renderStage(
      <ReviewDashboardStage
        session={fakeSession("dp-1")}
        sessionRuns={[]}
        onGoToCreateDatapack={vi.fn()}
      />,
    );

    await waitFor(() => expect(screen.getByText("2")).toBeTruthy());
    expect(screen.getByText("7")).toBeTruthy();
  });

  it("says the per-run table is empty rather than inventing rows for it", async () => {
    renderStage(
      <ReviewDashboardStage
        session={fakeSession("dp-1")}
        sessionRuns={[]}
        onGoToCreateDatapack={vi.fn()}
      />,
    );
    await waitFor(() =>
      expect(screen.getByText(/no reviews run in this session yet/i)).toBeTruthy(),
    );
  });

  it("shows this session's own runs with their real token counts", async () => {
    renderStage(
      <ReviewDashboardStage
        session={fakeSession("dp-1")}
        sessionRuns={[completedRun()]}
        onGoToCreateDatapack={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.getByText("512")).toBeTruthy());
    expect(screen.getByText("128")).toBeTruthy();
    expect(screen.getByText(new RegExp(MODEL_OPTIONS[0].label))).toBeTruthy();
  });
});
