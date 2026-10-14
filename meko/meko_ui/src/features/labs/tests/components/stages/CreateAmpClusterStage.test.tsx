import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactElement } from "react";

import { TooltipProvider } from "@/components/ui/tooltip";
import {
  ampCoreBudget,
  ampCoreLimitMessage,
  ampCreateCluster,
  ampListClusters,
} from "@/lib/api/ampApi";
import type { useLabRunnerSession } from "@/features/labs/hooks/useLabRunnerSession";
import { CreateAmpClusterStage } from "@/features/labs/components/stages/CreateAmpClusterStage";

vi.mock("@/lib/api/ampApi", () => ({
  ampListClusters: vi.fn(),
  ampCreateCluster: vi.fn(),
  ampCoreLimitMessage: vi.fn().mockReturnValue(null),
  ampCoreBudget: vi.fn(),
}));
vi.mock("@/lib/amp", () => ({ isAmpEnabled: () => true }));

const mockListClusters = vi.mocked(ampListClusters);
const mockCreateCluster = vi.mocked(ampCreateCluster);
const mockCoreLimitMessage = vi.mocked(ampCoreLimitMessage);
const mockCoreBudget = vi.mocked(ampCoreBudget);

function renderStage(ui: ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // TooltipProvider mirrors App.tsx's own ambient wrapping -- the atLimit
  // gate below uses a bare Tooltip, which throws without one.
  return render(
    <QueryClientProvider client={qc}>
      <TooltipProvider>{ui}</TooltipProvider>
    </QueryClientProvider>,
  );
}

function fakeSession(overrides: Partial<ReturnType<typeof useLabRunnerSession>> = {}) {
  return {
    datapackId: "dp-1",
    ampClusterId: null,
    ampBranchId: null,
    selectAmpCluster: vi.fn(),
    clearAmpCluster: vi.fn(),
    ...overrides,
  } as unknown as ReturnType<typeof useLabRunnerSession>;
}

afterEach(cleanup);
beforeEach(() => {
  vi.clearAllMocks();
  mockListClusters.mockResolvedValue([]);
  mockCoreLimitMessage.mockReturnValue(null);
  mockCoreBudget.mockResolvedValue({
    tier: "free",
    next_tier: "standard",
    used_cores: 0,
    max_cores: 500,
    at_limit: false,
  });
});

describe("CreateAmpClusterStage", () => {
  it("shows the degrade prompt when no datapack is selected", () => {
    renderStage(
      <CreateAmpClusterStage
        session={fakeSession({ datapackId: null })}
        onGoToCreateDatapack={vi.fn()}
      />,
    );
    expect(screen.getByText(/choose or create a datapack first/i)).toBeTruthy();
  });

  it("creates a cluster with the default (free-tier-safe) cpu budget, and selects it", async () => {
    mockCreateCluster.mockResolvedValue({
      id: "cluster-1",
      name: "review-metrics",
      storage_pool_id: "pool-1",
      default_branch: {
        branch_id: "branch-1",
        cluster_id: "cluster-1",
        name: "main",
        pg_version: 16,
        is_default: true,
        created_at: "",
      },
      created_at: "",
    });
    const session = fakeSession();
    renderStage(<CreateAmpClusterStage session={session} onGoToCreateDatapack={vi.fn()} />);

    await waitFor(() => expect(screen.getByLabelText("Cluster name")).toBeTruthy());
    fireEvent.change(screen.getByLabelText("Cluster name"), {
      target: { value: "review-metrics" },
    });
    fireEvent.click(screen.getByRole("button", { name: /create cluster/i }));

    await waitFor(() =>
      expect(session.selectAmpCluster).toHaveBeenCalledWith("cluster-1", "branch-1"),
    );
    expect(mockCreateCluster).toHaveBeenCalledWith("dp-1", { name: "review-metrics", cpu: "0.5" });
  });

  it("shows the upgrade notice instead of a raw error when creation hits the per-tier vCPU cap", async () => {
    mockCreateCluster.mockRejectedValue(new Error("402"));
    mockCoreLimitMessage.mockReturnValue("Upgrade to Standard to use more vCPU.");
    renderStage(<CreateAmpClusterStage session={fakeSession()} onGoToCreateDatapack={vi.fn()} />);

    await waitFor(() => expect(screen.getByLabelText("Cluster name")).toBeTruthy());
    fireEvent.change(screen.getByLabelText("Cluster name"), {
      target: { value: "review-metrics" },
    });
    fireEvent.click(screen.getByRole("button", { name: /create cluster/i }));

    // The shared tier-limit banner replaces the raw error: its composed copy
    // names the AMP compute subject and carries an upgrade CTA.
    expect(await screen.findByText(/AMP compute \(vCPU\)/i)).toBeTruthy();
    expect(screen.getByRole("link", { name: /talk to our team/i })).toBeTruthy();
  });

  it("shows a ready message once a cluster is selected", () => {
    renderStage(
      <CreateAmpClusterStage
        session={fakeSession({ ampClusterId: "cluster-1", ampBranchId: "branch-1" })}
        onGoToCreateDatapack={vi.fn()}
      />,
    );
    expect(screen.getByText(/ready/i)).toBeTruthy();
  });

  it("clears a stale cluster id (deleted server-side) once the live list resolves without it", async () => {
    mockListClusters.mockResolvedValue([]);
    const session = fakeSession({ ampClusterId: "deleted-cluster", ampBranchId: "branch-1" });
    renderStage(<CreateAmpClusterStage session={session} onGoToCreateDatapack={vi.fn()} />);

    await waitFor(() => expect(session.clearAmpCluster).toHaveBeenCalled());
  });

  it("greys the create button (with an upgrade tooltip) when already at the per-tier vCPU cap", async () => {
    mockCoreBudget.mockResolvedValue({
      tier: "free",
      next_tier: "standard",
      used_cores: 500,
      max_cores: 500,
      at_limit: true,
    });
    renderStage(<CreateAmpClusterStage session={fakeSession()} onGoToCreateDatapack={vi.fn()} />);

    // Same "grey the button, don't wait for a click" gate DatapackClustersPanel.tsx
    // uses -- the tooltip's own content is a Radix portal (hover-triggered),
    // not asserted here; the disabled state is the load-bearing behavior.
    await waitFor(() =>
      expect(
        (screen.getByRole("button", { name: /create cluster/i }) as HTMLButtonElement).disabled,
      ).toBe(true),
    );
  });
});
