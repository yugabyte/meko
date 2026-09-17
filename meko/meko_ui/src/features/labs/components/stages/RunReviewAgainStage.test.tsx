import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDemoAppConfig } from "../../types";
import { DEFAULT_REVIEW_AGENT_CONFIG } from "../demo-apps/ReviewAgentConfigPanel";
import { RunReviewAgainStage } from "./RunReviewAgainStage";

// Deep coverage of the review flow itself lives in PrReviewDemoApp.test.tsx —
// this test only locks the thin degrade-vs-render switch this stage owns,
// same convention as RunReviewStage.test.tsx.
vi.mock("../demo-apps/PrReviewDemoApp", () => ({
  PrReviewDemoApp: () => <div data-testid="pr-review-demo-app-mock" />,
}));

const DEMO_APP: LabDemoAppConfig = { kind: "pr-review-chat", agentDisplayName: "AgentK" };

function fakeSession(
  datapackId: string | null,
  deploymentActive = false,
): ReturnType<typeof useLabRunnerSession> {
  return {
    datapackId,
    deploymentActive,
    deployment: null,
    ampClusterId: null,
    ampBranchId: null,
    selectAmpCluster: vi.fn(),
  } as unknown as ReturnType<typeof useLabRunnerSession>;
}

afterEach(cleanup);

describe("RunReviewAgainStage", () => {
  it("shows the degrade prompt when no datapack is selected", () => {
    render(
      <RunReviewAgainStage
        reviewConfig={DEFAULT_REVIEW_AGENT_CONFIG}
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
      <RunReviewAgainStage
        reviewConfig={DEFAULT_REVIEW_AGENT_CONFIG}
        demoApp={DEMO_APP}
        session={fakeSession("dp-1", false)}
        onGoToCreateDatapack={vi.fn()}
        onGoToDeployAgent={vi.fn()}
      />,
    );
    expect(screen.getByText(/deploy your agent first/i)).toBeTruthy();
  });

  it("renders PrReviewDemoApp once a datapack is selected and the deployment is active", () => {
    render(
      <RunReviewAgainStage
        reviewConfig={DEFAULT_REVIEW_AGENT_CONFIG}
        demoApp={DEMO_APP}
        session={fakeSession("dp-1", true)}
        onGoToCreateDatapack={vi.fn()}
        onGoToDeployAgent={vi.fn()}
      />,
    );
    expect(screen.getByTestId("pr-review-demo-app-mock")).toBeTruthy();
  });
});
