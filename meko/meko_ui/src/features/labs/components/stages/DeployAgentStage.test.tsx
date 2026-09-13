import { afterEach, describe, expect, it, vi } from "vitest";
import { type ReactElement } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDemoAppConfig } from "../../types";
import { DeployAgentStage } from "./DeployAgentStage";

function renderStage(ui: ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

function fakeSession(datapackId: string | null): ReturnType<typeof useLabRunnerSession> {
  return {
    userId: "user-1",
    currentUser: null,
    datapackId,
    datapackName: datapackId ? "test-datapack" : null,
    datapackGrant: null,
    accessibleDatapacks: [],
    datapacksLoading: false,
    selectDatapack: vi.fn(),
    createAndSelectDatapack: vi.fn(),
    creating: false,
    hasHitDatapackCount: false,
    userToken: "mko_tkn_fake",
    loading: false,
    error: null,
    refreshTick: 0,
    bumpRefreshTick: vi.fn(),
    lastLessonTrace: null,
    recordLessonTrace: vi.fn(),
    deploymentId: null,
    selectDeployment: vi.fn(),
    clearDeployment: vi.fn(),
    deploymentMode: "dedicated" as const,
    selectDeploymentMode: vi.fn(),
    deployment: null,
    deploymentActive: false,
    ampClusterId: null,
    ampBranchId: null,
    selectAmpCluster: vi.fn(),
    clearAmpCluster: vi.fn(),
  };
}

const DEPLOYABLE_DEMO_APP: LabDemoAppConfig = {
  kind: "pr-review-chat",
  agentDisplayName: "AgentK",
  deployable: { agentSlug: "adlc-review-agent", sourceRepoUrl: "https://github.com/yugabyte/canned-agents" },
};

afterEach(cleanup);

describe("DeployAgentStage", () => {
  it("shows the degrade prompt when no datapack is selected", () => {
    renderStage(
      <DeployAgentStage demoApp={DEPLOYABLE_DEMO_APP} session={fakeSession(null)} onGoToCreateDatapack={vi.fn()} />,
    );
    expect(screen.getByText(/choose or create a datapack first/i)).toBeTruthy();
  });

  it("renders the deploy panel unconditionally once a datapack is selected — no review needed", () => {
    renderStage(
      <DeployAgentStage demoApp={DEPLOYABLE_DEMO_APP} session={fakeSession("dp-1")} onGoToCreateDatapack={vi.fn()} />,
    );
    expect(screen.getByText(/deploy agentk/i)).toBeTruthy();
    expect(screen.getByText("What context changes")).toBeTruthy();
  });

  it("shows a plain message when the lab has nothing to deploy", () => {
    const noDeployApp: LabDemoAppConfig = { kind: "pr-review-chat", agentDisplayName: "AgentK" };
    renderStage(<DeployAgentStage demoApp={noDeployApp} session={fakeSession("dp-1")} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.getByText(/nothing to deploy/i)).toBeTruthy();
  });

  it("collapses the comparison into a compact strip once scrolled past the threshold, and does NOT auto-revert on a scroll event reporting scrollTop back near 0", () => {
    // Regression test for a real bounce-loop bug: collapsing the comparison
    // removes more height than this stage typically has to scroll, so the
    // browser clamps scrollTop back to 0 right after collapsing. A
    // two-way toggle reading live scrollTop would see that clamp and
    // immediately re-expand, undoing the collapse forever. The fix is a
    // one-way latch — scrolling back to 0 must NOT un-collapse it.
    renderStage(
      <DeployAgentStage demoApp={DEPLOYABLE_DEMO_APP} session={fakeSession("dp-1")} onGoToCreateDatapack={vi.fn()} />,
    );

    const scrollRoot = screen.getByText(/deploy agentk/i).closest("div.overflow-y-auto") as HTMLElement;
    expect(scrollRoot).toBeTruthy();

    Object.defineProperty(scrollRoot, "scrollTop", { value: 100, writable: true });
    fireEvent.scroll(scrollRoot);
    expect(screen.queryByText("What context changes")).toBeNull();
    expect(screen.getByText(/click to show again/i)).toBeTruthy();

    // Simulate the browser clamping scrollTop back to 0 (no more overflow
    // once collapsed) and firing another scroll event for it.
    Object.defineProperty(scrollRoot, "scrollTop", { value: 0, writable: true });
    fireEvent.scroll(scrollRoot);
    expect(screen.queryByText("What context changes")).toBeNull();
    expect(screen.getByText(/click to show again/i)).toBeTruthy();
  });

  it("re-expands the comparison only via an explicit click on the compact strip", () => {
    renderStage(
      <DeployAgentStage demoApp={DEPLOYABLE_DEMO_APP} session={fakeSession("dp-1")} onGoToCreateDatapack={vi.fn()} />,
    );

    const scrollRoot = screen.getByText(/deploy agentk/i).closest("div.overflow-y-auto") as HTMLElement;
    Object.defineProperty(scrollRoot, "scrollTop", { value: 100, writable: true });
    fireEvent.scroll(scrollRoot);

    fireEvent.click(screen.getByRole("button", { name: /click to show again/i }));
    expect(screen.getByText("What context changes")).toBeTruthy();
    expect(screen.queryByText(/click to show again/i)).toBeNull();
  });
});
