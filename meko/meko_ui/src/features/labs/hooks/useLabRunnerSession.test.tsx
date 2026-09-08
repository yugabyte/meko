import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

import type { LabDefinition } from "../types";
import { useLabRunnerSession } from "./useLabRunnerSession";

const mockCreateDatapack = vi.fn();
const mockGetDatapackProvisionStatus = vi.fn();
vi.mock("@/lib/api/mekoApi", () => ({
  createDatapack: (...args: unknown[]) => mockCreateDatapack(...args),
  getDatapackProvisionStatus: (...args: unknown[]) => mockGetDatapackProvisionStatus(...args),
}));

vi.mock("@/hooks/useCurrentUser", () => ({
  useCurrentUser: () => ({ data: { account_id: "user-1", email: "me@example.com", user_type: "free" } }),
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
vi.mock("./useDeployments", () => ({
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
    // datapack -- it must not be evicted for that.
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
});
