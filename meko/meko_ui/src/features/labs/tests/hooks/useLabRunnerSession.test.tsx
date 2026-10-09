import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

import type { LabDefinition } from "@/features/labs/types";
import { useLabRunnerSession } from "@/features/labs/hooks/useLabRunnerSession";

const mockCreateDatapack = vi.fn();
const mockGetDatapackProvisionStatus = vi.fn();
vi.mock("@/lib/api/mekoApi", () => ({
  createDatapack: (...args: unknown[]) => mockCreateDatapack(...args),
  getDatapackProvisionStatus: (...args: unknown[]) => mockGetDatapackProvisionStatus(...args),
}));

vi.mock("@/hooks/useCurrentUser", () => ({
  useCurrentUser: () => ({
    data: { account_id: "user-1", email: "me@example.com", user_type: "free" },
  }),
}));

vi.mock("@/hooks/useAccountUsage", () => ({
  useAccountUsage: () => ({ data: undefined }),
  ACCOUNT_USAGE_QUERY_KEY: "account-usage",
}));

vi.mock("@/lib/freeTier", () => ({
  blockedMessage: () => "You've hit your datapack limit.",
  getFreeTierStatus: () => ({ hasHitDatapackCount: false }),
}));

vi.mock("@/features/observe/observeRefresh", () => ({
  notifyDatapacksListChanged: vi.fn(),
}));

type MockDeployment = { id: string; status: string } | undefined;
let mockDedicatedDeployment: MockDeployment;
let mockSharedDeployment: MockDeployment;
vi.mock("@/features/labs/hooks/useDeployments", () => ({
  useDeploymentStatus: () => ({ data: mockDedicatedDeployment }),
  useSharedDeploymentStatus: () => ({ data: mockSharedDeployment }),
}));

type MockDatapack = { datapack_id: string; datapack_name: string; grant: string };
let mockDatapacks: MockDatapack[] = [];
vi.mock("@/features/datapacks/hooks/useDatapacks", () => ({
  DATAPACKS_QUERY_KEY: "datapacks",
  useDatapacks: () => ({ data: mockDatapacks, isLoading: false }),
}));

const FIXTURE_LAB = {
  id: "test-lab",
  demoApp: { deployable: { agentSlug: "adlc-review-agent", sourceRepoUrl: "https://example.com" } },
} as LabDefinition;

function wrapper({ children }: { children: ReactNode }) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

beforeEach(() => {
  mockDatapacks = [];
  mockCreateDatapack.mockReset();
  mockGetDatapackProvisionStatus.mockReset();
  mockDedicatedDeployment = undefined;
  mockSharedDeployment = undefined;
});

afterEach(() => {
  localStorage.clear();
});

describe("useLabRunnerSession", () => {
  it("clears session.error once the datapack that caused it is evicted", async () => {
    mockDatapacks = [{ datapack_id: "dp-1", datapack_name: "DP1", grant: "owner" }];
    mockGetDatapackProvisionStatus.mockRejectedValueOnce(new Error("boom"));
    localStorage.setItem("meko:lab-datapack:test-lab", "dp-1");

    const { result, rerender } = renderHook(() => useLabRunnerSession(FIXTURE_LAB), { wrapper });

    await waitFor(() => expect(result.current.error).toBe("boom"));

    // Access revoked server-side: the next datapack list no longer
    // includes it, so the stale-choice effect evicts the selection.
    mockDatapacks = [];
    act(() => rerender());

    await waitFor(() => expect(result.current.datapackId).toBeNull());
    expect(result.current.error).toBeNull();
  });

  it("doesn't evict a just-created datapack before the list catches up with it", async () => {
    mockCreateDatapack.mockResolvedValue({ datapack_id: "new-dp", name: "New Datapack" });
    mockGetDatapackProvisionStatus.mockResolvedValue({ mcp_api_key: "token" });

    const { result, rerender } = renderHook(() => useLabRunnerSession(FIXTURE_LAB), { wrapper });

    await act(async () => {
      await result.current.createAndSelectDatapack("New Datapack");
    });

    // The list refetch after create hasn't (yet) surfaced the new
    // datapack -- it must not be evicted for that. createAndSelectDatapack
    // doesn't await that refetch (see the regression test below), so this
    // is the normal case, not just a narrow race window.
    expect(mockDatapacks.some((dp) => dp.datapack_id === "new-dp")).toBe(false);
    act(() => rerender());
    expect(result.current.datapackId).toBe("new-dp");

    // Once the list genuinely contains it, the grace is lifted --
    mockDatapacks = [{ datapack_id: "new-dp", datapack_name: "New Datapack", grant: "owner" }];
    act(() => rerender());
    expect(result.current.datapackId).toBe("new-dp");

    // -- so a real later revocation still evicts it like any other choice.
    mockDatapacks = [];
    act(() => rerender());
    await waitFor(() => expect(result.current.datapackId).toBeNull());
  });

  // Instruments `qc` so `invalidateQueries({queryKey: [queryKeyToHang]})`
  // hangs until the returned `resolve()` is called, while every other key
  // invalidates normally -- lets a test prove createAndSelectDatapack
  // doesn't await one specific refetch, without hanging the whole test.
  function hangInvalidationFor(qc: QueryClient, queryKeyToHang: string) {
    let resolve: () => void = () => {};
    const gate = new Promise<void>((r) => {
      resolve = r;
    });
    const realInvalidateQueries = qc.invalidateQueries.bind(qc);
    vi.spyOn(qc, "invalidateQueries").mockImplementation((filters, ...rest) => {
      const key = (filters as { queryKey?: unknown[] } | undefined)?.queryKey;
      if (Array.isArray(key) && key[0] === queryKeyToHang) {
        return gate.then(() => realInvalidateQueries(filters, ...rest));
      }
      return realInvalidateQueries(filters, ...rest);
    });
    return { resolve };
  }

  it("resolves createAndSelectDatapack even if the account-usage refetch hangs", async () => {
    // Regression test: account usage aggregates a known-slow Langfuse query
    // (see langfuse_repo.go:655-657's own TODO on the missing indexes) and
    // has been observed to 504 in production. hasHitDatapackCount is real
    // (gates this same function above, and CreateDatapackStage's create
    // button) but doesn't need to be refreshed *this instant* -- the BE
    // re-enforces the cap on the next attempt regardless -- so datapack
    // creation must never block on this refetch. See the test below for the
    // matching case on the datapacks-list invalidation.
    mockCreateDatapack.mockResolvedValue({ datapack_id: "new-dp", name: "New Datapack" });
    mockGetDatapackProvisionStatus.mockResolvedValue({ mcp_api_key: "token" });

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { resolve: resolveUsageInvalidate } = hangInvalidationFor(qc, "account-usage");

    const { result } = renderHook(() => useLabRunnerSession(FIXTURE_LAB), {
      wrapper: ({ children }) => <QueryClientProvider client={qc}>{children}</QueryClientProvider>,
    });

    let createSettled = false;
    await act(async () => {
      const createPromise = result.current.createAndSelectDatapack("New Datapack").then(() => {
        createSettled = true;
      });
      // createAndSelectDatapack must settle here -- the account-usage
      // invalidation is deliberately still hanging (resolveUsageInvalidate
      // hasn't been called yet).
      await waitFor(() => expect(createSettled).toBe(true));
      resolveUsageInvalidate();
      await createPromise;
    });

    expect(result.current.datapackId).toBe("new-dp");
  });

  it("resolves createAndSelectDatapack even if the datapacks-list refetch hangs", async () => {
    // Same shape as the account-usage test above, for the other
    // invalidation: justCreatedDatapackIdRef sets datapackId optimistically
    // from the create response specifically so a slow, failed, or racing
    // list refetch can't block (or evict) the new selection -- useDatapacks
    // is an active query on this same page too, so awaiting this
    // invalidation would reintroduce the exact bug the account-usage fix
    // addressed, just on the other query.
    mockCreateDatapack.mockResolvedValue({ datapack_id: "new-dp", name: "New Datapack" });
    mockGetDatapackProvisionStatus.mockResolvedValue({ mcp_api_key: "token" });

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { resolve: resolveListInvalidate } = hangInvalidationFor(qc, "datapacks");

    const { result } = renderHook(() => useLabRunnerSession(FIXTURE_LAB), {
      wrapper: ({ children }) => <QueryClientProvider client={qc}>{children}</QueryClientProvider>,
    });

    let createSettled = false;
    await act(async () => {
      const createPromise = result.current.createAndSelectDatapack("New Datapack").then(() => {
        createSettled = true;
      });
      await waitFor(() => expect(createSettled).toBe(true));
      resolveListInvalidate();
      await createPromise;
    });

    expect(result.current.datapackId).toBe("new-dp");
  });

  it("starts with no lesson trace, and recordLessonTrace sets it for X-Ray to read", () => {
    const { result } = renderHook(() => useLabRunnerSession(FIXTURE_LAB), { wrapper });

    expect(result.current.lastLessonTrace).toBeNull();

    act(() =>
      result.current.recordLessonTrace({
        lessonTitle: "Add a memory",
        calls: [{ name: "memory_add", durationMs: 42, status: "ok" }],
      }),
    );

    expect(result.current.lastLessonTrace).toEqual({
      lessonTitle: "Add a memory",
      calls: [{ name: "memory_add", durationMs: 42, status: "ok" }],
    });
  });

  it("defaults to shared mode, and resolves deployment/deploymentActive from the shared status", () => {
    mockSharedDeployment = { id: "shared-1", status: "active" };
    mockDedicatedDeployment = { id: "dedicated-1", status: "active" };
    const { result } = renderHook(() => useLabRunnerSession(FIXTURE_LAB), { wrapper });

    expect(result.current.deploymentMode).toBe("shared");
    // Even though a dedicated deployment is also "active", shared mode
    // reads from the shared status, not the dedicated one.
    expect(result.current.deployment).toEqual({ id: "shared-1", status: "active" });
    expect(result.current.deploymentActive).toBe(true);
  });

  it("selectDeploymentMode('dedicated') switches deployment/deploymentActive to the dedicated status", () => {
    mockSharedDeployment = { id: "shared-1", status: "active" };
    mockDedicatedDeployment = { id: "dedicated-1", status: "deploying" };
    const { result } = renderHook(() => useLabRunnerSession(FIXTURE_LAB), { wrapper });

    act(() => result.current.selectDeploymentMode("dedicated"));

    expect(result.current.deploymentMode).toBe("dedicated");
    expect(result.current.deployment).toEqual({ id: "dedicated-1", status: "deploying" });
    expect(result.current.deploymentActive).toBe(false);
  });

  it("persists the deployment mode choice per lab across remounts", () => {
    const { result, unmount } = renderHook(() => useLabRunnerSession(FIXTURE_LAB), { wrapper });
    act(() => result.current.selectDeploymentMode("dedicated"));
    unmount();

    const { result: result2 } = renderHook(() => useLabRunnerSession(FIXTURE_LAB), { wrapper });
    expect(result2.current.deploymentMode).toBe("dedicated");
  });

  it("clears the persisted AMP cluster/branch once the selected datapack actually changes", () => {
    mockDatapacks = [
      { datapack_id: "dp-1", datapack_name: "DP1", grant: "owner" },
      { datapack_id: "dp-2", datapack_name: "DP2", grant: "owner" },
    ];
    mockGetDatapackProvisionStatus.mockResolvedValue({ mcp_api_key: "token" });
    const { result } = renderHook(() => useLabRunnerSession(FIXTURE_LAB), { wrapper });

    act(() => result.current.selectDatapack("dp-1"));
    act(() => result.current.selectAmpCluster("cluster-1", "branch-1"));
    expect(result.current.ampClusterId).toBe("cluster-1");

    // A cluster id only exists inside the datapack pool it was created in --
    // switching datapacks must drop it, or CreateAmpClusterStage/
    // ReviewDashboardStage would keep treating it as live for a datapack
    // that has no such cluster.
    act(() => result.current.selectDatapack("dp-2"));
    expect(result.current.ampClusterId).toBeNull();
    expect(result.current.ampBranchId).toBeNull();
  });

  it("does not clear a persisted AMP cluster choice on initial mount for the same restored datapack", () => {
    mockDatapacks = [{ datapack_id: "dp-1", datapack_name: "DP1", grant: "owner" }];
    mockGetDatapackProvisionStatus.mockResolvedValue({ mcp_api_key: "token" });
    localStorage.setItem("meko:lab-datapack:test-lab", "dp-1");
    localStorage.setItem("meko:lab-amp-cluster:test-lab", "cluster-1");
    localStorage.setItem("meko:lab-amp-branch:test-lab", "branch-1");

    const { result } = renderHook(() => useLabRunnerSession(FIXTURE_LAB), { wrapper });

    expect(result.current.ampClusterId).toBe("cluster-1");
    expect(result.current.ampBranchId).toBe("branch-1");
  });

  it("clearAmpCluster drops the persisted selection", () => {
    const { result } = renderHook(() => useLabRunnerSession(FIXTURE_LAB), { wrapper });
    act(() => result.current.selectAmpCluster("cluster-1", "branch-1"));
    expect(result.current.ampClusterId).toBe("cluster-1");

    act(() => result.current.clearAmpCluster());
    expect(result.current.ampClusterId).toBeNull();
    expect(result.current.ampBranchId).toBeNull();
  });
});
