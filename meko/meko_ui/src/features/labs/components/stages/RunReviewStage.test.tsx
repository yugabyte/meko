import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDemoAppConfig } from "../../types";
import { RunReviewStage } from "./RunReviewStage";

// Deep coverage of the review flow itself lives in PrReviewDemoApp.test.tsx —
// this test only locks the thin degrade-vs-render switch this stage owns.
vi.mock("../demo-apps/PrReviewDemoApp", () => ({
  PrReviewDemoApp: () => <div data-testid="pr-review-demo-app-mock" />,
}));

const DEMO_APP: LabDemoAppConfig = { kind: "pr-review-chat", agentDisplayName: "AgentK" };

function fakeSession(
  datapackId: string | null,
  deploymentActive = false,
  deploymentStatus: "active" | "failed" | undefined = undefined,
): ReturnType<typeof useLabRunnerSession> {
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
    deploymentId: deploymentActive ? "dep-1" : null,
    selectDeployment: vi.fn(),
    clearDeployment: vi.fn(),
    deploymentMode: "shared" as const,
    selectDeploymentMode: vi.fn(),
    deployment: deploymentStatus
      ? ({ id: "dep-1", status: deploymentStatus } as ReturnType<typeof useLabRunnerSession>["deployment"])
      : null,
    deploymentActive,
    ampClusterId: null,
    ampBranchId: null,
    selectAmpCluster: vi.fn(),
    clearAmpCluster: vi.fn(),
  };
}

afterEach(cleanup);

describe("RunReviewStage", () => {
  it("shows the degrade prompt when no datapack is selected", () => {
    render(
      <RunReviewStage
        demoApp={DEMO_APP}
        session={fakeSession(null)}
        onGoToCreateDatapack={vi.fn()}
        onGoToDeployAgent={vi.fn()}
      />,
    );
    expect(screen.getByText(/choose or create a datapack first/i)).toBeTruthy();
    expect(screen.queryByTestId("pr-review-demo-app-mock")).toBeNull();
  });

  it("shows the deployment-needed prompt when a datapack is set but no deployment is active", () => {
    render(
      <RunReviewStage
        demoApp={DEMO_APP}
        session={fakeSession("dp-1", false)}
        onGoToCreateDatapack={vi.fn()}
        onGoToDeployAgent={vi.fn()}
      />,
    );
    expect(screen.getByText(/deploy your agent first/i)).toBeTruthy();
    expect(screen.queryByTestId("pr-review-demo-app-mock")).toBeNull();
  });

  it("shows a failed-deployment message when the deployment failed", () => {
    render(
      <RunReviewStage
        demoApp={DEMO_APP}
        session={fakeSession("dp-1", false, "failed")}
        onGoToCreateDatapack={vi.fn()}
        onGoToDeployAgent={vi.fn()}
      />,
    );
    expect(screen.getByText(/deployment failed/i)).toBeTruthy();
  });

  it("renders PrReviewDemoApp once a datapack is selected and the deployment is active", () => {
    render(
      <RunReviewStage
        demoApp={DEMO_APP}
        session={fakeSession("dp-1", true)}
        onGoToCreateDatapack={vi.fn()}
        onGoToDeployAgent={vi.fn()}
      />,
    );
    expect(screen.getByTestId("pr-review-demo-app-mock")).toBeTruthy();
  });
});
