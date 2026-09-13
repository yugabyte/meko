import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactElement } from "react";

import { ampListClusters } from "@/lib/api/ampApi";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import { SAMPLE_REVIEW_RUNS } from "../../data/adlcReviewAgentLab/sampleReviewRuns";
import { ReviewDashboardStage } from "./ReviewDashboardStage";

// recharts' ResponsiveContainer needs a real ResizeObserver, which jsdom
// doesn't provide -- a minimal no-op stub is enough since this test only
// checks the stat tiles/labeling around the chart, not its own layout.
beforeAll(() => {
  global.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  };
});

vi.mock("@/lib/api/ampApi", () => ({
  ampListClusters: vi.fn(),
}));

const mockListClusters = vi.mocked(ampListClusters);

function renderStage(ui: ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

function fakeSession(datapackId: string | null, ampClusterId: string | null): ReturnType<typeof useLabRunnerSession> {
  return {
    datapackId,
    ampClusterId,
    ampBranchId: ampClusterId ? "branch-1" : null,
    clearAmpCluster: vi.fn(),
  } as unknown as ReturnType<typeof useLabRunnerSession>;
}

afterEach(cleanup);
beforeEach(() => {
  vi.clearAllMocks();
  mockListClusters.mockResolvedValue([]);
});

describe("ReviewDashboardStage", () => {
  it("shows the degrade prompt when no datapack is selected", () => {
    renderStage(
      <ReviewDashboardStage
        session={fakeSession(null, null)}
        onGoToCreateDatapack={vi.fn()}
        onGoToCreateAmpCluster={vi.fn()}
      />,
    );
    expect(screen.getByText(/choose or create a datapack first/i)).toBeTruthy();
  });

  it("shows the degrade prompt when no AMP cluster is selected", () => {
    renderStage(
      <ReviewDashboardStage
        session={fakeSession("dp-1", null)}
        onGoToCreateDatapack={vi.fn()}
        onGoToCreateAmpCluster={vi.fn()}
      />,
    );
    expect(screen.getByText(/create an amp cluster first/i)).toBeTruthy();
  });

  it("shows stat tiles and marks the data as sample, not real, once the cluster is confirmed live", async () => {
    mockListClusters.mockResolvedValue([
      { id: "cluster-1", name: "review-metrics", storage_pool_id: "pool-1", default_branch: { branch_id: "branch-1", cluster_id: "cluster-1", name: "main", pg_version: 16, is_default: true, created_at: "" }, created_at: "" },
    ]);
    renderStage(
      <ReviewDashboardStage
        session={fakeSession("dp-1", "cluster-1")}
        onGoToCreateDatapack={vi.fn()}
        onGoToCreateAmpCluster={vi.fn()}
      />,
    );
    expect(screen.getByText(/sample data/i)).toBeTruthy();
    await waitFor(() => expect(screen.getByText(String(SAMPLE_REVIEW_RUNS.length))).toBeTruthy());
  });

  it("clears a stale cluster id (deleted server-side) once the live list resolves without it", async () => {
    mockListClusters.mockResolvedValue([]);
    const session = fakeSession("dp-1", "deleted-cluster");
    renderStage(
      <ReviewDashboardStage
        session={session}
        onGoToCreateDatapack={vi.fn()}
        onGoToCreateAmpCluster={vi.fn()}
      />,
    );
    await waitFor(() => expect(session.clearAmpCluster).toHaveBeenCalled());
  });
});
